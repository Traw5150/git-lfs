package tq

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/git-lfs/git-lfs/errors"
	"github.com/git-lfs/git-lfs/lfshttp"
	"github.com/git-lfs/git-lfs/ssh"
	"github.com/git-lfs/git-lfs/tools"
	"github.com/rubyist/tracerx"
)

type SSHBatchClient struct {
	maxRetries int
	transfer   *ssh.SSHTransfer
}

func (a *SSHBatchClient) batchInternal(args []string, batchLines []string) (int, []string, error) {
	conn := a.transfer.Connection(0)
	conn.Lock()
	defer conn.Unlock()
	err := conn.SendMessageWithLines("batch", args, batchLines)
	if err != nil {
		return 0, nil, errors.Wrap(err, "batch request")
	}

	status, _, lines, err := conn.ReadStatusWithLines()
	if err != nil {
		return status, nil, errors.Wrap(err, "batch response")
	}
	return status, lines, err
}

func (a *SSHBatchClient) Batch(remote string, bReq *batchRequest) (*BatchResponse, error) {
	bRes := &BatchResponse{TransferAdapterName: "ssh"}
	if len(bReq.Objects) == 0 {
		return bRes, nil
	}

	missing := make(map[string]bool)
	batchLines := make([]string, 0, len(bReq.Objects))
	for _, obj := range bReq.Objects {
		missing[obj.Oid] = obj.Missing
		batchLines = append(batchLines, fmt.Sprintf("%s %d", obj.Oid, obj.Size))
	}

	tracerx.Printf("api: batch %d files", len(bReq.Objects))

	requestedAt := time.Now()
	args := []string{"transfer=ssh"}
	if bReq.Ref != nil {
		args = append(args, fmt.Sprintf("refname=%s", bReq.Ref.Name))
	}
	status, lines, err := a.batchInternal(args, batchLines)
	if err != nil {
		return nil, err
	}

	if status != 200 {
		msg := "no message provided"
		if len(lines) > 0 {
			msg = lines[0]
		}
		return nil, fmt.Errorf("batch response: status %d from server (%s)", status, msg)
	}

	sort.Strings(lines)
	for _, line := range lines {
		entries := strings.Split(line, " ")
		if len(entries) < 3 {
			return nil, fmt.Errorf("batch response: malformed response: %q", line)
		}
		length := len(bRes.Objects)
		if length == 0 || bRes.Objects[length-1].Oid != entries[0] {
			bRes.Objects = append(bRes.Objects, &Transfer{Actions: make(map[string]*Action)})
		}
		transfer := bRes.Objects[len(bRes.Objects)-1]
		transfer.Oid = entries[0]
		transfer.Size, err = strconv.ParseInt(entries[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("batch response: invalid size: %s", entries[1])
		}
		if entries[2] == "noop" {
			continue
		}
		transfer.Actions[entries[2]] = &Action{}
		if len(entries) > 3 {
			for _, entry := range entries[3:] {
				if strings.HasPrefix(entry, "id=") {
					transfer.Actions[entries[2]].Id = entry[3:]
				} else if strings.HasPrefix(entry, "token=") {
					transfer.Actions[entries[2]].Token = entry[6:]
				} else if strings.HasPrefix(entry, "expires-in=") {
					transfer.Actions[entries[2]].ExpiresIn, err = strconv.Atoi(entry[11:])
					if err != nil {
						return nil, fmt.Errorf("batch response: invalid expires-in: %s", entry)
					}
				} else if strings.HasPrefix(entry, "expires-at=") {
					transfer.Actions[entries[2]].ExpiresAt, err = time.Parse(time.RFC3339, entry[11:])
					if err != nil {
						return nil, fmt.Errorf("batch response: invalid expires-at: %s", entry)
					}
				}
			}
		}
	}

	for _, obj := range bRes.Objects {
		obj.Missing = missing[obj.Oid]
		for _, a := range obj.Actions {
			a.createdAt = requestedAt
		}
	}

	return bRes, nil
}

func (a *SSHBatchClient) MaxRetries() int {
	return a.maxRetries
}

func (a *SSHBatchClient) SetMaxRetries(n int) {
	a.maxRetries = n
}

type SSHAdapter struct {
	*adapterBase
	ctx      lfshttp.Context
	transfer *ssh.SSHTransfer
}

// WorkerStarting is called when a worker goroutine starts to process jobs
// Implementations can run some startup logic here & return some context if needed
func (a *SSHAdapter) WorkerStarting(workerNum int) (interface{}, error) {
	return nil, nil
}

// WorkerEnding is called when a worker goroutine is shutting down
// Implementations can clean up per-worker resources here, context is as returned from WorkerStarting
func (a *SSHAdapter) WorkerEnding(workerNum int, ctx interface{}) {
}

func (a *SSHAdapter) tempDir() string {
	// Shared with the basic download adapter.
	d := filepath.Join(a.fs.LFSStorageDir, "incomplete")
	if err := tools.MkdirAll(d, a.fs); err != nil {
		return os.TempDir()
	}
	return d
}

// DoTransfer performs a single transfer within a worker. ctx is any context returned from WorkerStarting
func (a *SSHAdapter) DoTransfer(ctx interface{}, t *Transfer, cb ProgressCallback, authOkFunc func()) error {
	if authOkFunc != nil {
		authOkFunc()
	}
	if a.adapterBase.direction == Upload {
		return a.upload(t, cb)
	} else {
		return a.download(t, cb)
	}
}

func (a *SSHAdapter) download(t *Transfer, cb ProgressCallback) error {
	rel, err := t.Rel("download")
	if err != nil {
		return err
	}
	if rel == nil {
		return errors.Errorf("No download action for object: %s", t.Oid)
	}
	// Reserve a temporary filename. We need to make sure nobody operates on the file simultaneously with us.
	f, err := tools.TempFile(a.tempDir(), t.Oid, a.fs)
	if err != nil {
		return err
	}
	tmpName := f.Name()
	defer func() {
		if f != nil {
			f.Close()
		}
		os.Remove(tmpName)
	}()

	return a.doDownload(t, f, cb)
}

// doDownload starts a download. f is expected to be an existing file open in RW mode
func (a *SSHAdapter) doDownload(t *Transfer, f *os.File, cb ProgressCallback) error {
	conn := a.transfer.Connection(0)
	args := a.argumentsForTransfer(t, "download")
	conn.Lock()
	defer conn.Unlock()
	err := conn.SendMessage(fmt.Sprintf("get-object %s", t.Oid), args)
	if err != nil {
		return err
	}
	status, args, data, err := conn.ReadStatusWithData()
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		buffer := &bytes.Buffer{}
		if data != nil {
			io.CopyN(buffer, data, 1024)
			io.Copy(ioutil.Discard, data)
		}
		return errors.NewRetriableError(fmt.Errorf("got status %d when fetching OID %s: %s", status, t.Oid, buffer.String()))
	}

	var actualSize int64
	seenSize := false
	for _, arg := range args {
		if strings.HasPrefix(arg, "size=") {
			if seenSize {
				return errors.NewProtocolError("unexpected size argument", nil)
			}
			actualSize, err = strconv.ParseInt(arg[5:], 10, 64)
			if err != nil || actualSize < 0 {
				return errors.NewProtocolError(fmt.Sprintf("expected valid size, got %q", arg[5:]), err)
			}
			seenSize = true
		}
	}
	if !seenSize {
		return errors.NewProtocolError("no size argument seen", nil)
	}

	dlfilename := f.Name()
	// Wrap callback to give name context
	ccb := func(totalSize int64, readSoFar int64, readSinceLast int) error {
		if cb != nil {
			return cb(t.Name, totalSize, readSoFar, readSinceLast)
		}
		return nil
	}
	hasher := tools.NewHashingReader(data)
	written, err := tools.CopyWithCallback(f, hasher, t.Size, ccb)
	if err != nil {
		return errors.Wrapf(err, "cannot write data to tempfile %q", dlfilename)
	}

	if actual := hasher.Hash(); actual != t.Oid {
		return fmt.Errorf("expected OID %s, got %s after %d bytes written", t.Oid, actual, written)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("can't close tempfile %q: %v", dlfilename, err)
	}

	err = tools.RenameFileCopyPermissions(dlfilename, t.Path)
	if _, err2 := os.Stat(t.Path); err2 == nil {
		// Target file already exists, possibly was downloaded by other git-lfs process
		return nil
	}
	return err
}

