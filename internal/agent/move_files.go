package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/agim/lidza/packs/storage"
)

// Persistent files for application moves. An application's files are
// grouped in sets: the shared directories of a native release tree
// (shared/storage, shared/public/uploads, ...) and the Docker volumes a
// container application keeps its files in (persistent storage, stack
// volumes). Database and cache volumes are not file sets; databases move
// as dumps and caches start empty.
//
// The source archives one set into its spool as a gzip-compressed tar with
// relative paths, modes and modification times but no owners, computing
// the SHA-256 while it writes. Symbolic links that point outside the set
// are refused. Sockets, named pipes and device nodes are runtime state the
// services recreate when they start (a mail queue's control sockets, a
// filter's socket): they are skipped and counted, never a blocker. The
// archive reaches the destination
// through workspace off-site storage (a move-scoped object) or as a
// verified stream through the control panel. The destination extracts it
// into a staging directory, validates every entry again, gives the files
// to the application's account and replaces the set's directory with one
// rename, so a retry starts over cleanly.
//
//   - GET    /v1/move/apps/{id}/files                     the application's file sets
//   - POST   /v1/move/apps/{id}/files/{set}/archive       archive one set (source)
//   - GET    /v1/move/apps/{id}/files/{set}/archive?move= download the archive (source)
//   - POST   /v1/move/apps/{id}/files/{set}/restore?move= restore an uploaded archive (destination)
//   - POST   /v1/move/apps/{id}/files/{set}/fetch         restore from off-site storage (destination)
//   - DELETE /v1/move/files/{move}                        remove a move's spool and off-site objects

const (
	MoveFileKindDirectory = "directory"
	MoveFileKindVolume    = "volume"

	moveFileSpoolDir   = "move-files"
	moveFileIncoming   = ".lidza-move-incoming-"
	moveFilePrevious   = ".lidza-move-previous-"
	moveFileSpoolKeep  = 7 * 24 * time.Hour
	moveFileMaxProblem = 8
)

// MoveFileSet is one set of persistent files.
type MoveFileSet struct {
	ID   string `json:"id"`   // storage, public-uploads, stack-media
	Path string `json:"path"` // storage, public/uploads, stack/media
	Kind string `json:"kind"` // directory or volume
	// Source says where the files are, for people.
	Source string `json:"source"`
	Files  int64  `json:"files"` // regular files; -1 when not measured
	Bytes  int64  `json:"bytes"` // -1 when not measured
	Empty  bool   `json:"empty"`
	// PreserveOwners keeps numeric owners: stack volumes belong to the
	// users of their service images, not to the application account.
	PreserveOwners bool `json:"preserve_owners,omitempty"`
	// Skipped counts runtime files (sockets, named pipes, device nodes)
	// that are left behind; the service recreates them on the destination.
	Skipped int64 `json:"skipped,omitempty"`
	// Problems are links that cannot be moved.
	Problems []string `json:"problems,omitempty"`
}

// MoveFiles is an application's file inventory.
type MoveFiles struct {
	Sets []MoveFileSet `json:"sets"`
	// Warnings name files that are genuinely not transferable and why.
	Warnings []string `json:"warnings"`
	// Storage reports off-site storage on this agent, so archives can be
	// transferred through it.
	Storage bool `json:"storage"`
}

type MoveFileArchiveRequest struct {
	Move   string `json:"move"`
	Upload bool   `json:"upload,omitempty"`
	// Compression "zstd" asks for a Zstandard archive (both agents have the
	// tool); otherwise, or without the tool here, the archive is gzip.
	Compression string `json:"compression,omitempty"`
}

// MoveFileArchive is one archived set in the source's spool.
type MoveFileArchive struct {
	Set    string `json:"set"`
	Files  int64  `json:"files"`
	Bytes  int64  `json:"bytes"`  // uncompressed file bytes
	Size   int64  `json:"size"`   // archive length
	SHA256 string `json:"sha256"` // of the archive
	Empty  bool   `json:"empty"`
	// Skipped counts the runtime files (sockets, pipes, devices) the
	// archive left behind.
	Skipped     int64  `json:"skipped,omitempty"`
	Compression string `json:"compression,omitempty"`
	Key         string `json:"key,omitempty"` // off-site object, when uploaded
	UploadError string `json:"upload_error,omitempty"`
}

