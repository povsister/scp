package scp

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var gRand *rand.Rand

func init() {
	gRand = rand.New(rand.NewSource(time.Now().UnixNano()))
}

func fifoSession() (s1, s2 *sessionStream) {
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()
	s1 = &sessionStream{
		In:  w1,
		Out: r2,
	}
	s2 = &sessionStream{
		In:  w2,
		Out: r1,
	}
	return
}

func discardBytes(r io.Reader, n int64) {
	_, _ = io.CopyN(io.Discard, r, n)
}

func TestBufferSendRecv(t *testing.T) {
	s1, s2 := fifoSession()
	defer s1.Close()
	defer s2.Close()

	toSend := "testing str"

	r := strings.NewReader(toSend)
	w := bytes.NewBuffer(make([]byte, 0, r.Size()))
	sj := sendJob{
		Type:        file,
		Size:        r.Size(),
		Reader:      r,
		Destination: "test",
		Perm:        0644,
	}
	rj := receiveJob{
		Type:   file,
		Writer: w,
	}

	go func() {
		discardBytes(s1.Out, statusByteLen)
		handleSend(sj, s1)
	}()
	handleReceive(rj, s2)

	if read := w.String(); read != toSend {
		t.Errorf("want %q, got %q", toSend, w.String())
	}
}

const alphabet = "abcdefghijkmlnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randSuffix(n int) string {
	ret := make([]byte, 0, n)
	for i := 1; i <= n; i++ {
		ret = append(ret, alphabet[gRand.Intn(len(alphabet))])
	}
	return string(ret)
}

func prepareTmpFile(root string) (string, error) {
	tmp := filepath.Join(root, fmt.Sprintf("scp-test-%s", randSuffix(5)))
	fd, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return "", err
	}
	defer fd.Close()

	rs := make([]byte, gRand.Intn(1<<8)+1)
	_, err = rand.Read(rs)
	if err != nil {
		return "", err
	}
	_, err = fd.Write(rs)
	if err != nil {
		return "", err
	}

	err = os.Chtimes(tmp, time.Now(), time.Now().Add(-24*time.Hour))
	if err != nil {
		return "", err
	}

	return tmp, nil
}

func cleanTmpFile(path string) {
	_ = os.Remove(path)
}

func TestFileSendRecv(t *testing.T) {
	s1, s2 := fifoSession()
	defer s1.Close()
	defer s2.Close()

	tmp, err := prepareTmpFile(os.TempDir())
	if err != nil {
		t.Fatalf("failed to prepare tmp file: %v", err)
	}
	defer cleanTmpFile(tmp)

	fd, err := os.Open(tmp)
	if err != nil {
		t.Fatalf("can not open tmp file: %v", err)
	}
	defer fd.Close()
	st, _ := fd.Stat()

	sj := sendJob{
		Type:        file,
		Size:        st.Size(),
		Reader:      fd,
		Destination: st.Name(),
		Perm:        0644,
	}

	var buf bytes.Buffer
	rj := receiveJob{
		Type:   file,
		Writer: &buf,
	}

	go func() {
		discardBytes(s1.Out, 1)
		handleSend(sj, s1)
	}()
	handleReceive(rj, s2)

	fdContent := make([]byte, st.Size())
	_, _ = fd.ReadAt(fdContent, 0)

	if !bytes.Equal(fdContent, buf.Bytes()) {
		t.Errorf("want %q, got %q", string(fdContent), buf.String())
	}
}

func mkTmpDir(root string) (string, error) {
	tmpRoot := filepath.Join(root, fmt.Sprintf("scp-temp-%s", randSuffix(10)))
	err := os.Mkdir(tmpRoot, 0755)
	if err != nil {
		return "", err
	}
	return tmpRoot, nil
}

func prepareTmpDir(root string) (string, error) {
	tmpRoot := filepath.Join(root, fmt.Sprintf("scp-root-%s", randSuffix(10)))
	err := spawnTmpFile(tmpRoot, 5)
	if err != nil {
		return "", err
	}

	return tmpRoot, nil
}

