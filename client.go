package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type Client struct {
	settings Settings
	stopCh   chan bool
	errorCh  chan error
}

func MakeClient(settings Settings) *Client {
	return &Client{settings: settings}
}

func (r *Client) initialServerSync() (err error) {
	progressLn("Initial file sync using rsync at " + r.settings.host + "...")

	args := []string{"-e", "ssh " + strings.Join(sshOptions(r.settings), " ")}
	for dir := range r.settings.excludes {
		args = append(args, "--exclude="+dir)
	}

	err = openOutLogForRead(r.settings.host, true)
	if err != nil {
		return
	}
	if r.settings.sudouser != "" {
		args = append(args, "--rsync-path", "sudo -u "+r.settings.sudouser+" rsync")
	}

	//"--delete-excluded",
	args = append(args, "-a", "--delete", sourceDir+"/", r.settings.host+":"+r.settings.dir+"/")

	command := exec.Command("rsync", args...)

	go killOnStop(command, r.stopCh)
	output, err := command.Output()

	if err != nil {
		escapedArgs := make([]string, len(args))
		for i, arg := range args {
			// since we'll use '' to escape arguments, we need to escape single quotes differently
			arg = strings.Replace(arg, "'", "'\"'\"'", -1)
			escapedArgs[i] = "'" + arg + "'"
		}
		stringCommand := "rsync " + strings.Join(escapedArgs, " ")
		progressLn("Cannot perform initial sync. Please ensure that you can execute the following command:\n", stringCommand)

		if exitErr, ok := err.(*exec.ExitError); ok {
			debugLn("rsync output:\n", string(output), "\nstderr:\n", string(exitErr.Stderr))
		} else {
			// actually, this is mostly impossible to have something different than exec.ExitError here
			debugLn("rsync output:\n", string(output), "\nCannot get stderr!")
		}
		panic("Cannot perform initial sync")
	}
	return
}

func (r *Client) copyUnrealsyncBinaries(unrealsyncBinaryPathForHost string) {
	progressLn("Copying unrealsync binary " + unrealsyncBinaryPathForHost + " to " + r.settings.host)
	args := sshOptions(r.settings)
	destination := r.settings.host + ":" + r.settings.dir + "/.unrealsync/unrealsync"
	args = append(args, unrealsyncBinaryPathForHost, destination)
	execOrPanic("scp", args, r.stopCh)
}

func (r *Client) startServer() {
	r.stopCh = make(chan bool)
	r.errorCh = make(chan error)
	var cmd *exec.Cmd
	var stdin io.WriteCloser
	var stdout io.ReadCloser
	defer func() {
		if err := recover(); err != nil {
			close(r.stopCh)
			trace := make([]byte, 10000)
			runtime.Stack(trace, false)
			progressWithPrefix("ERROR", "Stopped for server ", r.settings.host, ": ", err, "\n")
			debugLn("Trace for ", r.settings.host, ":\n", string(trace))
			if cmd != nil {
				err := cmd.Process.Kill()
				if err != nil {
					progressLn("Could not kill ssh process for " + r.settings.host + ": " + err.Error())
					// no action
				}
				err = cmd.Wait()
				if err != nil {
					// we will have ExitError if we killed process or if it failed to start
					// We can't provide any additional information here if process failed to start
					// since we already linked command's stderr to the os.Stderr and captured command's output
					if _, ok := err.(*exec.ExitError); !ok {
						progressLn("Could not wait ssh process for " + r.settings.host + ":" + err.Error())
					}
				}
			}

			go func() {
				time.Sleep(retryInterval)
				progressLn("Reconnecting to " + r.settings.host)
				r.startServer()
			}()
		}
	}()

	r.initialServerSync()
	ostype, osarch, unrealsyncBinaryPath, unrealsyncVersion := r.createDirectoriesAt()
	progressLn("Discovered ostype:" + ostype + " osarch:" + osarch + " binary:" + unrealsyncBinaryPath + " version:" + unrealsyncVersion + " at " + r.settings.host)
	if r.settings.remoteBinPath != "" {
		unrealsyncBinaryPath = r.settings.remoteBinPath
	} else if unrealsyncBinaryPath == "" || !isCompatibleVersions(unrealsyncVersion, version) {
		unrealsyncBinaryPathForHost := unrealsyncDir + "/unrealsync-" + ostype + "-" + osarch
		if _, err := os.Stat(unrealsyncBinaryPathForHost); os.IsNotExist(err) {
			progressLn(unrealsyncBinaryPathForHost, " doesn't exists. Cannot find compatible unrealsync on remote and local hosts")
			panic("cannot find unrealsync binary for remote host (" + unrealsyncBinaryPathForHost + ")")
		}
		r.copyUnrealsyncBinaries(unrealsyncBinaryPathForHost)
		unrealsyncBinaryPath = r.settings.dir + "/.unrealsync/unrealsync"
	}

	cmd, stdin, stdout = r.launchUnrealsyncAt(unrealsyncBinaryPath)

	stream := make(chan BufBlocker)
	// receive from singlestdinwriter (stream) and send into ssh stdin
	go singleStdinWriter(stream, stdin, r.errorCh, r.stopCh)
	// read log and send into ssh stdin via singlestdinwriter (stream)
	// stops if stopChan closes and closes stream
	go doSendChanges(stream, r)
	// read ssh stdout and send into ssh stdin via singlestdinwriter (stream)
	go pingReplyThread(stdout, r.settings.host, stream, r.errorCh)

	err := <-r.errorCh
	panic(err)
}

