package agent

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Directory backups: the fast ClickHouse archive payload. BACKUP DATABASE
// to a directory (Disk('backups', 'agent/lidza-<id>/') on the host server,
// File('lidza-<id>/') inside a managed container) writes one file per part
// file and restores in seconds, where a .zip or tar archive backup reads
// each file with a scan of the archive's directory and takes hours for
// tens of thousands of parts. The directory travels as entries under
// backup/ in the agent's own archive (clickhouse_archive.go), which the
// agent writes outside ClickHouse as one stream (optionally Zstandard,
// stream_compress.go); the destination extracts it into its backups
// directory and restores from there.

const clickhouseDirectoryPrefix = "backup/"

var clickhouseBackupDirName = regexp.MustCompile(`^lidza-[a-z0-9-]{1,64}$`)

func newClickHouseBackupDirName() string { return "lidza-" + newID() }

// clickhouseDirectoryEntry validates one relative path of a backup
// directory: no absolute paths, no parent references, no empty parts.
func clickhouseDirectoryEntry(name string) (string, error) {
	name = strings.TrimPrefix(name, "./")
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" || trimmed == "." {
		return "", nil
	}
	if strings.HasPrefix(trimmed, "/") || strings.Contains(trimmed, "\\") || path.Clean(trimmed) != trimmed {
		return "", fmt.Errorf("the backup contains an invalid path %q", boundedErrorText(name, 120))
	}
	for _, part := range strings.Split(trimmed, "/") {
		if part == ".." || part == "" {
			return "", fmt.Errorf("the backup contains an invalid path %q", boundedErrorText(name, 120))
		}
	}
	return trimmed, nil
}

// ---- host server: the backups disk ------------------------------------------------

func (f hostBackupFiles) dirRef(name string) string {
	return "Disk('backups', '" + nativeCHBackupSubdir + "/" + name + "/')"
}

func (f hostBackupFiles) dirPath(name string) (string, error) {
	if !clickhouseBackupDirName.MatchString(name) {
		return "", errors.New("invalid ClickHouse backup directory name")
	}
	return filepath.Join(f.dir, nativeCHBackupSubdir, name), nil
}

// readDir streams the backup directory as a tar of relative paths.
func (f hostBackupFiles) readDir(ctx context.Context, name string) (io.ReadCloser, error) {
	dir, err := f.dirPath(name)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, errors.New("the ClickHouse backup directory is missing")
	}
	return tarDirectory(ctx, dir), nil
}

// tarDirectory streams dir's directories and regular files as a tar.
func tarDirectory(ctx context.Context, dir string) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		tw := tar.NewWriter(writer)
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil || rel == "." {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			switch {
			case entry.IsDir():
				return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: rel + "/", Mode: 0o750, ModTime: info.ModTime(), Format: tar.FormatPAX})
			case info.Mode().IsRegular():
				file, err := os.Open(path)
				if err != nil {
					return err
				}
				defer file.Close()
				if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: rel, Mode: 0o640, Size: info.Size(), ModTime: info.ModTime(), Format: tar.FormatPAX}); err != nil {
					return err
				}
				n, err := io.Copy(tw, file)
				if err == nil && n != info.Size() {
					err = errors.New("a backup file changed while it was read")
				}
				return err
			}
			return fmt.Errorf("the backup directory contains %s, which is not a regular file", rel)
		})
		if err == nil {
			err = tw.Close()
		}
		writer.CloseWithError(err)
	}()
	return reader
}

// writeDir extracts a tar of relative paths into the backup directory,
// owned by the server's user (directories 0750, files 0640).
func (f hostBackupFiles) writeDir(ctx context.Context, name string, stream io.Reader) error {
	dir, err := f.dirPath(name)
	if err != nil {
		return err
	}
	return f.writeDirAt(ctx, dir, stream)
}