func (a *SSHAdapter) verifyUpload(t *Transfer) error {
	conn := a.transfer.Connection(0)
	args := a.argumentsForTransfer(t, "upload")
	conn.Lock()
	defer conn.Unlock()
	err := conn.SendMessage(fmt.Sprintf("verify-object %s", t.Oid), args)
	if err != nil {
		return err
	}
	status, _, lines, err := conn.ReadStatusWithLines()
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		if len(lines) > 0 {
			return fmt.Errorf("got status %d when verifying upload OID %s: %s", status, t.Oid, lines[0])
		}
		return fmt.Errorf("got status %d when verifying upload OID %s", status, t.Oid)
	}
	return nil
}

func (a *SSHAdapter) doUpload(t *Transfer, f *os.File, cb ProgressCallback) (int, []string, []string, error) {
	conn := a.transfer.Connection(0)
	args := a.argumentsForTransfer(t, "upload")

	// Ensure progress callbacks made while uploading
	// Wrap callback to give name context
	ccb := func(totalSize int64, readSoFar int64, readSinceLast int) error {
		if cb != nil {
			return cb(t.Name, totalSize, readSoFar, readSinceLast)
		}
		return nil
	}

	cbr := tools.NewFileBodyWithCallback(f, t.Size, ccb)

	conn.Lock()
	defer conn.Unlock()
	defer cbr.Close()
	err := conn.SendMessageWithData(fmt.Sprintf("put-object %s", t.Oid), args, cbr)
	if err != nil {
		return 0, nil, nil, err
	}
	return conn.ReadStatusWithLines()
}

