package agent

import (
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Resumable move transfers. Every verified stream of a move (database dumps,
// ClickHouse archives, file archives) is served with byte ranges and can be
// received in pieces, so a dropped connection costs only the bytes in
// flight:
//
//   - Download routes answer "Range: bytes=N-" with 206 and the rest of the
//     artifact, and always send Accept-Ranges and the artifact's SHA-256.
//   - Restore routes take an upload id (X-Lidza-Upload), the artifact's total
//     size (X-Lidza-Size) and the offset the body starts at
//     (X-Lidza-Offset). The agent appends the body to a staged file and keeps
//     the rolling SHA-256 state beside it, persisted every 64 MiB and when a
//     request ends, so a later request (also after an agent restart)
//     continues where the last one stopped. Once every byte arrived, the
//     digest of the whole artifact is checked before anything is restored;
//     a mismatch removes the staged upload.
//   - GET /v1/move/uploads/{upload} reports how many bytes are staged (0 for
//     an unknown upload, which is also how the control panel recognizes an
//     agent that resumes); DELETE removes a staged upload.
//
// Requests without an upload id keep the original single-request protocol.

const (
	// MoveUploadHeader names a resumable upload; MoveOffsetHeader is the
	// offset its body starts at and MoveSizeHeader the artifact's size.
	MoveUploadHeader = "X-Lidza-Upload"
	MoveOffsetHeader = "X-Lidza-Offset"
	MoveSizeHeader   = "X-Lidza-Size"
	// MoveVerifiedHeader carries the SHA-256 a restore route verified over
	// the whole artifact before it restored it.
	MoveVerifiedHeader = "X-Lidza-Sha256-Verified"

	moveUploadCheckpoint = 64 << 20
	moveUploadKeep       = 48 * time.Hour
)

var (
	moveUploadPattern = regexp.MustCompile(`^[0-9a-f]{32,64}$`)
	moveUploadLocks   sync.Map // upload id → *sync.Mutex
	// errMoveUploadIncomplete answers a request that ended before the
	// artifact's last byte; the upload continues from the staged offset.
	errMoveUploadIncomplete = errors.New("the upload ended before the artifact's last byte; continue it from the staged offset")
	errMoveUploadChecksum   = errors.New("the artifact does not match its SHA-256 digest; the staged upload was removed and nothing was restored")
)

// MoveUploadState is a staged upload's progress.
type MoveUploadState struct {
	ID       string    `json:"id"`
	Size     int64     `json:"size"`
	SHA256   string    `json:"sha256"`
	Received int64     `json:"received"`
	Complete bool      `json:"complete,omitempty"`
	Updated  time.Time `json:"updated"`
	// Hash is the SHA-256 state after Received bytes (encoding.BinaryMarshaler).
	Hash []byte `json:"hash,omitempty"`
}

func (m *Manager) moveUploadDir() string { return filepath.Join(m.cfg.DataDir, "move-uploads") }

func (m *Manager) moveUploadPaths(id string) (data, state string) {
	dir := m.moveUploadDir()
	return filepath.Join(dir, id+".part"), filepath.Join(dir, id+".json")
}

func (m *Manager) readMoveUpload(id string) (MoveUploadState, bool) {
	_, statePath := m.moveUploadPaths(id)
	raw, err := os.ReadFile(statePath)
	var state MoveUploadState
	if err != nil || json.Unmarshal(raw, &state) != nil || state.ID != id {
		return MoveUploadState{ID: id}, false
	}
	return state, true
}

func (m *Manager) writeMoveUpload(state MoveUploadState) error {
	_, statePath := m.moveUploadPaths(state.ID)
	state.Updated = time.Now().UTC()
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	temporary := statePath + ".partial"
	if err := os.WriteFile(temporary, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, statePath)
}

func (m *Manager) removeMoveUpload(id string) {
	data, state := m.moveUploadPaths(id)
	_ = os.Remove(data)
	_ = os.Remove(state)
}

// pruneMoveUploads removes staged uploads nobody continued for two days.
func (m *Manager) pruneMoveUploads() {
	entries, err := os.ReadDir(m.moveUploadDir())
	if err != nil {
		return
	}
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > moveUploadKeep {
			_ = os.Remove(filepath.Join(m.moveUploadDir(), entry.Name()))
		}
	}
}

func (m *Manager) moveUploadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/move/uploads/{upload}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("upload")
		if !moveUploadPattern.MatchString(id) {
			Fail(w, 400, errors.New("invalid upload"))
			return
		}
		state, _ := m.readMoveUpload(id)
		state.Hash = nil
		w.Header().Set("Cache-Control", "no-store")
		JSON(w, 200, state)
	})
	mux.HandleFunc("DELETE /v1/move/uploads/{upload}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("upload")
		if !moveUploadPattern.MatchString(id) {
			Fail(w, 400, errors.New("invalid upload"))
			return
		}
		lock := moveUploadLock(id)
		if !lock.TryLock() {
			Fail(w, 409, errors.New("the upload is in progress"))
			return
		}
		defer lock.Unlock()
		m.removeMoveUpload(id)
		JSON(w, 200, map[string]string{"status": "removed"})
	})
}