type MoveFileFetchRequest struct {
	Move   string `json:"move"`
	Key    string `json:"key"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// MoveFileRestore reports a restored set.
type MoveFileRestore struct {
	Set   string `json:"set"`
	Files int64  `json:"files"`
	Bytes int64  `json:"bytes"`
	Path  string `json:"path"`
}

var (
	// moveMaxFileBytes bounds the uncompressed bytes of one set and
	// moveMaxFileEntries its number of entries.
	moveMaxFileBytes   int64 = 256 << 30
	moveMaxFileEntries int64 = 5_000_000
	moveMeasureTimeout       = 30 * time.Second

	moveIDPattern      = regexp.MustCompile(`^[a-z0-9]{8,64}$`)
	moveFileSetPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

	// Shared directories that hold caches, logs, sockets or installed
	// dependencies: rebuilt on the destination, never moved.
	moveFileExcluded = []string{"log", "logs", "tmp", "cache", "caches", "bundle", ".bundle", "node_modules", "pids", "sockets", "redis"}
)

// Hooks replaced in tests.
var (
	// moveVolumeMountpoint is where a Docker volume's files are on this
	// host. The agent reads them there; it writes volumes only through a
	// helper container.
	moveVolumeMountpoint = func(ctx context.Context, volume string) (string, error) {
		out, err := command(ctx, "", nil, "docker", "volume", "inspect", "--format", "{{.Mountpoint}}", volume)
		if err != nil {
			return "", errors.New("the volume could not be inspected")
		}
		out = strings.TrimSpace(out)
		if !filepath.IsAbs(out) {
			return "", errors.New("the volume has no local mount point")
		}
		return out, nil
	}
	// moveVolumeEnsure creates a destination volume the way deployments do.
	moveVolumeEnsure = func(ctx context.Context, kind, resource, volume, user string) error {
		if kind == "stack" {
			return ensureStackResource(ctx, "volume", volume, resource)
		}
		return prepareStorageVolumeAs(ctx, resource, volume, user)
	}
	// moveVolumeImport replaces the files at subpath of a volume with the
	// validated tree in staging.
	moveVolumeImport = dockerVolumeImport
	// moveNativeFiles locates a native application's release tree.
	moveNativeFiles = func(m *Manager, id string) (moveNativeTree, error) { return m.nativeFileTree(id) }
)

// moveNativeTree is a native application's release tree and account.
type moveNativeTree struct {
	Root string
	Dirs []string // shared directories of the release profile
	UID  int
	GID  int
}

func (m *Manager) nativeFileTree(id string) (moveNativeTree, error) {
	_, policy, err := m.nativeReleasePolicy(id)
	if err != nil {
		return moveNativeTree{}, err
	}
	account, err := m.nativeReleaseAccount(policy)
	if err != nil {
		return moveNativeTree{}, err
	}
	profile, err := m.nativeReleaseProfile(policy, m.nativeReleaseLayout(account, policy))
	if err != nil {
		return moveNativeTree{}, err
	}
	return moveNativeTree{Root: policy.Root, Dirs: slices.Clone(profile.Shared.Dirs), UID: account.UID, GID: account.GID}, nil
}

// moveFileLocation is where one set lives on this host.
type moveFileLocation struct {
	Set MoveFileSet
	// Native sets: the application root and the directory under shared/.
	Root   string
	Shared string
	// Volume sets: the volume, the subdirectory inside it and the owner
	// of new files ("uid:gid"), or the stack resource for stack volumes.
	Volume   string
	Subpath  string
	Owner    string
	Resource string
	Stack    bool
	UID, GID int
}

// dir is the set's directory for reading.
func (l moveFileLocation) dir(ctx context.Context) (string, error) {
	if l.Volume == "" {
		return l.Shared, nil
	}
	mount, err := moveVolumeMountpoint(ctx, l.Volume)
	if err != nil {
		return "", err
	}
	if l.Subpath == "" {
		return mount, nil
	}
	return filepath.Join(mount, l.Subpath), nil
}

func moveFileSetID(p string) string {
	return strings.ReplaceAll(p, "/", "-")
}

func moveFileExcludedPath(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if slices.Contains(moveFileExcluded, part) {
			return true
		}
	}
	return p == "vendor/bundle" || strings.HasPrefix(p, "vendor/bundle/")
}

// moveCleanRelative accepts a clean relative slash path inside a tree.
func moveCleanRelative(p string) (string, bool) {
	if p == "" || strings.ContainsAny(p, "\x00\\") || path.IsAbs(p) || len(p) > 4096 {
		return "", false
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Count(clean, "/") > 64 {
		return "", false
	}
	return clean, true
}

// moveFileLocations lists the sets of an application on this host. For a
// destination, sets that have no counterpart are absent.
func (m *Manager) moveFileLocations(id string) ([]moveFileLocation, []string, error) {
	if !idPattern.MatchString(id) {
		return nil, nil, errors.New("invalid application id")
	}
	m.mu.Lock()
	a, container := m.data.Apps[id]
	storageVolume := m.data.StorageVolumes[id]
	stackVolumes := slices.Clone(m.data.StackVolumes[id])
	m.mu.Unlock()
	warnings := []string{}
	if container {
		if a.Retiring {
			return nil, nil, errors.New("application unavailable")
		}
		out := []moveFileLocation{}
		rid := a.resourceID()
		if storageVolume != "" {
			if a.isRails() {
				for _, entry := range []struct{ path, sub string }{{"storage", "storage"}, {"public/system", "public-system"}, {"public/uploads", "public-uploads"}} {
					out = append(out, moveFileLocation{Set: MoveFileSet{ID: moveFileSetID(entry.path), Path: entry.path, Kind: MoveFileKindVolume, Source: "volume " + storageVolume + " (" + entry.sub + ")"},
						Volume: storageVolume, Subpath: entry.sub, Owner: a.runtimeUser(), Resource: rid})
				}
			} else {
				out = append(out, moveFileLocation{Set: MoveFileSet{ID: "storage", Path: "storage", Kind: MoveFileKindVolume, Source: "volume " + storageVolume},
					Volume: storageVolume, Owner: a.runtimeUser(), Resource: rid})
			}
		}
		prefix := "lidza-stack-" + rid + "-"
		for _, volume := range stackVolumes {
			name := strings.TrimPrefix(volume, prefix)
			if name == volume || !moveFileSetPattern.MatchString("stack-"+name) {
				warnings = append(warnings, "Stack volume "+volume+" does not belong to this application's stack; its files are not moved.")
				continue
			}
			out = append(out, moveFileLocation{Set: MoveFileSet{ID: "stack-" + name, Path: "stack/" + name, Kind: MoveFileKindVolume, Source: "stack volume " + volume, PreserveOwners: true},
				Volume: volume, Resource: rid, Stack: true})
		}
		return out, warnings, nil
	}
	tree, err := moveNativeFiles(m, id)
	if err != nil {
		return nil, nil, errors.New("application unavailable")
	}
	shared := filepath.Join(tree.Root, "shared")
	out := []moveFileLocation{}
	seen := map[string]bool{}
	for _, raw := range tree.Dirs {
		p, ok := moveCleanRelative(filepath.ToSlash(raw))
		if !ok || moveFileExcludedPath(p) || seen[p] {
			continue
		}
		seen[p] = true
		set := MoveFileSet{ID: moveFileSetID(p), Path: p, Kind: MoveFileKindDirectory, Source: "shared/" + p}
		if !moveFileSetPattern.MatchString(set.ID) {
			warnings = append(warnings, "shared/"+p+" has a name that cannot be moved; copy it manually.")
			continue
		}
		out = append(out, moveFileLocation{Set: set, Root: tree.Root, Shared: filepath.Join(shared, filepath.FromSlash(p)), UID: tree.UID, GID: tree.GID})
	}
	return out, warnings, nil
}

// moveFileInventory measures every set within a time limit; sets that
// cannot be measured in time report -1.
func (m *Manager) moveFileInventory(ctx context.Context, id string) (*MoveFiles, error) {
	locations, warnings, err := m.moveFileLocations(id)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	out := &MoveFiles{Sets: []MoveFileSet{}, Warnings: warnings, Storage: m.data.BackupStorage != nil}
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, moveMeasureTimeout)
	defer cancel()
	for _, l := range locations {
		set := l.Set
		if l.Volume == "" {
			if why := moveSharedProblem(l.Root, l.Set.Path); why != "" {
				out.Warnings = append(out.Warnings, "shared/"+l.Set.Path+" is "+why+"; its files are not moved. Copy them to the destination manually.")
				continue
			}
		}
		dir, err := l.dir(ctx)
		var measured moveMeasurement
		if err == nil {
			measured, err = moveMeasure(ctx, dir)
		}
		if err != nil {
			measured = moveMeasurement{Files: -1, Bytes: -1}
		}
		set.Files, set.Bytes, set.Skipped, set.Problems = measured.Files, measured.Bytes, measured.Skipped, measured.Problems
		set.Empty = set.Files == 0 && len(set.Problems) == 0
		out.Sets = append(out.Sets, set)
	}
	return out, nil
}

// moveSharedProblem reports a shared directory that is a link or lies
// behind one: its files live outside the application's tree.
func moveSharedProblem(root, rel string) string {
	current := filepath.Join(root, "shared")
	parts := append([]string{""}, strings.Split(rel, "/")...)
	for i, part := range parts {
		if part != "" {
			current = filepath.Join(current, part)
		}
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return ""
		}
		if err != nil {
			return "unreadable"
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, _ := os.Readlink(current)
			return "a link to " + target + " outside the application directory"
		}
		if !info.IsDir() && i == len(parts)-1 {
			return "not a directory"
		}
	}
	return ""
}

// moveInstallDirectory replaces final with staging: the old tree is moved
// aside, staging takes its name with one rename, and the old tree is
// removed. A link at final is refused.
func moveInstallDirectory(staging, final, previous string) error {
	if err := os.RemoveAll(previous); err != nil {
		return err
	}
	info, err := os.Lstat(final)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case info.Mode()&fs.ModeSymlink != 0:
		return errors.New("the destination directory is a link; its files were not replaced")
	case !info.IsDir():
		return errors.New("the destination path is not a directory")
	default:
		if err := os.Rename(final, previous); err != nil {
			return err
		}
	}
	if err := os.Rename(staging, final); err != nil {
		if info != nil {
			_ = os.Rename(previous, final)
		}
		return err
	}
	return os.RemoveAll(previous)
}

// dockerVolumeImport streams the validated tree into the volume through a
// helper container that sees only the volume. The tree is extracted next
// to its final place inside the volume and swapped in; owners are set to
// owner, or kept with preserveOwners.
func dockerVolumeImport(ctx context.Context, volume, subpath, owner string, preserveOwners bool, staging, setID string) error {
	if !moveFileSetPattern.MatchString(setID) || strings.ContainsAny(subpath, "/\\.\"'$` ") || owner != "" && !regexp.MustCompile(`^[0-9]{1,10}:[0-9]{1,10}$`).MatchString(owner) {
		return errors.New("invalid volume import")
	}
	info, err := os.Stat(staging)
	if err != nil {
		return err
	}
	rootOwner := owner
	if st, ok := info.Sys().(*syscall.Stat_t); ok && (preserveOwners || owner == "") {
		rootOwner = fmt.Sprintf("%d:%d", st.Uid, st.Gid)
	}
	rootMode := fmt.Sprintf("%o", info.Mode().Perm())
	stage, old := "/data/"+moveFileIncoming+setID, "/data/"+moveFilePrevious+setID
	script := `set -eu
rm -rf "$STAGE" "$OLD"
mkdir "$STAGE"
tar -x -f - -C "$STAGE"
`
	if !preserveOwners && owner != "" {
		script += `chown -R "$OWNER" "$STAGE"
`
	}
	if subpath != "" {
		script += `chown "$ROOT_OWNER" "$STAGE"; chmod "$ROOT_MODE" "$STAGE"
dst="/data/$SUB"
if [ -e "$dst" ] || [ -L "$dst" ]; then mv "$dst" "$OLD"; fi
mv "$STAGE" "$dst"
rm -rf "$OLD"
`
	} else {
		script += `mkdir "$OLD"
for f in /data/* /data/.[!.]* /data/..?*; do
  [ -e "$f" ] || [ -L "$f" ] || continue
  case "${f##*/}" in .lidza-move-incoming-*|.lidza-move-previous-*) continue;; esac
  mv "$f" "$OLD/"
done
for f in "$STAGE"/* "$STAGE"/.[!.]* "$STAGE"/..?*; do
  [ -e "$f" ] || [ -L "$f" ] || continue
  mv "$f" /data/
done
chown "$ROOT_OWNER" /data; chmod "$ROOT_MODE" /data
rm -rf "$STAGE" "$OLD"
`
	}
	reader, writer := io.Pipe()
	go func() {
		_, err := moveWriteArchive(ctx, staging, true, compressionNone, writer, moveMaxFileBytes)
		writer.CloseWithError(err)
	}()
	defer reader.Close()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "--network", "none", "--user", "0:0", "--read-only", "--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "FOWNER", "--cap-add", "DAC_OVERRIDE",
		"--security-opt", "no-new-privileges", "--pids-limit", "64",
		"--env", "STAGE="+stage, "--env", "OLD="+old, "--env", "SUB="+subpath, "--env", "OWNER="+owner, "--env", "ROOT_OWNER="+rootOwner, "--env", "ROOT_MODE="+rootMode,
		"--mount", "type=volume,src="+volume+",dst=/data", cacheImage, "sh", "-c", script)
	cmd.Stdin = reader
	var output limitedBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.WaitDelay = 5 * time.Second
	for _, name := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT"} {
		if v := os.Getenv(name); v != "" {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("the files could not be written into the destination volume")
	}
	return nil
}