func (f hostBackupFiles) writeDirAt(ctx context.Context, dir string, stream io.Reader) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return errors.New("the ClickHouse backups disk is unavailable")
	}
	_ = os.Chmod(parent, 0o750)
	if err := os.Mkdir(dir, 0o750); err != nil {
		return errors.New("the ClickHouse backup directory could not be created")
	}
	chown := func(path string) error {
		if !f.chown {
			return nil
		}
		return os.Lchown(path, f.uid, f.gid)
	}
	for _, p := range []string{parent, dir} {
		if err := chown(p); err != nil {
			return errors.New("the ClickHouse backups disk could not be handed to the server")
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(stream)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.New("the backup directory stream is damaged")
		}
		rel, err := clickhouseDirectoryEntry(header.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(rel, 0o750); err != nil {
				return fmt.Errorf("the backup directory could not be written: %w", err)
			}
			if f.chown {
				if err := root.Lchown(rel, f.uid, f.gid); err != nil {
					return errors.New("the backup directory could not be handed to the server")
				}
			}
		case tar.TypeReg:
			if dirname := path.Dir(rel); dirname != "." {
				if err := root.MkdirAll(dirname, 0o750); err != nil {
					return fmt.Errorf("the backup directory could not be written: %w", err)
				}
			}
			if err := writeRootFile(root, rel, tr, header.Size, f); err != nil {
				return err
			}
		default:
			return fmt.Errorf("the backup contains %s, which is not a regular file", boundedErrorText(rel, 120))
		}
	}
}

func writeRootFile(root *os.Root, rel string, data io.Reader, size int64, f hostBackupFiles) error {
	file, err := root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("the backup file could not be created: %w", err)
	}
	if f.chown {
		if err := file.Chown(f.uid, f.gid); err != nil {
			file.Close()
			return errors.New("the backup file could not be handed to the server")
		}
	}
	n, err := io.Copy(file, data)
	if err == nil && n != size {
		err = errors.New("a backup file is incomplete")
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (f hostBackupFiles) removeDir(_ context.Context, name string) error {
	dir, err := f.dirPath(name)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// ---- managed container: File() inside the container -------------------------------

func (f containerBackupFiles) dirRef(name string) string { return "File('" + name + "/')" }

func (f containerBackupFiles) dirPath(name string) (string, error) {
	if !clickhouseBackupDirName.MatchString(name) || !strings.HasPrefix(f.container, "lidza-clickhouse-") {
		return "", errors.New("invalid ClickHouse backup directory")
	}
	return containerClickHouseBackupDir + name, nil
}

// readDir streams the directory out of the container with its tar.
func (f containerBackupFiles) readDir(ctx context.Context, name string) (io.ReadCloser, error) {
	dir, err := f.dirPath(name)
	if err != nil {
		return nil, err
	}
	cmd := clickhouseDockerExec(ctx, "exec", f.container, "tar", "cf", "-", "-C", dir, ".")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, errors.New("the ClickHouse backup could not be read from the container")
	}
	return commandReader{ReadCloser: stdout, cmd: cmd}, nil
}

// writeDir extracts the stream into the container; files stay readable by
// the server's user (root-owned, 0644, like single backup files).
func (f containerBackupFiles) writeDir(ctx context.Context, name string, stream io.Reader) error {
	dir, err := f.dirPath(name)
	if err != nil {
		return err
	}
	cmd := clickhouseDockerExec(ctx, "exec", "-i", f.container, "sh", "-c", `umask 022; mkdir "$1" && tar xf - --no-same-owner --no-same-permissions -C "$1"`, "sh", dir)
	cmd.Stdin = stream
	var output limitedBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		return errors.New("the ClickHouse backup could not be copied into the container: " + boundedErrorText(strings.TrimSpace(output.String()), 300))
	}
	return nil
}

func (f containerBackupFiles) removeDir(ctx context.Context, name string) error {
	dir, err := f.dirPath(name)
	if err != nil {
		return err
	}
	return clickhouseDockerExec(ctx, "exec", f.container, "rm", "-rf", dir).Run()
}

// ---- archive payload ------------------------------------------------------------------

