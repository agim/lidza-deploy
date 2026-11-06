package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agim/lidza/packs/storage"
)

const moveTestMove = "0123456789abcdef01234567"

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func archiveOf(t *testing.T, dir string, preserve bool) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if _, err := moveWriteArchive(context.Background(), dir, preserve, compressionGzip, &buffer, 1<<30); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// craftArchive builds a gzip tar from raw headers, for hostile inputs.
func craftArchive(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var buffer bytes.Buffer
	zw := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(zw)
	for _, h := range entries {
		h := h
		body := []byte(nil)
		if h.Typeflag == tar.TypeReg {
			body = []byte("content")
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
	}
	tw.Close()
	zw.Close()
	return buffer.Bytes()
}

func TestMoveFileArchiveRoundTripKeepsPathsModesAndTimes(t *testing.T) {
	source := t.TempDir()
	writeTree(t, source, map[string]string{"a.txt": "alpha", "nested/deep/b.bin": "bravo", "nested/c.txt": "charlie"})
	if err := os.Chmod(filepath.Join(source, "a.txt"), 0o4755); err != nil { // set-id bits are dropped
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "nested", "c.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("deep/b.bin", filepath.Join(source, "nested", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "empty"), 0o750); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(source, "nested", "c.txt"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	measured, err := moveMeasure(context.Background(), source)
	size := int64(len("alpha") + len("bravo") + len("charlie"))
	if err != nil || measured.Files != 3 || measured.Bytes != size || measured.Skipped != 0 || len(measured.Problems) != 0 {
		t.Fatalf("measure: %+v %v", measured, err)
	}
	archive := archiveOf(t, source, false)
	staging := filepath.Join(t.TempDir(), "staging")
	stats, err := moveExtractArchive(context.Background(), bytes.NewReader(archive), staging, false, moveOwner{}, 0o775, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if stats.files != 3 || stats.bytes != size {
		t.Fatalf("stats %+v", stats)
	}
	for name, body := range map[string]string{"a.txt": "alpha", "nested/deep/b.bin": "bravo", "nested/c.txt": "charlie", "nested/link": "bravo"} {
		got, err := os.ReadFile(filepath.Join(staging, filepath.FromSlash(name)))
		if err != nil || string(got) != body {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
	if info, _ := os.Stat(filepath.Join(staging, "a.txt")); info.Mode().Perm() != 0o755 || info.Mode()&fs.ModeSetuid != 0 {
		t.Fatalf("a.txt mode %v", info.Mode())
	}
	info, _ := os.Stat(filepath.Join(staging, "nested", "c.txt"))
	if info.Mode().Perm() != 0o600 || !info.ModTime().Equal(stamp) {
		t.Fatalf("c.txt mode %v time %v", info.Mode(), info.ModTime())
	}
	if target, _ := os.Readlink(filepath.Join(staging, "nested", "link")); target != "deep/b.bin" {
		t.Fatalf("link target %q", target)
	}
	if info, _ := os.Stat(filepath.Join(staging, "empty")); info == nil || !info.IsDir() || info.Mode().Perm() != 0o750 {
		t.Fatalf("empty directory %v", info)
	}
	if info, _ := os.Stat(staging); info.Mode().Perm() != 0o775 {
		t.Fatalf("root mode %v", info.Mode())
	}
	// Owners are not archived unless asked for.
	zr, _ := gzip.NewReader(bytes.NewReader(archive))
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Fatalf("owner archived for %s: %d:%d", h.Name, h.Uid, h.Gid)
		}
	}
	// A missing set is an empty archive.
	empty := archiveOf(t, filepath.Join(t.TempDir(), "missing"), false)
	stats, err = moveExtractArchive(context.Background(), bytes.NewReader(empty), filepath.Join(t.TempDir(), "staging"), false, moveOwner{}, 0, 1<<30)
	if err != nil || stats.files != 0 {
		t.Fatalf("empty archive: %+v %v", stats, err)
	}
}

func TestMoveFileArchiveRefusesLinksOutsideTheSet(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		setup func(dir string) error
	}{
		{"absolute link", func(dir string) error { return os.Symlink("/etc/passwd", filepath.Join(dir, "passwd")) }},
		{"relative escape", func(dir string) error { return os.Symlink("../../secret", filepath.Join(dir, "up")) }},
		{"escape through a link", func(dir string) error {
			if err := os.Symlink(".", filepath.Join(dir, "self")); err != nil {
				return err
			}
			// Lexically inside, really one level above the set.
			return os.Symlink("self/..", filepath.Join(dir, "parent"))
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "set")
			writeTree(t, dir, map[string]string{"ok.txt": "fine"})
			if err := scenario.setup(dir); err != nil {
				t.Fatal(err)
			}
			measured, err := moveMeasure(context.Background(), dir)
			if err != nil || len(measured.Problems) != 1 || measured.Skipped != 0 {
				t.Fatalf("problems %+v %v", measured, err)
			}
			if _, err := moveWriteArchive(context.Background(), dir, false, compressionGzip, io.Discard, 1<<30); err == nil || !strings.Contains(err.Error(), "cannot be moved") {
				t.Fatalf("archive accepted: %v", err)
			}
		})
	}
	// A set that is itself a link is refused.
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(real, link)
	if _, err := moveWriteArchive(context.Background(), link, false, compressionGzip, io.Discard, 1<<30); err == nil {
		t.Fatal("a linked set directory was archived")
	}
	// The size bound applies while archiving.
	big := t.TempDir()
	writeTree(t, big, map[string]string{"large": strings.Repeat("x", 4096)})
	if _, err := moveWriteArchive(context.Background(), big, false, compressionGzip, io.Discard, 1024); err == nil {
		t.Fatal("an oversized set was archived")
	}
}

func TestMoveFileArchiveSkipsRuntimeSocketsAndPipes(t *testing.T) {
	// A mail queue volume: queued mail next to the daemons' control
	// sockets and pipes, which the service recreates when it starts.
	source := filepath.Join(t.TempDir(), "queue")
	writeTree(t, source, map[string]string{"active/1A2B3C": "queued mail", "defer/.keep": ""})
	for _, name := range []string{"private/anvil", "public/pickup"} {
		path := filepath.Join(source, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
	}
	if err := syscall.Mkfifo(filepath.Join(source, "private", "bounce"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeFiles := []string{"private/anvil", "public/pickup", "private/bounce"}
	measured, err := moveMeasure(context.Background(), source)
	if err != nil || measured.Files != 2 || measured.Skipped != 3 || len(measured.Problems) != 0 {
		t.Fatalf("measure: %+v %v", measured, err)
	}
	var buffer bytes.Buffer
	stats, err := moveWriteArchive(context.Background(), source, true, compressionGzip, &buffer, 1<<30)
	if err != nil || stats.files != 2 || stats.skipped != 3 {
		t.Fatalf("archive: %+v %v", stats, err)
	}
	// The archive has no entry for them.
	zr, err := gzip.NewReader(bytes.NewReader(buffer.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for tr := tar.NewReader(zr); ; {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	for _, name := range runtimeFiles {
		if slices.Contains(names, name) {
			t.Fatalf("%s was archived: %v", name, names)
		}
	}
	staging := filepath.Join(t.TempDir(), "staging")
	extracted, err := moveExtractArchive(context.Background(), bytes.NewReader(buffer.Bytes()), staging, true, moveOwner{}, 0, 1<<30)
	if err != nil || extracted.files != 2 {
		t.Fatalf("extract: %+v %v", extracted, err)
	}
	if got, err := os.ReadFile(filepath.Join(staging, "active", "1A2B3C")); err != nil || string(got) != "queued mail" {
		t.Fatalf("queued mail: %q %v", got, err)
	}
	// Their directories arrive, so the service finds its layout.
	for _, dir := range []string{"private", "public", "defer"} {
		if info, err := os.Stat(filepath.Join(staging, dir)); err != nil || !info.IsDir() {
			t.Fatalf("%s: %v %v", dir, info, err)
		}
	}
	for _, name := range runtimeFiles {
		if _, err := os.Lstat(filepath.Join(staging, filepath.FromSlash(name))); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s was extracted: %v", name, err)
		}
	}
}

func TestMoveFileExtractRefusesTraversalAndEscapingLinks(t *testing.T) {
	now := time.Now()
	reg := func(name string) tar.Header {
		return tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, ModTime: now}
	}
	link := func(name, target string) tar.Header {
		return tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777, ModTime: now}
	}
	for _, scenario := range []struct {
		name    string
		entries []tar.Header
	}{
		{"parent traversal", []tar.Header{reg("../evil")}},
		{"nested traversal", []tar.Header{reg("a/../../evil")}},
		{"absolute path", []tar.Header{reg("/tmp/evil")}},
		{"absolute link", []tar.Header{link("l", "/etc")}},
		{"escaping link", []tar.Header{link("l", "../../outside")}},
		{"entry inside a link", []tar.Header{{Name: "sub/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: now}, link("l", "sub"), reg("l/file")}},
		{"escape through a link", []tar.Header{link("self", "."), link("parent", "self/..")}},
		{"hard link outside", []tar.Header{{Name: "h", Typeflag: tar.TypeLink, Linkname: "../../etc/passwd", ModTime: now}}},
		{"device", []tar.Header{{Name: "dev", Typeflag: tar.TypeChar, Mode: 0o600, ModTime: now}}},
		{"duplicate", []tar.Header{reg("a"), reg("a")}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			base := t.TempDir()
			staging := filepath.Join(base, "set", "staging")
			os.MkdirAll(filepath.Dir(staging), 0o755)
			_, err := moveExtractArchive(context.Background(), bytes.NewReader(craftArchive(t, scenario.entries...)), staging, false, moveOwner{}, 0, 1<<30)
			if err == nil {
				t.Fatal("hostile archive accepted")
			}
			for _, outside := range []string{filepath.Join(base, "evil"), filepath.Join(base, "set", "evil"), "/tmp/evil"} {
				if _, err := os.Lstat(outside); err == nil {
					t.Fatalf("%s was written outside the staging directory", outside)
				}
			}
		})
	}
	// Links that stay inside are kept.
	staging := filepath.Join(t.TempDir(), "staging")
	if _, err := moveExtractArchive(context.Background(), bytes.NewReader(craftArchive(t, reg("data/a"), link("alias", "data/a"), link("dangling", "data/missing"))), staging, false, moveOwner{}, 0, 1<<30); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(staging, "alias")); err != nil || string(body) != "content" {
		t.Fatalf("inside link %q %v", body, err)
	}
}

func TestMoveInstallDirectoryIsIdempotent(t *testing.T) {
	shared := t.TempDir()
	final := filepath.Join(shared, "storage")
	writeTree(t, final, map[string]string{"old.txt": "old"})
	for attempt, body := range []string{"first", "second"} {
		staging := filepath.Join(shared, moveFileIncoming+"storage")
		writeTree(t, staging, map[string]string{"new.txt": body})
		if attempt == 1 {
			// A leftover from an interrupted earlier attempt.
			writeTree(t, filepath.Join(shared, moveFilePrevious+"storage"), map[string]string{"stale": "x"})
		}
		if err := moveInstallDirectory(staging, final, filepath.Join(shared, moveFilePrevious+"storage")); err != nil {
			t.Fatal(err)
		}
		entries, _ := os.ReadDir(shared)
		if len(entries) != 1 {
			t.Fatalf("leftovers after install: %v", entries)
		}
		got, _ := os.ReadFile(filepath.Join(final, "new.txt"))
		if string(got) != body {
			t.Fatalf("attempt %d: %q", attempt, got)
		}
		if _, err := os.Stat(filepath.Join(final, "old.txt")); err == nil {
			t.Fatal("the old tree was kept")
		}
	}
	// A destination directory that is a link is never replaced.
	outside := t.TempDir()
	linked := filepath.Join(shared, "uploads")
	os.Symlink(outside, linked)
	staging := filepath.Join(shared, moveFileIncoming+"uploads")
	writeTree(t, staging, map[string]string{"x": "x"})
	if err := moveInstallDirectory(staging, linked, filepath.Join(shared, moveFilePrevious+"uploads")); err == nil {
		t.Fatal("a linked destination was replaced")
	}
}

// moveFilesFixture is a container source with a storage volume and a
// native destination tree, both on temporary directories.
type moveFilesFixture struct {
	m       *Manager
	volume  string // the storage volume's mount point
	imports []string
	tree    moveNativeTree
}

func newMoveFilesFixture(t *testing.T) *moveFilesFixture {
	t.Helper()
	fx := &moveFilesFixture{m: testManager(t, &fakeRuntime{}), volume: t.TempDir()}
	if err := fx.m.Upsert(testApp("shop")); err != nil {
		t.Fatal(err)
	}
	fx.m.mu.Lock()
	fx.m.data.StorageVolumes["shop"] = "lidza-storage-shop"
	fx.m.mu.Unlock()
	fx.tree = moveNativeTree{Root: t.TempDir(), Dirs: []string{"log", "storage", "tmp", "public/system", "public/uploads"}}
	previousMount, previousImport, previousEnsure, previousNative := moveVolumeMountpoint, moveVolumeImport, moveVolumeEnsure, moveNativeFiles
	moveVolumeMountpoint = func(_ context.Context, volume string) (string, error) {
		if volume != "lidza-storage-shop" {
			t.Errorf("unexpected volume %s", volume)
		}
		return fx.volume, nil
	}
	moveVolumeEnsure = func(context.Context, string, string, string, string) error { return nil }
	moveVolumeImport = func(_ context.Context, volume, subpath, owner string, preserve bool, staging, set string) error {
		fx.imports = append(fx.imports, volume+":"+subpath+":"+owner+":"+set)
		dst := filepath.Join(fx.volume+"-imported", subpath)
		os.RemoveAll(dst)
		os.MkdirAll(filepath.Dir(dst), 0o755)
		return os.Rename(staging, dst)
	}
	moveNativeFiles = func(m *Manager, id string) (moveNativeTree, error) {
		if id != "native" {
			return moveNativeTree{}, os.ErrNotExist
		}
		return fx.tree, nil
	}
	t.Cleanup(func() {
		moveVolumeMountpoint, moveVolumeImport, moveVolumeEnsure, moveNativeFiles = previousMount, previousImport, previousEnsure, previousNative
	})
	return fx
}

func archiveRequest(t *testing.T, h http.Handler, app, set string, upload bool) (int, MoveFileArchive) {
	t.Helper()
	raw, _ := json.Marshal(MoveFileArchiveRequest{Move: moveTestMove, Upload: upload})
	w := moveRequest(t, h, "POST", "/v1/move/apps/"+app+"/files/"+set+"/archive", bytes.NewReader(raw), nil)
	var archive MoveFileArchive
	json.Unmarshal(w.Body.Bytes(), &archive)
	if w.Code != 200 {
		t.Logf("archive %s: %d %s", set, w.Code, w.Body)
	}
	return w.Code, archive
}

func TestMoveFileSetsDirectTransferVerifiesTheDigest(t *testing.T) {
	fx := newMoveFilesFixture(t)
	writeTree(t, fx.volume, map[string]string{"avatars/1.png": "png-bytes", "report.pdf": "pdf"})
	// A service socket in the volume is runtime state: counted, not copied.
	socket, err := net.Listen("unix", filepath.Join(fx.volume, "cache.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	h := Handler(fx.m)

	w := moveRequest(t, h, "GET", "/v1/move/apps/shop/files", nil, nil)
	var files MoveFiles
	if err := json.Unmarshal(w.Body.Bytes(), &files); err != nil || len(files.Sets) != 1 {
		t.Fatalf("inventory %d %s", w.Code, w.Body)
	}
	if set := files.Sets[0]; set.ID != "storage" || set.Kind != MoveFileKindVolume || set.Files != 2 || set.Bytes != 12 || set.Skipped != 1 || len(set.Problems) != 0 || set.Empty || files.Storage {
		t.Fatalf("set %+v", set)
	}
	// The move source carries the inventory.
	w = moveRequest(t, h, "GET", "/v1/move/apps/shop", nil, nil)
	var source MoveSource
	json.Unmarshal(w.Body.Bytes(), &source)
	if source.Files == nil || len(source.Files.Sets) != 1 {
		t.Fatalf("source files %+v", source.Files)
	}

	code, archive := archiveRequest(t, h, "shop", "storage", false)
	if code != 200 || archive.Files != 2 || archive.Skipped != 1 || archive.Empty || archive.Key != "" || !moveSHA256Pattern.MatchString(archive.SHA256) {
		t.Fatalf("archive %d %+v", code, archive)
	}
	w = moveRequest(t, h, "GET", "/v1/move/apps/shop/files/storage/archive?move="+moveTestMove, nil, nil)
	if w.Code != 200 || w.Header().Get(MoveSHA256Header) != archive.SHA256 || int64(w.Body.Len()) != archive.Size {
		t.Fatalf("download %d %v", w.Code, w.Header())
	}
	body := w.Body.Bytes()

	// The destination is a container application too (the same manager).
	corrupt := bytes.Clone(body)
	corrupt[len(corrupt)/2] ^= 0xff
	w = moveRequest(t, h, "POST", "/v1/move/apps/shop/files/storage/restore?move="+moveTestMove, bytes.NewReader(corrupt), map[string]string{MoveSHA256Header: archive.SHA256})
	if w.Code != http.StatusUnprocessableEntity || len(fx.imports) != 0 {
		t.Fatalf("checksum mismatch: %d %s, imports %v", w.Code, w.Body, fx.imports)
	}
	w = moveRequest(t, h, "POST", "/v1/move/apps/shop/files/storage/restore?move="+moveTestMove, bytes.NewReader(body[:len(body)-1]), map[string]string{MoveSHA256Header: archive.SHA256})
	if w.Code == 200 || len(fx.imports) != 0 {
		t.Fatalf("truncated upload: %d", w.Code)
	}
	w = moveRequest(t, h, "POST", "/v1/move/apps/shop/files/storage/restore?move="+moveTestMove, bytes.NewReader(body), map[string]string{MoveSHA256Header: archive.SHA256})
	var restored MoveFileRestore
	json.Unmarshal(w.Body.Bytes(), &restored)
	if w.Code != 200 || restored.Files != 2 || !slices.Equal(fx.imports, []string{"lidza-storage-shop::65532:65532:storage"}) {
		t.Fatalf("restore %d %s, imports %v", w.Code, w.Body, fx.imports)
	}
	if got, _ := os.ReadFile(filepath.Join(fx.volume+"-imported", "avatars", "1.png")); string(got) != "png-bytes" {
		t.Fatalf("imported %q", got)
	}
	if _, err := os.Lstat(filepath.Join(fx.volume+"-imported", "cache.sock")); err == nil {
		t.Fatal("the socket was copied")
	}
	// Cleanup removes the spool.
	if w := moveRequest(t, h, "DELETE", "/v1/move/files/"+moveTestMove, nil, nil); w.Code != 200 {
		t.Fatalf("cleanup %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(fx.m.moveSpool(moveTestMove)); err == nil {
		t.Fatal("the spool remains after cleanup")
	}
	if w := moveRequest(t, h, "GET", "/v1/move/apps/shop/files/storage/archive?move="+moveTestMove, nil, nil); w.Code != 404 {
		t.Fatalf("download after cleanup: %d", w.Code)
	}
}

func TestMoveFileSetEmptyArchiveIsReported(t *testing.T) {
	fx := newMoveFilesFixture(t)
	code, archive := archiveRequest(t, Handler(fx.m), "shop", "storage", true)
	if code != 200 || !archive.Empty || archive.Files != 0 || archive.Key != "" {
		t.Fatalf("empty set: %d %+v", code, archive)
	}
}

func TestMoveFileSetsOfNativeApplications(t *testing.T) {
	fx := newMoveFilesFixture(t)
	fx.m.mu.Lock()
	fx.m.data.NativeApps = map[string]NativeApplication{}
	fx.m.mu.Unlock()
	shared := filepath.Join(fx.tree.Root, "shared")
	writeTree(t, shared, map[string]string{"storage/blob": "blob", "log/production.log": "log", "tmp/cache/x": "x", "public/uploads/u.png": "u"})
	outside := t.TempDir()
	os.MkdirAll(filepath.Join(shared, "public"), 0o755)
	os.Symlink(outside, filepath.Join(shared, "public", "system"))
	files, err := fx.m.moveFileInventory(context.Background(), "native")
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, set := range files.Sets {
		ids = append(ids, set.ID)
	}
	if !slices.Equal(ids, []string{"storage", "public-uploads"}) {
		t.Fatalf("sets %v (logs, caches and tmp are excluded)", ids)
	}
	if len(files.Warnings) != 1 || !strings.Contains(files.Warnings[0], "shared/public/system is a link to "+outside) {
		t.Fatalf("warnings %v", files.Warnings)
	}
	// Restore into the native tree: shared/public/uploads is replaced as a whole.
	writeTree(t, filepath.Join(shared, "public", "uploads"), map[string]string{"stale.png": "old"})
	source := t.TempDir()
	writeTree(t, source, map[string]string{"u.png": "new", "nested/v.png": "v"})
	archive := archiveOf(t, source, false)
	sum := sha256.Sum256(archive)
	h := Handler(fx.m)
	for attempt := 0; attempt < 2; attempt++ { // a retry gives the same result
		w := moveRequest(t, h, "POST", "/v1/move/apps/native/files/public-uploads/restore?move="+moveTestMove, bytes.NewReader(archive), map[string]string{MoveSHA256Header: hex.EncodeToString(sum[:])})
		if w.Code != 200 {
			t.Fatalf("native restore %d %s", w.Code, w.Body)
		}
		if got, _ := os.ReadFile(filepath.Join(shared, "public", "uploads", "nested", "v.png")); string(got) != "v" {
			t.Fatalf("restored %q", got)
		}
		if _, err := os.Stat(filepath.Join(shared, "public", "uploads", "stale.png")); err == nil {
			t.Fatal("files absent on the source remain on the destination")
		}
		entries, _ := os.ReadDir(filepath.Join(shared, "public"))
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".lidza-move-") {
				t.Fatalf("staging leftover %s", entry.Name())
			}
		}
	}
	// Excluded directories and stack volumes have no native place.
	for _, set := range []string{"log", "stack-media"} {
		w := moveRequest(t, h, "POST", "/v1/move/apps/native/files/"+set+"/restore?move="+moveTestMove, bytes.NewReader(archive), map[string]string{MoveSHA256Header: hex.EncodeToString(sum[:])})
		if w.Code != 409 {
			t.Fatalf("%s restore: %d", set, w.Code)
		}
	}
}

// fakeS3 is an in-memory S3-compatible bucket with path-style addressing.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	srv     *httptest.Server
}

func newFakeS3(t *testing.T) *fakeS3 {
	f := &fakeS3{objects: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		key := strings.TrimPrefix(r.URL.Path, "/bucket/")
		switch {
		case r.Method == "GET" && r.URL.Query().Get("list-type") == "2":
			type content struct {
				Key  string `xml:"Key"`
				Size int64  `xml:"Size"`
			}
			var page struct {
				XMLName  xml.Name  `xml:"ListBucketResult"`
				Contents []content `xml:"Contents"`
			}
			for k, v := range f.objects {
				if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
					page.Contents = append(page.Contents, content{k, int64(len(v))})
				}
			}
			xml.NewEncoder(w).Encode(page)
		case r.Method == "PUT":
			body, _ := io.ReadAll(r.Body)
			f.objects[key] = body
		case r.Method == "HEAD" || r.Method == "GET":
			body, ok := f.objects[key]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			if r.Method == "GET" {
				w.Write(body)
			}
		case r.Method == "DELETE":
			delete(f.objects, key)
			w.WriteHeader(204)
		default:
			w.WriteHeader(405)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeS3) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := []string{}
	for k := range f.objects {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func TestMoveFileSetsTransferThroughOffsiteStorage(t *testing.T) {
	fx := newMoveFilesFixture(t)
	s3 := newFakeS3(t)
	fx.m.mu.Lock()
	fx.m.data.BackupStorage = &storage.Config{Provider: "s3", Endpoint: s3.srv.URL, Bucket: "bucket", Region: "us-east-1", Prefix: "workspace", AccessKey: "fixture", SecretKey: "fixture-secret", Timeout: 5 * time.Second}
	fx.m.mu.Unlock()
	writeTree(t, fx.volume, map[string]string{"a.txt": "offsite"})
	h := Handler(fx.m)
	code, archive := archiveRequest(t, h, "shop", "storage", true)
	key := moveFileObjectKey(moveTestMove, "storage")
	if code != 200 || archive.Key != key || archive.UploadError != "" {
		t.Fatalf("upload: %d %+v", code, archive)
	}
	if keys := s3.keys(); !slices.Equal(keys, []string{"workspace/" + key}) {
		t.Fatalf("objects %v", keys)
	}
	fetch := func(in MoveFileFetchRequest) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(in)
		return moveRequest(t, h, "POST", "/v1/move/apps/shop/files/storage/fetch", bytes.NewReader(raw), nil)
	}
	// Only the move's own object may be fetched, and only with its digest.
	if w := fetch(MoveFileFetchRequest{Move: moveTestMove, Key: "databases/shop/x.dump", Size: archive.Size, SHA256: archive.SHA256}); w.Code != 400 {
		t.Fatalf("foreign key: %d", w.Code)
	}
	wrong := sha256.Sum256([]byte("other"))
	if w := fetch(MoveFileFetchRequest{Move: moveTestMove, Key: key, Size: archive.Size, SHA256: hex.EncodeToString(wrong[:])}); w.Code != http.StatusUnprocessableEntity || len(fx.imports) != 0 {
		t.Fatalf("digest mismatch: %d %v", w.Code, fx.imports)
	}
	if w := fetch(MoveFileFetchRequest{Move: moveTestMove, Key: key, Size: archive.Size, SHA256: archive.SHA256}); w.Code != 200 || len(fx.imports) != 1 {
		t.Fatalf("fetch: %d %s", w.Code, w.Body)
	}
	if got, _ := os.ReadFile(filepath.Join(fx.volume+"-imported", "a.txt")); string(got) != "offsite" {
		t.Fatalf("imported %q", got)
	}
	// Cleanup removes the object and the spool.
	if w := moveRequest(t, h, "DELETE", "/v1/move/files/"+moveTestMove, nil, nil); w.Code != 200 {
		t.Fatalf("cleanup %d %s", w.Code, w.Body)
	}
	if keys := s3.keys(); len(keys) != 0 {
		t.Fatalf("objects after cleanup %v", keys)
	}
}

func TestMoveFileEndpointsRequireTheAgentKey(t *testing.T) {
	h := Handler(testManager(t, &fakeRuntime{}))
	for _, route := range []struct{ method, path string }{
		{"GET", "/v1/move/apps/shop/files"},
		{"POST", "/v1/move/apps/shop/files/storage/archive"},
		{"GET", "/v1/move/apps/shop/files/storage/archive?move=" + moveTestMove},
		{"POST", "/v1/move/apps/shop/files/storage/restore?move=" + moveTestMove},
		{"POST", "/v1/move/apps/shop/files/storage/fetch"},
		{"DELETE", "/v1/move/files/" + moveTestMove},
	} {
		req := httptest.NewRequest(route.method, route.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: %d", route.method, route.path, w.Code)
		}
	}
}

// Archives negotiated as Zstandard extract like gzip ones; a destination
// detects the compression from the stream.
func TestMoveFileArchiveWithZstd(t *testing.T) {
	fakeZstd(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "uploads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "uploads", "a.txt"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	stats, err := moveWriteArchive(context.Background(), dir, false, compressionZstd, &buffer, 1<<30)
	if err != nil || stats.files != 1 || !bytes.HasPrefix(buffer.Bytes(), zstdMagic) {
		t.Fatalf("archive %+v %v", stats, err)
	}
	staging := filepath.Join(t.TempDir(), "incoming")
	if _, err := moveExtractArchive(context.Background(), bytes.NewReader(buffer.Bytes()), staging, false, moveOwner{}, 0, 1<<30); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(staging, "uploads", "a.txt")); err != nil || string(got) != "first" {
		t.Fatalf("extracted %q %v", got, err)
	}
	// A plain tar is refused: archives are always compressed.
	buffer.Reset()
	if _, err := moveWriteArchive(context.Background(), dir, false, compressionNone, &buffer, 1<<30); err != nil {
		t.Fatal(err)
	}
	if _, err := moveExtractArchive(context.Background(), bytes.NewReader(buffer.Bytes()), filepath.Join(t.TempDir(), "x"), false, moveOwner{}, 0, 1<<30); err == nil {
		t.Fatal("plain tar extracted")
	}
}