func spawnTmpFile(this string, depth int) error {
	if depth <= 0 {
		return nil
	}
	err := os.Mkdir(this, 0755)
	if err != nil {
		return err
	}
	err = os.Chtimes(this, time.Now(), time.Now().Add(-24*time.Hour))
	if err != nil {
		return err
	}

	for i := 1; i <= depth; i++ {
		_, err = prepareTmpFile(this)
		if err != nil {
			return err
		}
	}

	child := filepath.Join(this, fmt.Sprintf("dir-%s", randSuffix(10)))

	return spawnTmpFile(child, depth-1)
}

func cleanTmpDir(path string) {
	_ = os.RemoveAll(path)
}

func TestDirSendRecv(t *testing.T) {
	s1, s2 := fifoSession()
	defer s1.Close()
	defer s2.Close()

	tmpSend, err := prepareTmpDir(os.TempDir())
	if err != nil {
		t.Fatalf("failed to mk tmp send dir: %v", err)
	}
	defer cleanTmpDir(tmpSend)

	errCh := make(chan error, 1)
	to := &DirTransferOption{
		PreserveProp: true,
	}
	fd, err := os.Open(tmpSend)
	if err != nil {
		t.Fatalf("failed to open tmp root: %v", err)
	}
	_, jobCh := traverse(nil, fd, to, errCh)

	tmpReceive, err := mkTmpDir(os.TempDir())
	if err != nil {
		t.Fatalf("failed to mk tmp recv dir: %v", err)
	}
	defer cleanTmpDir(tmpReceive)
	rj := receiveJob{
		Type:      directory,
		Path:      tmpReceive,
		recursive: true,
	}

	go func() {
		discardBytes(s1.Out, statusByteLen)
		for j := range jobCh {
			handleSend(j, s1)
		}
	}()
	handleReceive(rj, s2)

	if failMsg, err := compareDir(tmpSend, tmpReceive, true); err != nil {
		t.Fatalf("failed to compare dir content: %v", err)
	} else if len(failMsg) > 0 {
		t.Error(failMsg)
	}
}

func compareDir(from, to string, includeStat bool) (string, error) {
	return readDir(from, to, func(original, copied []byte, oStat, cStat os.FileInfo) string {
		if !cStat.IsDir() {
			if !bytes.Equal(original, copied) {
				return "file content mismatch"
			}
		}

		if includeStat {
			// scp protocol does not support UnixNano precision
			// So time.Equal() can not be used
			if !oStat.IsDir() && oStat.ModTime().Unix() != cStat.ModTime().Unix() {
				return "mtime mismatch"
			}
			// mode not available on windows
			if runtime.GOOS != "windows" && oStat.Mode() != cStat.Mode() {
				return "%s mode mismatch"
			}
		}
		return ""
	})
}

func readDir(fromDir, toDir string, fn func(original, copied []byte, oStat, cStat os.FileInfo) string) (string, error) {
	fd, err := os.Open(fromDir)
	if err != nil {
		return "", err
	}
	defer fd.Close()

	ff, err := fd.Readdir(-1)
	if err != nil {
		return "", err
	}

	for _, fi := range ff {
		from := filepath.Join(fromDir, fi.Name())
		to := filepath.Join(toDir, fi.Name())
		original, oStat, err := read(from)
		if err != nil {
			return "", err
		}
		copied, cStat, err := read(to)
		if err != nil {
			return "", err
		}
		var failMsg string
		if fi.IsDir() {
			failMsg = fn(nil, nil, oStat, cStat)
		} else {
			failMsg = fn(original, copied, oStat, cStat)
		}
		if len(failMsg) > 0 {
			return fmt.Sprintf("%s: %q -> %q", failMsg, from, to), nil
		}
		if fi.IsDir() {
			return readDir(from, to, fn)
		}
	}
	return "", nil
}

func read(path string) ([]byte, os.FileInfo, error) {
	fd, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer fd.Close()
	fi, err := fd.Stat()
	if err != nil {
		return nil, nil, err
	}
	if fi.IsDir() {
		return nil, fi, nil
	}
	rd, err := io.ReadAll(fd)
	if err != nil {
		return nil, nil, err
	}
	return rd, fi, nil
}