// copyClickHouseDirectory copies a backup directory's tar stream into the
// archive under backup/; it returns the number of files.
func copyClickHouseDirectory(ctx context.Context, tw *tar.Writer, stream io.Reader, created time.Time) (int64, error) {
	tr := tar.NewReader(stream)
	var files int64
	progress := progressFrom(ctx)
	for {
		if err := ctx.Err(); err != nil {
			return files, err
		}
		header, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return files, errors.New("the ClickHouse backup directory could not be read completely")
		}
		rel, err := clickhouseDirectoryEntry(header.Name)
		if err != nil || rel == "" {
			if err != nil {
				return files, err
			}
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: clickhouseDirectoryPrefix + rel + "/", Mode: 0o750, ModTime: created, Format: tar.FormatPAX}); err != nil {
				return files, err
			}
		case tar.TypeReg:
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: clickhouseDirectoryPrefix + rel, Mode: 0o640, Size: header.Size, ModTime: created, Format: tar.FormatPAX}); err != nil {
				return files, err
			}
			if n, err := io.Copy(tw, tr); err != nil || n != header.Size {
				return files, errors.New("a ClickHouse backup file could not be read completely")
			}
			files++
			if files%256 == 0 {
				progress.Objects("files", files, 0)
			}
		default:
			return files, fmt.Errorf("the ClickHouse backup contains %s, which is not a regular file", boundedErrorText(rel, 120))
		}
	}
}

// directoryStream turns the archive's backup/ entries into a tar of
// relative paths for the backups directory, starting with first.
func directoryStream(ctx context.Context, tr *tar.Reader, first *tar.Header, files *atomic.Int64) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		tw := tar.NewWriter(writer)
		err := func() error {
			for header := first; ; {
				if err := ctx.Err(); err != nil {
					return err
				}
				if !strings.HasPrefix(header.Name, clickhouseDirectoryPrefix) {
					return errors.New("the archive has entries after the ClickHouse backup")
				}
				rel, err := clickhouseDirectoryEntry(strings.TrimPrefix(header.Name, clickhouseDirectoryPrefix))
				if err != nil {
					return err
				}
				if rel != "" {
					switch header.Typeflag {
					case tar.TypeDir:
						if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: rel + "/", Mode: 0o750, ModTime: header.ModTime, Format: tar.FormatPAX}); err != nil {
							return err
						}
					case tar.TypeReg:
						if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: rel, Mode: 0o640, Size: header.Size, ModTime: header.ModTime, Format: tar.FormatPAX}); err != nil {
							return err
						}
						if n, err := io.Copy(tw, tr); err != nil || n != header.Size {
							return errors.New("the archive's ClickHouse backup is incomplete")
						}
						files.Add(1)
					default:
						return errors.New("the archive's ClickHouse backup contains an entry that is not a regular file")
					}
				}
				next, err := tr.Next()
				if err == io.EOF {
					return tw.Close()
				}
				if err != nil {
					return errors.New("the archive's ClickHouse backup is damaged")
				}
				header = next
			}
		}()
		writer.CloseWithError(err)
	}()
	return reader
}

// ---- operations: progress and cancellation -----------------------------------------

type clickhouseOperationStatus struct {
	Status    string `json:"status"`
	Error     string `json:"error"`
	QueryID   string `json:"query_id"`
	NumFiles  uint64 `json:"num_files"`
	FilesRead uint64 `json:"files_read"`
	TotalSize uint64 `json:"total_size"`
	BytesRead uint64 `json:"bytes_read"`
}

// cancelClickHouseOperation stops a running BACKUP or RESTORE: an aborted
// move or a cancelled backup never waits for it.
func cancelClickHouseOperation(ctx context.Context, c clickhouseClient, id, queryID string) {
	kill, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if queryID == "" {
		if rows, err := clickhouseRows[clickhouseOperationStatus](kill, c, "SELECT status, error, query_id, num_files, files_read, total_size, bytes_read FROM system.backups WHERE id = "+mustCHString(id)); err == nil && len(rows) == 1 {
			queryID = rows[0].QueryID
		}
	}
	if queryID == "" {
		return
	}
	literal, err := chString(queryID)
	if err != nil {
		return
	}
	_ = c.exec(kill, "KILL QUERY WHERE query_id = "+literal+" ASYNC")
}

