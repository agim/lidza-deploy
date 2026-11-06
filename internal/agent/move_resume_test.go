package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestMoveArtifactServesByteRanges(t *testing.T) {
	content := bytes.Repeat([]byte("0123456789"), 100)
	path := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	serve := func(rangeHeader string) *httptest.ResponseRecorder {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		r := httptest.NewRequest("GET", "/", nil)
		if rangeHeader != "" {
			r.Header.Set("Range", rangeHeader)
		}
		w := httptest.NewRecorder()
		serveMoveArtifact(w, r, f, int64(len(content)), digest, "application/octet-stream", 0)
		return w
	}
	full := serve("")
	if full.Code != 200 || !bytes.Equal(full.Body.Bytes(), content) || full.Header().Get("Accept-Ranges") != "bytes" || full.Header().Get(MoveSHA256Header) != digest {
		t.Fatalf("full download: %d %q", full.Code, full.Header())
	}
	part := serve("bytes=400-")
	if part.Code != http.StatusPartialContent || !bytes.Equal(part.Body.Bytes(), content[400:]) || part.Header().Get("Content-Range") != "bytes 400-999/1000" || part.Header().Get("Content-Length") != "600" {
		t.Fatalf("ranged download: %d %q", part.Code, part.Header())
	}
	for _, bad := range []string{"bytes=1000-", "bytes=0-10", "bytes=1-2,4-", "items=3-"} {
		if w := serve(bad); w.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}
}

// failingReader yields n bytes of r and then fails like a dropped connection.
type failingReader struct {
	r io.Reader
	n int
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("connection reset by peer")
	}
	if len(p) > f.n {
		p = p[:f.n]
	}
	n, err := f.r.Read(p)
	f.n -= n
	return n, err
}

func resumableUpload(id string, content []byte, offset int64, body io.Reader, digest string) *http.Request {
	r := httptest.NewRequest("POST", "/", body)
	r.ContentLength = int64(len(content)) - offset
	r.Header.Set(MoveSHA256Header, digest)
	r.Header.Set(MoveUploadHeader, id)
	r.Header.Set(MoveSizeHeader, strconv.Itoa(len(content)))
	r.Header.Set(MoveOffsetHeader, strconv.FormatInt(offset, 10))
	return r
}

func TestMoveUploadResumesFromTheStagedOffset(t *testing.T) {
	dataDir := t.TempDir()
	m := &Manager{cfg: Config{DataDir: dataDir}}
	content := bytes.Repeat([]byte("resumable move artifact "), 4096)
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	id := "0123456789abcdef0123456789abcdef"

	// The first request drops after 40000 bytes.
	_, err := m.receiveMoveUpload(resumableUpload(id, content, 0, &failingReader{r: bytes.NewReader(content), n: 40000}, digest), t.TempDir(), 1<<30, "dump", nil)
	var upload *moveUploadError
	if !errors.As(err, &upload) || !errors.Is(err, errMoveUploadIncomplete) || upload.offset != 40000 {
		t.Fatalf("dropped request: %v", err)
	}
	// A restarted agent reads the staged state from disk.
	restarted := &Manager{cfg: Config{DataDir: dataDir}}
	mux := http.NewServeMux()
	restarted.moveUploadRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/move/uploads/"+id, nil))
	var state MoveUploadState
	if json.Unmarshal(w.Body.Bytes(), &state); w.Code != 200 || state.Received != 40000 || state.Size != int64(len(content)) || state.Hash != nil {
		t.Fatalf("upload state: %d %s", w.Code, w.Body.String())
	}
	// A request at another offset is told where to continue.
	_, err = restarted.receiveMoveUpload(resumableUpload(id, content, 100, bytes.NewReader(content[100:]), digest), t.TempDir(), 1<<30, "dump", nil)
	if !errors.As(err, &upload) || upload.status != 409 || upload.offset != 40000 {
		t.Fatalf("wrong offset: %v", err)
	}
	staged, err := restarted.receiveMoveUpload(resumableUpload(id, content, 40000, bytes.NewReader(content[40000:]), digest), t.TempDir(), 1<<30, "dump", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(staged.File)
	if !bytes.Equal(got, content) || staged.SHA256 != digest {
		t.Fatal("the resumed upload differs from the artifact")
	}
	// A failed restore keeps the complete upload for the next attempt,
	// which needs no body at all.
	staged.Done(false)
	again, err := restarted.receiveMoveUpload(resumableUpload(id, content, int64(len(content)), http.NoBody, digest), t.TempDir(), 1<<30, "dump", nil)
	if err != nil {
		t.Fatal(err)
	}
	again.Done(true)
	if _, found := restarted.readMoveUpload(id); found {
		t.Fatal("a restored upload is removed")
	}
}

func TestMoveUploadRejectsADigestMismatch(t *testing.T) {
	m := &Manager{cfg: Config{DataDir: t.TempDir()}}
	content := bytes.Repeat([]byte("x"), 70000)
	id := "fedcba9876543210fedcba9876543210"
	wrong := hex.EncodeToString(make([]byte, 32))
	_, err := m.receiveMoveUpload(resumableUpload(id, content, 0, bytes.NewReader(content), wrong), t.TempDir(), 1<<30, "dump", nil)
	var upload *moveUploadError
	if !errors.As(err, &upload) || upload.status != http.StatusUnprocessableEntity {
		t.Fatalf("mismatch: %v", err)
	}
	if _, found := m.readMoveUpload(id); found {
		t.Fatal("a mismatching upload is removed")
	}
	// Single-request uploads keep the original protocol.
	r := httptest.NewRequest("POST", "/", bytes.NewReader(content))
	sum := sha256.Sum256(content)
	r.Header.Set(MoveSHA256Header, hex.EncodeToString(sum[:]))
	staged, err := m.receiveMoveUpload(r, t.TempDir(), 1<<30, "dump", nil)
	if err != nil || staged.Size != int64(len(content)) {
		t.Fatalf("single request: %v", err)
	}
	staged.Done(true)
	if _, err := os.Stat(staged.Path); !os.IsNotExist(err) {
		t.Fatal("a single-request upload is removed once used")
	}
}

func TestMoveProbeMeasuresTheDataDisk(t *testing.T) {
	probe, err := moveProbeDisk(t.Context(), t.TempDir(), 2<<20)
	if err != nil || probe.WriteBytes <= 0 || probe.ReadBytes <= 0 || probe.Bytes != 2<<20 {
		t.Fatalf("probe: %+v %v", probe, err)
	}
}