// upload starts an upload.
func (a *SSHAdapter) upload(t *Transfer, cb ProgressCallback) error {
	rel, err := t.Rel("upload")
	if err != nil {
		return err
	}
	if rel == nil {
		return errors.Errorf("No upload action for object: %s", t.Oid)
	}

	f, err := os.OpenFile(t.Path, os.O_RDONLY, 0644)
	if err != nil {
		return errors.Wrap(err, "SSH upload")
	}
	defer f.Close()

	status, _, lines, err := a.doUpload(t, f, cb)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		// A status code of 403 likely means that an authentication token for the
		// upload has expired. This can be safely retried.
		if status == 403 {
			err = errors.New("http: received status 403")
			return errors.NewRetriableError(err)
		}

		if status == 429 {
			return errors.NewRetriableError(fmt.Errorf("got status %d when uploading OID %s", status, t.Oid))
		}

		if len(lines) > 0 {
			return fmt.Errorf("got status %d when uploading OID %s: %s", status, t.Oid, lines[0])
		}
		return fmt.Errorf("got status %d when uploading OID %s", status, t.Oid)

	}

	return a.verifyUpload(t)
}

func (a *SSHAdapter) argumentsForTransfer(t *Transfer, action string) []string {
	args := make([]string, 0, 3)
	set, ok := t.Actions[action]
	if !ok {
		return nil
	}
	args = append(args, fmt.Sprintf("size=%d", t.Size))
	if set.Id != "" {
		args = append(args, fmt.Sprintf("id=%s", set.Id))
	}
	if set.Token != "" {
		args = append(args, fmt.Sprintf("token=%s", set.Token))
	}
	return args
}

// Begin a new batch of uploads or downloads. Call this first, followed by one
// or more Add calls. The passed in callback will receive updates on progress.
func (a *SSHAdapter) Begin(cfg AdapterConfig, cb ProgressCallback) error {
	if err := a.adapterBase.Begin(cfg, cb); err != nil {
		return err
	}
	a.ctx = a.adapterBase.apiClient.Context()
	a.debugging = a.ctx.OSEnv().Bool("GIT_TRANSFER_TRACE", false)
	return nil
}

// ClearTempStorage clears any temporary files, such as unfinished downloads that
// would otherwise be resumed
func (a *SSHAdapter) ClearTempStorage() error {
	return os.RemoveAll(a.tempDir())
}

func (a *SSHAdapter) Trace(format string, args ...interface{}) {
	if !a.adapterBase.debugging {
		return
	}
	tracerx.Printf(format, args...)
}

func configureSSHAdapter(m *Manifest) {
	m.RegisterNewAdapterFunc("ssh", Upload, func(name string, dir Direction) Adapter {
		a := &SSHAdapter{newAdapterBase(m.fs, name, dir, nil), nil, m.sshTransfer}
		a.transferImpl = a
		return a
	})
	m.RegisterNewAdapterFunc("ssh", Download, func(name string, dir Direction) Adapter {
		a := &SSHAdapter{newAdapterBase(m.fs, name, dir, nil), nil, m.sshTransfer}
		a.transferImpl = a
		return a
	})
}