func moveUploadLock(id string) *sync.Mutex {
	lock, _ := moveUploadLocks.LoadOrStore(id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// ---- downloads --------------------------------------------------------------------

// moveRangeStart parses "bytes=N-" for an artifact of size bytes. Only one
// open-ended range is supported; that is what a resumed transfer asks for.
func moveRangeStart(header string, size int64) (int64, bool) {
	spec, ok := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, false
	}
	first, last, ok := strings.Cut(spec, "-")
	if !ok || last != "" {
		return 0, false
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, false
	}
	return start, true
}

// serveMoveArtifact streams a verified artifact (a regular file of size
// bytes with its digest), from the requested offset when the request
// carries a Range header.
func serveMoveArtifact(w http.ResponseWriter, r *http.Request, f *os.File, size int64, digest, contentType string, deadline time.Duration) {
	offset := int64(0)
	if raw := r.Header.Get("Range"); raw != "" {
		start, ok := moveRangeStart(raw, size)
		if !ok {
			w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
			Fail(w, http.StatusRequestedRangeNotSatisfiable, errors.New("the requested range is outside the artifact"))
			return
		}
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			Fail(w, 500, errors.New("the artifact could not be read from the requested offset"))
			return
		}
		offset = start
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(deadline))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set(MoveSHA256Header, digest)
	w.Header().Set("Content-Length", strconv.FormatInt(size-offset, 10))
	status := http.StatusOK
	if offset > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, size-1, size))
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	_, _ = io.Copy(w, io.LimitReader(f, size-offset))
}

// ---- uploads ----------------------------------------------------------------------

// moveStaged is a received artifact, verified against its digest and open
// for reading from its start. done removes it: a resumable upload is kept
// when the restore failed, so a retry restores it again without another
// transfer.
type moveStaged struct {
	File   *os.File
	Path   string
	Size   int64
	SHA256 string
	done   func(restored bool)
}

func (s *moveStaged) Done(restored bool) {
	if s != nil && s.done != nil {
		s.done(restored)
		s.done = nil
	}
}

// moveUploadError is a receive failure with its HTTP status; offset, when
// set, is the staged offset the next request must start at.
type moveUploadError struct {
	status int
	err    error
	offset int64
	staged bool
}

func (e *moveUploadError) Error() string { return e.err.Error() }
func (e *moveUploadError) Unwrap() error { return e.err }

// failMoveUpload answers a receive failure.
func failMoveUpload(w http.ResponseWriter, err error) {
	var upload *moveUploadError
	if errors.As(err, &upload) {
		if upload.staged {
			w.Header().Set(MoveOffsetHeader, strconv.FormatInt(upload.offset, 10))
		}
		Fail(w, upload.status, upload.err)
		return
	}
	Fail(w, 400, err)
}