// ---- spool ----------------------------------------------------------------------

type moveSpoolManifest struct {
	Set    string `json:"set"`
	Files  int64  `json:"files"`
	Bytes  int64  `json:"bytes"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Key    string `json:"key,omitempty"`
}

func (m *Manager) moveSpool(move string) string {
	return filepath.Join(m.cfg.DataDir, moveFileSpoolDir, move)
}

// moveFileObjectKey is the off-site object of one archived set.
func moveFileObjectKey(move, set string) string {
	return "moves/" + move + "/files/" + set + ".tar.gz"
}

// pruneMoveSpools removes spools of moves that ended long ago without a
// cleanup, for example when the control panel was replaced.
func (m *Manager) pruneMoveSpools() {
	base := filepath.Join(m.cfg.DataDir, moveFileSpoolDir)
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && entry.IsDir() && time.Since(info.ModTime()) > moveFileSpoolKeep {
			_ = os.RemoveAll(filepath.Join(base, entry.Name()))
		}
	}
}

func (m *Manager) beginMoveFiles(id string) error {
	if m.upgradePending() {
		return errors.New("agent upgrade in progress")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.moveFileOps[id] {
		return errors.New("a file transfer of this application is already running")
	}
	if m.busy(id) || m.nativeOperations[id] {
		return errors.New("wait for the application's active operation")
	}
	if m.moveFileOps == nil {
		m.moveFileOps = map[string]bool{}
	}
	m.moveFileOps[id] = true
	return nil
}

func (m *Manager) endMoveFiles(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.moveFileOps, id)
}

func (m *Manager) moveFileLocation(id, set string) (moveFileLocation, error) {
	locations, _, err := m.moveFileLocations(id)
	if err != nil {
		return moveFileLocation{}, err
	}
	for _, l := range locations {
		if l.Set.ID == set {
			return l, nil
		}
	}
	return moveFileLocation{}, errors.New("unknown file set")
}

// ---- routes ---------------------------------------------------------------------

func (m *Manager) moveFileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/move/apps/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		files, err := m.moveFileInventory(r.Context(), r.PathValue("id"))
		if err != nil {
			Fail(w, 404, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		JSON(w, 200, files)
	})
	mux.HandleFunc("POST /v1/move/apps/{id}/files/{set}/archive", m.moveTransfer(m.moveFileArchiveRoute))
	mux.HandleFunc("GET /v1/move/apps/{id}/files/{set}/archive", m.moveTransfer(m.moveFileDownloadRoute))
	mux.HandleFunc("POST /v1/move/apps/{id}/files/{set}/restore", m.moveTransfer(m.moveFileRestoreRoute))
	mux.HandleFunc("POST /v1/move/apps/{id}/files/{set}/fetch", m.moveTransfer(m.moveFileFetchRoute))
	mux.HandleFunc("GET /v1/move/files/{move}/progress", m.moveFileProgressRoute)
	mux.HandleFunc("DELETE /v1/move/files/{move}", func(w http.ResponseWriter, r *http.Request) {
		move := r.PathValue("move")
		if !moveIDPattern.MatchString(move) {
			Fail(w, 400, errors.New("invalid move"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		if err := m.cleanupMoveFiles(ctx, move); err != nil {
			Fail(w, 502, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "removed"})
	})
}

// moveFileProgressKey names the live progress of one set of a move.
func moveFileProgressKey(move, set string) string { return "move/" + move + "/" + set }

// MoveFileProgress is the live progress of a move's file sets on this
// agent: archiving and uploading on the source, downloading and extracting
// on the destination. Sets without a running operation are absent.
type MoveFileProgress struct {
	Sets map[string]Progress `json:"sets"`
}

func (m *Manager) moveFileProgressRoute(w http.ResponseWriter, r *http.Request) {
	move := r.PathValue("move")
	if !moveIDPattern.MatchString(move) {
		Fail(w, 400, errors.New("invalid move"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	JSON(w, 200, MoveFileProgress{Sets: m.progress.prefixed("move/" + move + "/")})
}

// trackMoveFiles starts the live progress of one set and returns ctx
// carrying it with a function that ends it.
func (m *Manager) trackMoveFiles(ctx context.Context, move, set, phase string) (context.Context, func()) {
	t, done := m.progress.track(moveFileProgressKey(move, set), phase)
	return withProgress(ctx, t), done
}

func (m *Manager) moveFileArchiveRoute(w http.ResponseWriter, r *http.Request) {
	id, set := r.PathValue("id"), r.PathValue("set")
	var in MoveFileArchiveRequest
	if err := Decode(w, r, &in); err != nil || !moveIDPattern.MatchString(in.Move) || !moveFileSetPattern.MatchString(set) {
		Fail(w, 400, errors.New("invalid archive request"))
		return
	}
	location, err := m.moveFileLocation(id, set)
	if err != nil {
		Fail(w, 404, err)
		return
	}
	if err := m.beginMoveFiles(id); err != nil {
		Fail(w, 409, err)
		return
	}
	defer m.endMoveFiles(id)
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(6 * time.Hour))
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Hour)
	defer cancel()
	ctx, endProgress := m.trackMoveFiles(ctx, in.Move, set, "Measuring files")
	defer endProgress()
	m.pruneMoveSpools()
	compression := compressionGzip
	if in.Compression == compressionZstd && zstdAvailable() {
		compression = compressionZstd
	}
	archive, err := m.moveArchiveSet(ctx, in.Move, location, compression)
	if err != nil {
		Fail(w, 409, err)
		return
	}
	if in.Upload && !archive.Empty {
		m.mu.Lock()
		cfg := m.data.BackupStorage
		m.mu.Unlock()
		if cfg == nil {
			archive.UploadError = "off-site storage is not configured on the source"
		} else {
			key := moveFileObjectKey(in.Move, set)
			if err := uploadBackup(ctx, *cfg, filepath.Join(m.moveSpool(in.Move), set+".tar.gz"), key, archive.Size); err != nil {
				archive.UploadError = err.Error()
			} else {
				archive.Key = key
				_ = m.writeMoveManifest(in.Move, moveSpoolManifest{Set: set, Files: archive.Files, Bytes: archive.Bytes, Size: archive.Size, SHA256: archive.SHA256, Key: key})
			}
		}
	}
	JSON(w, 200, archive)
}

func (m *Manager) writeMoveManifest(move string, manifest moveSpoolManifest) error {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	file := filepath.Join(m.moveSpool(move), manifest.Set+".json")
	temporary := file + ".partial"
	if err := os.WriteFile(temporary, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, file)
}

func (m *Manager) readMoveManifest(move, set string) (moveSpoolManifest, error) {
	var manifest moveSpoolManifest
	raw, err := os.ReadFile(filepath.Join(m.moveSpool(move), set+".json"))
	if err != nil {
		return manifest, err
	}
	err = json.Unmarshal(raw, &manifest)
	return manifest, err
}

// moveArchiveSet writes one set into the move's spool.
func (m *Manager) moveArchiveSet(ctx context.Context, move string, location moveFileLocation, compression string) (MoveFileArchive, error) {
	out := MoveFileArchive{Set: location.Set.ID}
	if location.Volume == "" {
		if why := moveSharedProblem(location.Root, location.Set.Path); why != "" {
			return out, errors.New("shared/" + location.Set.Path + " is " + why)
		}
	}
	dir, err := location.dir(ctx)
	if err != nil {
		return out, err
	}
	spool := m.moveSpool(move)
	if err := os.MkdirAll(spool, 0o700); err != nil {
		return out, errors.New("cannot prepare the spool directory")
	}
	var measured int64
	if size, err := moveMeasure(ctx, dir); err == nil {
		if err := diskFits(spool, uint64(max(size.Bytes, 0)), "the file archive on the source"); err != nil {
			return out, err
		}
		measured = size.Bytes
	}
	final := filepath.Join(spool, location.Set.ID+".tar.gz")
	file, err := os.CreateTemp(spool, ".archive-*")
	if err != nil {
		return out, errors.New("cannot create the archive")
	}
	defer os.Remove(file.Name())
	defer file.Close()
	hash := sha256.New()
	counter := &moveCountingWriter{}
	// Progress counts the files' bytes read into the archive, against the
	// measured total.
	read := &atomic.Int64{}
	progressFrom(ctx).Stage("Archiving files", measured, read.Load)
	stats, err := moveWriteArchive(withMoveArchiveCounter(ctx, read), dir, location.Set.PreserveOwners, compression, io.MultiWriter(&reserveWriter{dir: spool}, file, hash, counter), moveMaxFileBytes)
	if err != nil {
		return out, err
	}
	if err := file.Sync(); err != nil {
		return out, err
	}
	if err := file.Close(); err != nil {
		return out, err
	}
	if err := os.Rename(file.Name(), final); err != nil {
		return out, err
	}
	out.Compression = compression
	out.Files, out.Bytes, out.Size, out.SHA256, out.Skipped, out.Empty = stats.files, stats.bytes, counter.n, hex.EncodeToString(hash.Sum(nil)), stats.skipped, stats.files == 0 && stats.entries <= 1
	if err := m.writeMoveManifest(move, moveSpoolManifest{Set: out.Set, Files: out.Files, Bytes: out.Bytes, Size: out.Size, SHA256: out.SHA256}); err != nil {
		return out, err
	}
	return out, nil
}

type moveCountingWriter struct{ n int64 }

func (c *moveCountingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

func (m *Manager) moveFileDownloadRoute(w http.ResponseWriter, r *http.Request) {
	set, move := r.PathValue("set"), r.URL.Query().Get("move")
	if !moveIDPattern.MatchString(move) || !moveFileSetPattern.MatchString(set) {
		Fail(w, 400, errors.New("invalid download"))
		return
	}
	if _, err := m.moveFileLocation(r.PathValue("id"), set); err != nil {
		Fail(w, 404, err)
		return
	}
	manifest, err := m.readMoveManifest(move, set)
	if err != nil || !moveSHA256Pattern.MatchString(manifest.SHA256) || manifest.Size <= 0 {
		Fail(w, 404, errors.New("the set has not been archived for this move"))
		return
	}
	f, err := os.Open(filepath.Join(m.moveSpool(move), set+".tar.gz"))
	if err != nil {
		Fail(w, 404, errors.New("archive unavailable"))
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != manifest.Size {
		Fail(w, 409, errors.New("the archive does not match its record"))
		return
	}
	serveMoveArtifact(w, r, f, manifest.Size, manifest.SHA256, "application/gzip", 6*time.Hour)
}

// moveReceive stores an incoming archive in the spool and verifies its
// length and SHA-256 before anything is extracted.
func (m *Manager) moveReceive(ctx context.Context, move, set string, body io.Reader, size int64, expected string) (string, error) {
	spool := m.moveSpool(move)
	if err := os.MkdirAll(spool, 0o700); err != nil {
		return "", errors.New("cannot prepare the spool directory")
	}
	if err := diskFits(spool, uint64(size), "the file archive on the destination"); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(spool, ".incoming-"+set+"-*")
	if err != nil {
		return "", errors.New("cannot store the archive")
	}
	hash := sha256.New()
	received := &progressWriter{}
	progressFrom(ctx).Stage("Receiving the archive", size, received.n.Load)
	n, err := io.Copy(io.MultiWriter(&reserveWriter{dir: spool}, file, hash, received), io.LimitReader(body, size+1))
	closeErr := file.Close()
	if err != nil || closeErr != nil || n != size {
		os.Remove(file.Name())
		return "", errors.New("the archive was incomplete; nothing was restored")
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		os.Remove(file.Name())
		return "", errMoveFileChecksum
	}
	return file.Name(), nil
}

var errMoveFileChecksum = errors.New("the archive does not match its SHA-256 digest; nothing was restored")

func (m *Manager) moveFileRestoreRoute(w http.ResponseWriter, r *http.Request) {
	id, set, move := r.PathValue("id"), r.PathValue("set"), r.URL.Query().Get("move")
	if !moveIDPattern.MatchString(move) || !moveFileSetPattern.MatchString(set) {
		Fail(w, 400, errors.New("the upload needs its move, set and SHA-256 digest"))
		return
	}
	if err := m.beginMoveFiles(id); err != nil {
		Fail(w, 409, err)
		return
	}
	defer m.endMoveFiles(id)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(6 * time.Hour))
	_ = controller.SetWriteDeadline(time.Now().Add(6*time.Hour + time.Minute))
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Hour)
	defer cancel()
	ctx, endProgress := m.trackMoveFiles(ctx, move, set, "Receiving the archive")
	defer endProgress()
	staged, err := m.receiveMoveUpload(r, m.moveSpool(move), moveMaxFileBytes, "archive", progressFrom(ctx))
	if err != nil {
		failMoveUpload(w, err)
		return
	}
	staged.File.Close()
	restored, err := m.moveRestoreSet(ctx, id, move, set, staged.Path)
	staged.Done(err == nil)
	if err != nil {
		Fail(w, 409, err)
		return
	}
	w.Header().Set(MoveVerifiedHeader, staged.SHA256)
	JSON(w, 200, restored)
}

func (m *Manager) moveFileFetchRoute(w http.ResponseWriter, r *http.Request) {
	id, set := r.PathValue("id"), r.PathValue("set")
	var in MoveFileFetchRequest
	if err := Decode(w, r, &in); err != nil || !moveIDPattern.MatchString(in.Move) || !moveFileSetPattern.MatchString(set) || !moveSHA256Pattern.MatchString(in.SHA256) || in.Size <= 0 {
		Fail(w, 400, errors.New("invalid fetch request"))
		return
	}
	if in.Key != moveFileObjectKey(in.Move, set) {
		Fail(w, 400, errors.New("the object does not belong to this move"))
		return
	}
	if in.Size > moveMaxFileBytes {
		Fail(w, http.StatusRequestEntityTooLarge, errors.New("the archive exceeds the transfer limit"))
		return
	}
	m.mu.Lock()
	cfg := m.data.BackupStorage
	m.mu.Unlock()
	if cfg == nil {
		Fail(w, 409, errors.New("off-site storage is not configured on the destination"))
		return
	}
	if err := m.beginMoveFiles(id); err != nil {
		Fail(w, 409, err)
		return
	}
	defer m.endMoveFiles(id)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(6*time.Hour + time.Minute))
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Hour)
	defer cancel()
	ctx, endProgress := m.trackMoveFiles(ctx, in.Move, set, "Downloading the archive from workspace storage")
	defer endProgress()
	store, err := storage.New(*cfg)
	if err != nil {
		Fail(w, 409, errors.New("off-site storage is invalid on the destination"))
		return
	}
	stream, object, err := store.Get(ctx, in.Key)
	if err != nil {
		Fail(w, 502, errors.New("the archive could not be downloaded from off-site storage"))
		return
	}
	defer stream.Close()
	if object.Size > 0 && object.Size != in.Size {
		Fail(w, http.StatusUnprocessableEntity, errors.New("the off-site archive does not have the expected length; nothing was restored"))
		return
	}
	archive, err := m.moveReceive(ctx, in.Move, set, stream, in.Size, in.SHA256)
	if err != nil {
		code := 502
		if errors.Is(err, errMoveFileChecksum) {
			code = http.StatusUnprocessableEntity
		}
		Fail(w, code, err)
		return
	}
	defer os.Remove(archive)
	restored, err := m.moveRestoreSet(ctx, id, in.Move, set, archive)
	if err != nil {
		Fail(w, 409, err)
		return
	}
	JSON(w, 200, restored)
}

// moveRestoreSet extracts a verified archive into the set's place on this
// host. Sets are matched by path, so a container set lands in the
// matching native shared directory and the other way round.
func (m *Manager) moveRestoreSet(ctx context.Context, id, move, set, archive string) (MoveFileRestore, error) {
	out := MoveFileRestore{Set: set}
	target, err := m.moveRestoreTarget(ctx, id, set)
	if err != nil {
		return out, err
	}
	file, err := os.Open(archive)
	if err != nil {
		return out, err
	}
	defer file.Close()
	var size int64
	if info, err := file.Stat(); err == nil {
		size = info.Size()
	}
	f := &progressReader{r: file}
	progressFrom(ctx).Stage("Extracting files", size, f.n.Load)
	if target.Volume == "" {
		final := target.Shared
		parentDir := filepath.Dir(final)
		staging := filepath.Join(parentDir, moveFileIncoming+set)
		previous := filepath.Join(parentDir, moveFilePrevious+set)
		if err := os.RemoveAll(staging); err != nil {
			return out, err
		}
		stats, err := moveExtractArchive(ctx, f, staging, false, moveOwner{UID: target.UID, GID: target.GID}, 0o775, moveMaxFileBytes)
		if err != nil {
			_ = os.RemoveAll(staging)
			return out, err
		}
		if err := moveInstallDirectory(staging, final, previous); err != nil {
			_ = os.RemoveAll(staging)
			return out, err
		}
		out.Files, out.Bytes, out.Path = stats.files, stats.bytes, "shared/"+target.Set.Path
		return out, nil
	}
	staging := filepath.Join(m.moveSpool(move), set+".staging")
	if err := os.RemoveAll(staging); err != nil {
		return out, err
	}
	defer os.RemoveAll(staging)
	var owner moveOwner
	if !target.Set.PreserveOwners && target.Owner != "" {
		parts := strings.SplitN(target.Owner, ":", 2)
		owner.UID, _ = strconv.Atoi(parts[0])
		owner.GID, _ = strconv.Atoi(parts[1])
	}
	var rootMode fs.FileMode
	if !target.Set.PreserveOwners {
		rootMode = 0o700
	}
	stats, err := moveExtractArchive(ctx, f, staging, target.Set.PreserveOwners, owner, rootMode, moveMaxFileBytes)
	if err != nil {
		return out, err
	}
	progressFrom(ctx).Stage("Copying files into the volume", 0, nil)
	if err := moveVolumeImport(ctx, target.Volume, target.Subpath, target.Owner, target.Set.PreserveOwners, staging, set); err != nil {
		return out, err
	}
	out.Files, out.Bytes, out.Path = stats.files, stats.bytes, target.Set.Source
	return out, nil
}

// moveRestoreTarget is where a set goes on this destination. Container
// volumes are created and recorded the way a deployment would.
func (m *Manager) moveRestoreTarget(ctx context.Context, id, set string) (moveFileLocation, error) {
	m.mu.Lock()
	a, container := m.data.Apps[id]
	m.mu.Unlock()
	if !container {
		tree, err := moveNativeFiles(m, id)
		if err != nil {
			return moveFileLocation{}, errors.New("application unavailable")
		}
		if strings.HasPrefix(set, "stack-") {
			return moveFileLocation{}, errors.New("stack volumes have no native counterpart")
		}
		// The set id names a shared directory; find its path among the
		// release profile's directories, else take the id as one level.
		p := set
		for _, raw := range tree.Dirs {
			if clean, ok := moveCleanRelative(filepath.ToSlash(raw)); ok && moveFileSetID(clean) == set {
				p = clean
			}
		}
		if moveFileExcludedPath(p) {
			return moveFileLocation{}, errors.New("this shared directory is not moved")
		}
		shared := filepath.Join(tree.Root, "shared")
		if why := moveSharedProblem(tree.Root, p); why != "" {
			return moveFileLocation{}, errors.New("shared/" + p + " on the destination is " + why)
		}
		if err := moveMkdirOwned(tree.Root, filepath.Join("shared", filepath.Dir(filepath.FromSlash(p))), tree.UID, tree.GID); err != nil {
			return moveFileLocation{}, err
		}
		return moveFileLocation{Set: MoveFileSet{ID: set, Path: p, Kind: MoveFileKindDirectory, Source: "shared/" + p}, Root: tree.Root, Shared: filepath.Join(shared, filepath.FromSlash(p)), UID: tree.UID, GID: tree.GID}, nil
	}
	if a.Retiring {
		return moveFileLocation{}, errors.New("application unavailable")
	}
	rid := a.resourceID()
	l := moveFileLocation{Resource: rid, Owner: a.runtimeUser()}
	switch {
	case set == "storage" || a.isRails() && (set == "public-system" || set == "public-uploads"):
		l.Volume = "lidza-storage-" + rid
		l.Set = MoveFileSet{ID: set, Path: strings.Replace(set, "-", "/", 1), Kind: MoveFileKindVolume, Source: "volume " + l.Volume}
		if a.isRails() {
			l.Subpath = set
		}
		if err := moveVolumeEnsure(ctx, "storage", rid, l.Volume, a.runtimeUser()); err != nil {
			return l, err
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if current := m.data.StorageVolumes[id]; current == "" {
			m.data.StorageVolumes[id] = l.Volume
			if err := m.save(); err != nil {
				delete(m.data.StorageVolumes, id)
				return l, errors.New("could not save the persistent storage attachment")
			}
		} else if current != l.Volume {
			return l, errors.New("the destination already uses another storage volume")
		}
		return l, nil
	case strings.HasPrefix(set, "stack-"):
		name := strings.TrimPrefix(set, "stack-")
		l.Volume, l.Stack, l.Owner = "lidza-stack-"+rid+"-"+name, true, ""
		l.Set = MoveFileSet{ID: set, Path: "stack/" + name, Kind: MoveFileKindVolume, Source: "stack volume " + l.Volume, PreserveOwners: true}
		if err := moveVolumeEnsure(ctx, "stack", rid, l.Volume, ""); err != nil {
			return l, err
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if !slices.Contains(m.data.StackVolumes[id], l.Volume) {
			previous := m.data.StackVolumes[id]
			m.data.StackVolumes[id] = append(slices.Clone(previous), l.Volume)
			if err := m.save(); err != nil {
				m.data.StackVolumes[id] = previous
				return l, errors.New("could not save the stack volume")
			}
		}
		return l, nil
	}
	return l, errors.New("this application has no place for the " + set + " files")
}

// moveMkdirOwned creates rel (slash-free platform path) under the
// application root one level at a time for the account, through a root so
// a link planted on the way cannot redirect it.
func moveMkdirOwned(appRoot, rel string, uid, gid int) error {
	root, err := os.OpenRoot(appRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	current := ""
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := root.Mkdir(current, 0o775); err != nil {
				return err
			}
			if os.Geteuid() == 0 && (uid != 0 || gid != 0) {
				if err := root.Lchown(current, uid, gid); err != nil {
					return err
				}
			}
		case err != nil:
			return err
		case info.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a link", filepath.Join(appRoot, current))
		case !info.IsDir():
			return fmt.Errorf("%s is not a directory", filepath.Join(appRoot, current))
		}
	}
	return nil
}

// cleanupMoveFiles removes a move's spool and the off-site objects it
// uploaded. Missing objects are not an error.
func (m *Manager) cleanupMoveFiles(ctx context.Context, move string) error {
	spool := m.moveSpool(move)
	keys := []string{}
	if entries, err := os.ReadDir(spool); err == nil {
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			if manifest, err := m.readMoveManifest(move, strings.TrimSuffix(entry.Name(), ".json")); err == nil && manifest.Key != "" {
				keys = append(keys, manifest.Key)
			}
		}
	}
	m.mu.Lock()
	cfg := m.data.BackupStorage
	m.mu.Unlock()
	var storageErr error
	if cfg != nil {
		if store, err := storage.New(*cfg); err == nil {
			if objects, err := store.List(ctx, "moves/"+move+"/", 1000); err == nil {
				for _, object := range objects {
					if !slices.Contains(keys, object.Key) {
						keys = append(keys, object.Key)
					}
				}
			}
			for _, key := range keys {
				if !strings.HasPrefix(key, "moves/"+move+"/") {
					continue
				}
				if err := store.Delete(ctx, key); err != nil {
					storageErr = errors.New("an off-site archive of the move could not be removed")
				}
			}
		} else if len(keys) > 0 {
			storageErr = errors.New("off-site storage is invalid; the move's archives were not removed")
		}
	} else if len(keys) > 0 {
		storageErr = errors.New("off-site storage is no longer configured; the move's archives were not removed")
	}
	if storageErr != nil {
		return storageErr
	}
	if err := os.RemoveAll(spool); err != nil {
		return errors.New("the move's spool could not be removed")
	}
	return nil
}