func (s clickhouseOperationStatus) progress(t *ProgressTracker, label string, restore bool, bytes *atomic.Int64) {
	if t == nil || label == "" {
		return
	}
	if restore {
		bytes.Store(int64(min(s.BytesRead, 1<<62)))
		if s.NumFiles > 0 {
			t.Objects("files", int64(min(s.FilesRead, 1<<62)), int64(min(s.NumFiles, 1<<62)))
		}
		return
	}
	if s.NumFiles > 0 {
		t.Objects("files", int64(min(s.NumFiles, 1<<62)), 0)
	}
}

// ---- replacing a database -----------------------------------------------------------

// clickhousePreviousSuffix names the database a restore moves aside.
const clickhousePreviousSuffix = "_lidza_previous"

func (s clickhouseService) databasesPresent(ctx context.Context, names ...string) (map[string]bool, error) {
	literals := []string{}
	for _, name := range names {
		literal, err := chString(name)
		if err != nil {
			return nil, err
		}
		literals = append(literals, literal)
	}
	type row struct {
		Name   string `json:"name"`
		Engine string `json:"engine"`
	}
	rows, err := clickhouseRows[row](ctx, s.client, "SELECT name, engine FROM system.databases WHERE name IN ("+strings.Join(literals, ", ")+")")
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, r := range rows {
		present[r.Name] = true
	}
	return present, nil
}

// replaceDatabase runs restore into the service's database so a failure
// leaves the database as it was: the current database is renamed aside,
// restore and verify run against the original name (so RESTORE ... AS
// rewrites views to it), and the aside copy is dropped only after both
// succeeded. A failure drops the partial database and renames the
// previous one back. Without RENAME (an Ordinary database) the database
// is dropped first, as before.
func (s clickhouseService) replaceDatabase(ctx context.Context, restore func(context.Context) error) error {
	target := s.database
	previous := target + clickhousePreviousSuffix
	if _, err := chIdent(previous); err != nil || len(previous) > 200 {
		previous = ""
	}
	present, err := s.databasesPresent(ctx, target, previous)
	if err != nil {
		return err
	}
	cleanup := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	}
	aside := false
	switch {
	case previous == "":
	case present[previous] && present[target]:
		// An earlier restore stopped after moving the database aside: the
		// aside copy is the one to keep; the other is partial.
		if err := s.client.exec(ctx, "DROP DATABASE IF EXISTS "+mustCHIdent(target)+" SYNC"); err != nil {
			return err
		}
		aside = true
	case present[previous]:
		aside = true
	case present[target]:
		if err := s.client.exec(ctx, "RENAME DATABASE "+mustCHIdent(target)+" TO "+mustCHIdent(previous)); err == nil {
			aside = true
		}
	}
	if !aside {
		if err := s.client.exec(ctx, "DROP DATABASE IF EXISTS "+mustCHIdent(target)+" SYNC"); err != nil {
			return err
		}
	}
	if err := restore(ctx); err != nil {
		if aside {
			undo, cancel := cleanup()
			defer cancel()
			if dropErr := s.client.exec(undo, "DROP DATABASE IF EXISTS "+mustCHIdent(target)+" SYNC"); dropErr == nil {
				if renameErr := s.client.exec(undo, "RENAME DATABASE "+mustCHIdent(previous)+" TO "+mustCHIdent(target)); renameErr != nil {
					return fmt.Errorf("%w; the previous database is kept as %s", err, previous)
				}
			}
		}
		return err
	}
	if aside {
		done, cancel := cleanup()
		defer cancel()
		_ = s.client.exec(done, "DROP DATABASE IF EXISTS "+mustCHIdent(previous)+" SYNC")
	}
	return nil
}

// clickhouseFiles formats a file count for logs.
func clickhouseFiles(n int64) string { return strconv.FormatInt(n, 10) + " files" }