// receiveMoveUpload stores the request's artifact. dir is where a
// single-request upload is staged (the restore's own directory), limit the
// largest accepted artifact, what names it in messages, and progress (may be
// nil) follows the bytes received. The artifact's digest comes from the
// X-Lidza-Sha256 header.
func (m *Manager) receiveMoveUpload(r *http.Request, dir string, limit int64, what string, progress *ProgressTracker) (*moveStaged, error) {
	expected := strings.ToLower(r.Header.Get(MoveSHA256Header))
	if !moveSHA256Pattern.MatchString(expected) {
		return nil, &moveUploadError{status: 400, err: errors.New("the upload needs its SHA-256 digest")}
	}
	if id := r.Header.Get(MoveUploadHeader); id != "" {
		return m.receiveResumableUpload(r, id, expected, limit, what, progress)
	}
	size := r.ContentLength
	if size <= 0 {
		return nil, &moveUploadError{status: http.StatusLengthRequired, err: errors.New("the upload needs its exact length")}
	}
	if size > limit {
		return nil, &moveUploadError{status: http.StatusRequestEntityTooLarge, err: fmt.Errorf("the %s exceeds the transfer limit", what)}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, &moveUploadError{status: 500, err: errors.New("cannot prepare the upload directory")}
	}
	if err := diskFits(dir, uint64(size), "the "+what); err != nil {
		return nil, &moveUploadError{status: http.StatusInsufficientStorage, err: err}
	}
	file, err := os.CreateTemp(dir, ".move-*")
	if err != nil {
		return nil, &moveUploadError{status: 500, err: errors.New("cannot store the upload")}
	}
	remove := func() { file.Close(); os.Remove(file.Name()) }
	sum := sha256.New()
	received := &progressWriter{}
	progress.Stage("Receiving the "+what, size, received.n.Load)
	n, err := io.Copy(io.MultiWriter(&reserveWriter{dir: dir}, file, sum, received), io.LimitReader(r.Body, size+1))
	if errors.Is(err, errDiskReserve) {
		remove()
		return nil, &moveUploadError{status: http.StatusInsufficientStorage, err: err}
	}
	if err != nil || n != size {
		remove()
		return nil, &moveUploadError{status: 400, err: fmt.Errorf("the upload was incomplete; nothing was restored")}
	}
	if hex.EncodeToString(sum.Sum(nil)) != expected {
		remove()
		return nil, &moveUploadError{status: http.StatusUnprocessableEntity, err: fmt.Errorf("the %s does not match its SHA-256 digest; nothing was restored", what)}
	}
	if err := file.Sync(); err != nil {
		remove()
		return nil, &moveUploadError{status: 500, err: errors.New("the upload could not be stored")}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		remove()
		return nil, &moveUploadError{status: 500, err: err}
	}
	return &moveStaged{File: file, Path: file.Name(), Size: size, SHA256: expected, done: func(bool) { remove() }}, nil
}