func (r *Client) launchUnrealsyncAt(unrealsyncBinaryPath string) (*exec.Cmd, io.WriteCloser, io.ReadCloser) {
	progressLn("Launching unrealsync at " + r.settings.host + "...")

	args := sshOptions(r.settings)
	// TODO: escaping
	flags := "--server --hostname=" + r.settings.host
	if isDebug {
		flags += " --debug"
	}
	for dir := range r.settings.excludes {
		flags += " --exclude " + dir
	}

	unrealsyncLaunchCmd := unrealsyncBinaryPath + " " + flags + " " + r.settings.dir
	if r.settings.sudouser != "" {
		unrealsyncLaunchCmd = "sudo -u " + r.settings.sudouser + " " + unrealsyncLaunchCmd
	}
	args = append(args, r.settings.host, unrealsyncLaunchCmd)

	debugLn("ssh", args)
	cmd := exec.Command("ssh", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fatalLn("Cannot get stdout pipe: ", err.Error())
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		fatalLn("Cannot get stdin pipe: ", err.Error())
	}

	cmd.Stderr = os.Stderr

	if err = cmd.Start(); err != nil {
		panic("Cannot start command ssh " + strings.Join(args, " ") + ": " + err.Error())
	}
	return cmd, stdin, stdout
}

func (r *Client) createDirectoriesAt() (ostype, osarch, unrealsyncBinaryPath, unrealsyncVersion string) {
	progressLn("Creating directories at " + r.settings.host + "...")

	args := sshOptions(r.settings)
	// TODO: escaping
	dir := r.settings.dir + "/.unrealsync"
	args = append(args, r.settings.host, "if [ ! -d "+dir+" ]; then mkdir -m a=rwx -p "+dir+"; fi;"+
		"rm -f "+dir+"/unrealsync &&"+
		"uname && uname -m && if ! which unrealsync 2>/dev/null ; then echo 'no-binary'; echo 'no-version';"+
		"else unrealsync --version 2>/dev/null ; echo 'no-version' ; fi")

	output := execOrPanic("ssh", args, r.stopCh)
	uname := strings.Split(strings.TrimSpace(output), "\n")

	return strings.ToLower(uname[0]), uname[1], uname[2], uname[3]
}

func singleStdinWriter(stream chan BufBlocker, stdin io.WriteCloser, errorCh chan error, stopCh chan bool) {
	var bufBlocker BufBlocker
	for {
		select {
		case bufBlocker = <-stream:
		case <-stopCh:
			break
		}
		_, err := stdin.Write(bufBlocker.buf)
		if err != nil {
			sendErrorNonBlocking(errorCh, err)
			break
		}
		select {
		case bufBlocker.sent <- true:
		case <-stopCh:
			break
		}
	}
}

func pingReplyThread(stdout io.ReadCloser, hostname string, stream chan BufBlocker, errorCh chan error) {
	bufBlocker := BufBlocker{buf: make([]byte, 20), sent: make(chan bool)}
	bufBlocker.buf = []byte(actionPong + fmt.Sprintf("%10d", 0))
	buf := make([]byte, 10)
	for {
		readBytes, err := io.ReadFull(stdout, buf)
		if err != nil {
			sendErrorNonBlocking(errorCh, errors.New("Could not read from server: "+hostname+" err:"+err.Error()))
			break
		}
		actionStr := string(buf)
		debugLn("Read ", readBytes, " from ", hostname, " ", buf)
		if actionStr == actionPing {
			stream <- bufBlocker
			<-bufBlocker.sent
		} else if actionStr == actionStopServer {
			currentProcess, err := os.FindProcess(os.Getpid())
			if err != nil {
				panic("Cannot find current process")
			}
			progressLn("Got StopServer command from the remote client ", hostname)
			currentProcess.Kill()
		}
	}
}

func (r *Client) notifySendQueueSize(sendQueueSize int64) (err error) {
	if r.settings.sendQueueSizeLimit != 0 && sendQueueSize > r.settings.sendQueueSizeLimit {
		err = errors.New("SendQueueSize limit exceeded for " + r.settings.host)
		progressLn(err)
		sendErrorNonBlocking(r.errorCh, err)
	}
	return
}
