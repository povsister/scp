package scp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/crypto/ssh"
)

var (
	// The default permission bits for transferred files.
	DefaultFilePerm = os.FileMode(0644)
	// The default permission bits for transferred directories.
	DefaultDirPerm = os.FileMode(0755)

	// Indicate a non-nil TransferOption should be provided
	ErrNoTransferOption = errors.New("TransferOption is not provided")

	// The num of pre-read files/directories for transferring a directory.
	// Do not set it too large or you will exceed the max open files limit.
	DirectoryPreReads = 10
)

// FileTransferOption holds the transfer options for file.
type FileTransferOption struct {
	// Timeout for transferring the file.
	// Default: 0 (Means no timeout)
	Timeout time.Duration
	// The permission bits for transferred file.
	// Override "PreserveProp" if specified.
	// Default: 0644
	Perm os.FileMode
	// Preserve modification times and permission bits from the original file.
	// Only valid for file transfer.
	// Default: false
	PreserveProp bool
	// Limits the used bandwidth, specified in Kbit/s.
	// Default: 0 (Means no limit)
	// TODO: not implemented yet
	SpeedLimit int64
}

// KnownSize is intended for reader whose size is already known before reading.
type KnownSize interface {
	// return num in bytes
	Size() int64
}

// CopyFileToRemote copies a local file to remote location.
func (c *Client) CopyFileToRemote(file string, remoteLoc string, opt *FileTransferOption) error {
	if opt == nil {
		return ErrNoTransferOption
	}

	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("scp: %v", err)
	}

	return c.CopyToRemote(f, remoteLoc, opt)
}

// CopyToRemote copies content from reader to remoteTarget.
// The reader must implement "KnownSize" interface except *os.File.
//
// Currently, it supports following readers:
//   - *os.File
//   - *strings.Reader
//   - *bytes.Reader
// Note that the last part of remoteTarget will be used as filename if unspecified.
//
// It's CALLER'S responsibility to CLOSE the file if an *os.File is supplied.
func (c *Client) CopyToRemote(reader io.Reader, remoteTarget string, opt *FileTransferOption) error {
	if opt == nil {
		return ErrNoTransferOption
	}

	var size int64
	var fileName, remotePath string
	var mtime, atime *time.Time
	var perm os.FileMode = DefaultFilePerm
	if opt.Perm != 0 {
		perm = opt.Perm
	}

	switch r := reader.(type) {
	case *os.File:
		stat, err := r.Stat()
		if err != nil {
			return fmt.Errorf("scp: error getting file stat %v", err)
		}
		size = stat.Size()
		fileName = stat.Name()
		remotePath = remoteTarget
		if opt.PreserveProp {
			mt, at := stat.ModTime(), time.Now()
			mtime, atime = &mt, &at
			if opt.Perm == 0 {
				perm = stat.Mode()
			}
		}
	default:
		if ks, ok := reader.(KnownSize); ok {
			size = ks.Size()
			fileName = filepath.Base(remoteTarget)
			// ToSlash guarantees "coping from Windows to *unix" works as expected
			remotePath = filepath.ToSlash(filepath.Dir(remoteTarget))
		} else {
			return fmt.Errorf("scp: reader does not implement KnownSize interface")
		}
	}

	session, stream, reusableErrCh, err := c.prepareTransfer(false, scpLocalToRemote, remotePath)
	if err != nil {
		return err
	}
	defer session.Close()
	defer stream.Close()

	job := transferJob{
		Type:         file,
		Size:         size,
		Reader:       reader,
		Destination:  fileName,
		Perm:         perm,
		AccessTime:   atime,
		ModifiedTime: mtime,
	}

	finished := make(chan struct{})
	go c.sendToRemote(job, stream, finished, reusableErrCh)

	stopFn, timer := setupTimeout(opt.Timeout)
	select {
	case <-timer:
		return fmt.Errorf("scp: timeout sending file to remote")
	case err = <-reusableErrCh:
		// remote scp server automatically exits on error
		return fmt.Errorf("scp: %v", err)
	case <-finished:
		stopFn()
		c.sendToRemote(exitJob, stream, nil, reusableErrCh)
	}

	return nil
}

// DirTransferOption holds the transfer options for directory.
type DirTransferOption struct {
	// Timeout for transferring the whole directory.
	// Default: 0 (means no timeout)
	Timeout time.Duration
	// Preserve modification times and modes from the original file/directory.
	// Default: false
	PreserveProp bool
	// Limits the used bandwidth, specified in Kbit/s.
	// Default: 0 (Means no limit)
	// TODO: not implemented yet
	SpeedLimit int64
}