func (m *Manager) receiveResumableUpload(r *http.Request, id, expected string, limit int64, what string, progress *ProgressTracker) (*moveStaged, error) {
	if !moveUploadPattern.MatchString(id) {
		return nil, &moveUploadError{status: 400, err: errors.New("invalid upload id")}
	}
	size, err := strconv.ParseInt(r.Header.Get(MoveSizeHeader), 10, 64)
	if err != nil || size <= 0 {
		return nil, &moveUploadError{status: 400, err: errors.New("a resumable upload needs the artifact's size")}
	}
	if size > limit {
		return nil, &moveUploadError{status: http.StatusRequestEntityTooLarge, err: fmt.Errorf("the %s exceeds the transfer limit", what)}
	}
	offset, err := strconv.ParseInt(r.Header.Get(MoveOffsetHeader), 10, 64)
	if err != nil || offset < 0 || offset > size {
		return nil, &moveUploadError{status: 400, err: errors.New("invalid upload offset")}
	}
	if r.ContentLength != size-offset {
		return nil, &moveUploadError{status: 400, err: errors.New("the body must hold the artifact from the offset to its end")}
	}
	lock := moveUploadLock(id)
	if !lock.TryLock() {
		return nil, &moveUploadError{status: 409, err: errors.New("the upload is in progress")}
	}
	unlock := sync.OnceFunc(lock.Unlock)
	fail := func(e *moveUploadError) (*moveStaged, error) { unlock(); return nil, e }
	dir := m.moveUploadDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(&moveUploadError{status: 500, err: errors.New("cannot prepare the upload directory")})
	}
	dataPath, _ := m.moveUploadPaths(id)
	state, found := m.readMoveUpload(id)
	if found && (state.Size != size || state.SHA256 != expected) {
		// Another artifact under the same id: start over.
		m.removeMoveUpload(id)
		state, found = MoveUploadState{ID: id}, false
	}
	if offset == 0 && (!found || !state.Complete) {
		m.pruneMoveUploads()
		m.removeMoveUpload(id)
		state, found = MoveUploadState{ID: id, Size: size, SHA256: expected}, false
		if err := diskFits(dir, uint64(size), "the "+what); err != nil {
			return fail(&moveUploadError{status: http.StatusInsufficientStorage, err: err})
		}
	}
	if offset != state.Received && !(state.Complete && offset == size) {
		return fail(&moveUploadError{status: 409, err: fmt.Errorf("the upload continues at byte %d", state.Received), offset: state.Received, staged: true})
	}
	sum := sha256.New()
	if len(state.Hash) > 0 {
		if err := sum.(encoding.BinaryUnmarshaler).UnmarshalBinary(state.Hash); err != nil {
			m.removeMoveUpload(id)
			return fail(&moveUploadError{status: 409, err: errors.New("the staged upload's digest state is unreadable; start it again"), staged: true})
		}
	}
	file, err := os.OpenFile(dataPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fail(&moveUploadError{status: 500, err: errors.New("cannot store the upload")})
	}
	// Bytes written after the last saved state are not in the digest state:
	// the file always continues from the recorded offset.
	if err := file.Truncate(state.Received); err != nil {
		file.Close()
		return fail(&moveUploadError{status: 500, err: errors.New("the staged upload could not be prepared")})
	}
	if _, err := file.Seek(state.Received, io.SeekStart); err != nil {
		file.Close()
		return fail(&moveUploadError{status: 500, err: err})
	}
	state.Size, state.SHA256 = size, expected
	save := func() error {
		if err := file.Sync(); err != nil {
			return err
		}
		marshaled, err := sum.(encoding.BinaryMarshaler).MarshalBinary()
		if err != nil {
			return err
		}
		state.Hash = marshaled
		return m.writeMoveUpload(state)
	}
	if !state.Complete && offset < size {
		start := state.Received
		received := &progressWriter{}
		progress.Stage("Receiving the "+what, size, func() int64 { return start + received.n.Load() })
		writer := &moveUploadWriter{file: file, hash: sum, state: &state, save: save}
		n, err := io.Copy(io.MultiWriter(&reserveWriter{dir: dir}, writer, received), io.LimitReader(r.Body, size-offset))
		_ = n
		if saveErr := save(); saveErr != nil && err == nil {
			err = saveErr
		}
		if errors.Is(err, errDiskReserve) {
			file.Close()
			return fail(&moveUploadError{status: http.StatusInsufficientStorage, err: err, offset: state.Received, staged: true})
		}
		if err != nil || state.Received != size {
			file.Close()
			return fail(&moveUploadError{status: 400, err: errMoveUploadIncomplete, offset: state.Received, staged: true})
		}
		if hex.EncodeToString(sum.Sum(nil)) != expected {
			file.Close()
			m.removeMoveUpload(id)
			return fail(&moveUploadError{status: http.StatusUnprocessableEntity, err: errMoveUploadChecksum})
		}
		state.Complete = true
		if err := save(); err != nil {
			file.Close()
			return fail(&moveUploadError{status: 500, err: errors.New("the upload could not be stored")})
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return fail(&moveUploadError{status: 500, err: err})
	}
	return &moveStaged{File: file, Path: dataPath, Size: size, SHA256: expected, done: func(restored bool) {
		file.Close()
		if restored {
			m.removeMoveUpload(id)
		}
		unlock()
	}}, nil
}

// moveUploadWriter appends to a staged upload, hashing what it writes and
// saving the digest state every moveUploadCheckpoint bytes.
type moveUploadWriter struct {
	file  *os.File
	hash  hash.Hash
	state *MoveUploadState
	save  func() error
	since int64
}

func (u *moveUploadWriter) Write(p []byte) (int, error) {
	n, err := u.file.Write(p)
	u.hash.Write(p[:n])
	u.state.Received += int64(n)
	u.since += int64(n)
	if err != nil {
		return n, err
	}
	if u.since >= moveUploadCheckpoint {
		u.since = 0
		if err := u.save(); err != nil {
			return n, err
		}
	}
	return n, nil
}
