package scp

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type sessionStream struct {
	In     io.WriteCloser
	Out    io.Reader
	ErrOut io.Reader
}

func (s *sessionStream) Close() error {
	return s.In.Close()
}

func (c *Client) sessionAndStream() (*ssh.Session, *sessionStream, error) {
	s, err := c.NewSession()
	if err != nil {
		return nil, nil, err
	}
	ss := &sessionStream{}
	for _, f := range []func() error{
		func() (err error) {
			ss.In, err = s.StdinPipe()
			return
		},
		func() (err error) {
			ss.Out, err = s.StdoutPipe()
			return
		},
		func() (err error) {
			ss.ErrOut, err = s.StderrPipe()
			return
		},
	} {
		if err = f(); err != nil {
			return nil, nil, err
		}
	}
	return s, ss, nil
}

// represents the remote scp server working mode
type scpServerMode string

const (
	// Used like "scp user@remote.server:~/something ./"
	scpRemoteToLocal scpServerMode = "f"
	// Used like "scp ./something user@remote.server:~/"
	scpLocalToRemote scpServerMode = "t"
)

func (c *Client) launchScpServerOnRemote(recursive bool, mode scpServerMode, s *ssh.Session, remotePath string, readyCh chan<- struct{}, errCh chan<- error) {
	remoteExec := c.scpOpt.RemoteBinary
	if c.scpOpt.Sudo && !c.isRootUser() {
		remoteExec = fmt.Sprintf("sudo %s", c.scpOpt.RemoteBinary)
	}
	var r string
	if recursive {
		r = "-r"
	}
	cmd := fmt.Sprintf("%s %s -q -%s '%s'", remoteExec, r, mode, remotePath)
	err := s.Start(cmd)
	if err != nil {
		errCh <- fmt.Errorf("error executing command %q on remote: %s", cmd, err)
		return
	}
	close(readyCh)
	err = s.Wait()
	if err != nil {
		errCh <- fmt.Errorf("unexpected remote scp server failure: %s", err)
		return
	}
}

type transferType string

const (
	// indicate a file transfer
	file transferType = "file"
	// indicate a directory transfer
	directory transferType = "directory"
	// exit the scp server (at the root directory)
	// or back to the previous directory (equals to "cd ..")
	exit transferType = "exit"
)

type transferJob struct {
	Type         transferType
	Size         int64
	Reader       io.Reader // the content reader
	Destination  string    // must be file or directory name. Path is not supported
	Perm         os.FileMode
	AccessTime   *time.Time // can be nil
	ModifiedTime *time.Time // must be both set or nil with atime
	close        bool       // close the reader after using it. internal usage.
}

var (
	// represent a "E" signal
	exitJob = transferJob{Type: exit}
)

// it accepts a single "transferJob" or "<-chan transferJob"
func (c *Client) sendToRemote(jobs interface{}, stream *sessionStream, finished chan<- struct{}, errCh chan<- error) {
	defer func() {
		if r := recover(); r != nil {
			errCh <- fmt.Errorf("%v", r)
		}
	}()

	switch reflect.ValueOf(jobs).Kind() {
	case reflect.Struct:
		j := jobs.(transferJob)
		handleTransferJob(&j, stream)
	case reflect.Chan:
		jobCh := jobs.(<-chan transferJob)
		for {
			j, ok := <-jobCh
			if !ok {
				// jobCh closed
				break
			}
			handleTransferJob(&j, stream)
		}
	default:
		panicf("programmer error: unknown type %T", jobs)
	}

	if finished != nil {
		close(finished)
	}
}

func handleTransferJob(j *transferJob, stream *sessionStream) {
	switch j.Type {
	case file:
		// close if required
		if j.close {
			if rc, ok := j.Reader.(io.ReadCloser); ok {
				defer rc.Close()
			}
		}
		// set timestamp for the next coming file
		if j.AccessTime != nil && j.ModifiedTime != nil {
			sendTimestamp(j, stream)
		}
		// send signal
		_, err := fmt.Fprintf(stream.In, "C0%o %d %s\n", j.Perm, j.Size, j.Destination)
		if err != nil {
			panicf("error sending signal C: %s", err)
		}
		checkResponse(stream)
		// send file
		_, err = io.Copy(stream.In, j.Reader)
		if err != nil {
			panicf("error sending file %q: %s", j.Destination, err)
		}
		_, err = fmt.Fprint(stream.In, "\x00")
		if err != nil {
			panicf("error finishing file %q: %s", j.Destination, err)
		}
		checkResponse(stream)

	case directory:
		if j.AccessTime != nil && j.ModifiedTime != nil {
			sendTimestamp(j, stream)
		}
		// size is always 0 for directory
		_, err := fmt.Fprintf(stream.In, "D0%o 0 %s\n", j.Perm, j.Destination)
		if err != nil {
			panicf("error sending signal D: %s", err)
		}
		checkResponse(stream)

	case exit:
		// size is always 0 for directory
		_, err := fmt.Fprintf(stream.In, "E\n")
		if err != nil {
			panicf("error sending signal E: %s", err)
		}
		checkResponse(stream)
	default:
		panicf("programmer error: unknown transferType %q", j.Type)
	}
}

func sendTimestamp(j *transferJob, stream *sessionStream) {
	_, err := fmt.Fprintf(stream.In, "T%d 0 %d 0\n", j.ModifiedTime.Unix(), j.AccessTime.Unix())
	if err != nil {
		panicf("error sending signal T: %s", err)
	}
	checkResponse(stream)
}

type responseStatus uint8

const (
	// There are 3 types of responses that the remote can send back:
	// OK, Error and Fatal
	//
	// The difference between Error and Fatal is that the connection is not closed by the remote.
	// However, a Error can indicate a file transfer failure (such as invalid destination directory)
	//
	// All responses except for the OK always have a message (although they can be empty)

	// Normal OK
	statusOK responseStatus = 0
	// A failure operation
	statusErr responseStatus = 1
	// Indicate a Fatal error, though no one actually use it.
	statusFatal responseStatus = 2

	// The byte length for representing a status
	statusByteLen = 1
)

// check response from remote scp
// panic on error
func checkResponse(stream *sessionStream) {
	status := make([]byte, statusByteLen)
	n, err := stream.Out.Read(status)
	if err != nil {
		panicf("error reading server response status: %s", err)
	}
	if n != statusByteLen {
		panicf("expecting to read %d byte, but got %d", statusByteLen, n)
	}

	st := responseStatus(status[0])
	switch st {
	case statusErr, statusFatal:
		buf := bufio.NewReader(stream.Out)
		errMsg, err := buf.ReadString('\n')
		if err != nil {
			panicf("error reading server response message: %s", err)
		}
		panic(strings.Trim(strings.TrimPrefix(errMsg, "scp:"), " "))
	case statusOK:
		// status OK, do nothing
	default:
		panicf("unknown server response status %d", st)
	}
}

func panicf(format string, a ...interface{}) {
	panic(fmt.Sprintf(format, a...))
}