// CopyDirToRemote recursively copies a directory to remoteDir.
func (c *Client) CopyDirToRemote(localDir string, remoteDir string, opt *DirTransferOption) error {
	if opt == nil {
		return ErrNoTransferOption
	}

	dir, err := os.Open(localDir)
	if err != nil {
		return fmt.Errorf("scp: error opening local dir: %v", err)
	}

	session, stream, reusableErrCh, err := c.prepareTransfer(true, scpLocalToRemote, remoteDir)
	if err != nil {
		return err
	}
	defer session.Close()
	defer stream.Close()

	jobCh := traverse(dir, opt, reusableErrCh)
	finished := make(chan struct{})
	go c.sendToRemote(jobCh, stream, finished, reusableErrCh)

	stopFn, timer := setupTimeout(opt.Timeout)
	select {
	case <-timer:
		return fmt.Errorf("scp: timeout recursively sending directory to remote")
	case err = <-reusableErrCh:
		return fmt.Errorf("scp: %v", err)
	case <-finished:
		stopFn()
		// don't call exitJob.
		// Because it's generated by traverse automatically.
	}

	return nil
}

// traverse iterates files and directories of fd in specific order.
// Return a chan for jobs.
// The fd will be automatically closed after read.
func traverse(fd *os.File, opt *DirTransferOption, errCh chan error) <-chan transferJob {
	jobCh := make(chan transferJob, DirectoryPreReads)

	go traverseDir(true, fd, opt, jobCh, errCh)

	return jobCh
}

func traverseDir(rootDir bool, dir *os.File, opt *DirTransferOption, jobCh chan transferJob, errCh chan error) {
	curDirStat, err := dir.Stat()
	if err != nil {
		errCh <- fmt.Errorf("error getting dir stat: %v", err)
		return
	}
	deliverDir(curDirStat, opt, jobCh)

	list, err := dir.Readdir(-1)
	_ = dir.Close()
	if err != nil {
		errCh <- fmt.Errorf("error traverse dir: %v", err)
		return
	}

	var subDirs []os.FileInfo
	for i := range list {
		fStat := list[i]
		// transfer files first
		if !fStat.IsDir() {
			fd, err := os.Open(filepath.Join(dir.Name(), fStat.Name()))
			if err != nil {
				errCh <- fmt.Errorf("error opening file: %v", err)
				return
			}
			deliverFile(fd, fStat, opt, jobCh)
		} else {
			subDirs = append(subDirs, fStat)
		}
	}

	// traverse sub dirs
	for i := range subDirs {
		dirStat := subDirs[i]
		fd, err := os.Open(filepath.Join(dir.Name(), dirStat.Name()))
		if err != nil {
			errCh <- fmt.Errorf("error opening sub dir: %v", err)
			return
		}
		// recursively transfer the dirs
		traverseDir(false, fd, opt, jobCh, errCh)
	}

	// exit current directory
	jobCh <- exitJob

	if rootDir {
		close(jobCh)
	}
}

// deliver a directory transfer
func deliverDir(stat os.FileInfo, opt *DirTransferOption, jobCh chan transferJob) {
	j := transferJob{
		Type:        directory,
		Destination: stat.Name(),
		Perm:        DefaultDirPerm,
	}
	if opt.PreserveProp {
		// directory permission bit not available on windows
		if runtime.GOOS != "windows" {
			j.Perm = stat.Mode()
		}
		mt, at := stat.ModTime(), time.Now()
		j.ModifiedTime, j.AccessTime = &mt, &at
	}
	jobCh <- j
}

// deliver a file transfer job.
// close the fd automatically.
func deliverFile(fd *os.File, stat os.FileInfo, opt *DirTransferOption, jobCh chan transferJob) {
	j := transferJob{
		Type:        file,
		Size:        stat.Size(),
		Reader:      fd,
		Destination: stat.Name(),
		Perm:        DefaultFilePerm,
		close:       true,
	}
	if opt.PreserveProp {
		j.Perm = stat.Mode()
		mt, at := stat.ModTime(), time.Now()
		j.ModifiedTime, j.AccessTime = &mt, &at
	}
	jobCh <- j
}

// helper func to setup a timeout timer.
// 0 means no timeout and the chan will block forever.
func setupTimeout(dur time.Duration) (func(), <-chan time.Time) {
	if dur == 0 {
		return func() {}, make(chan time.Time)
	}
	t := time.NewTimer(dur)
	return func() { t.Stop() }, t.C
}

// prepare for the transfer. Including setup session/stream and run remote scp command
func (c *Client) prepareTransfer(recursive bool, mode scpServerMode, remotePath string) (*ssh.Session, *sessionStream, chan error, error) {
	session, stream, err := c.sessionAndStream()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("scp: error creating ssh session %v", err)
	}

	errCh := make(chan error, 3)
	serverReady := make(chan struct{})

	go c.launchScpServerOnRemote(recursive, mode, session, remotePath, serverReady, errCh)

	t := time.NewTimer(10 * time.Second)
	select {
	case <-t.C:
		return nil, nil, nil, fmt.Errorf("scp: timeout starting remote scp server")
	case err = <-errCh:
		return nil, nil, nil, fmt.Errorf("scp: %v", err)
	case <-serverReady:
		t.Stop()
	}

	return session, stream, errCh, nil
}
