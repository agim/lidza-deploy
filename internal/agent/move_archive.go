package agent

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// File-set archives for application moves. Every read and write goes
// through an os.Root opened on the set's directory, so an application that
// swaps a directory for a link while the agent works cannot make the agent
// read or write outside the set: the source keeps serving during the copy,
// and the destination may already run the application during a final sync.

// moveOpenSet opens a set's directory. A missing directory is an empty set
// (nil root); a directory that is a link is refused.
func moveOpenSet(dir string) (*os.Root, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("the set is not a directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, errors.New("the set changed while it was opened")
	}
	return root, nil
}

// moveLinkProblem reports why the link at name cannot be moved, or "" when
// it stays inside the set. A link that resolves is checked where it really
// points; a dangling link may only point down from its directory.
func moveLinkProblem(root *os.Root, name string) string {
	target, err := root.Readlink(name)
	if err != nil {
		return "unreadable link"
	}
	if target == "" || filepath.IsAbs(target) {
		return "link to an absolute path"
	}
	lexical := filepath.Clean(filepath.Join(filepath.Dir(name), target))
	if lexical == ".." || strings.HasPrefix(lexical, ".."+string(filepath.Separator)) {
		return "link pointing outside the set"
	}
	_, err = root.Stat(name)
	switch {
	case err == nil:
		return ""
	case errors.Is(err, fs.ErrNotExist):
		for _, part := range strings.Split(filepath.ToSlash(target), "/") {
			if part == ".." {
				return "dangling link that leaves its directory"
			}
		}
		return ""
	}
	return "link pointing outside the set"
}

// moveMeasurement is what an inventory of a set found. Hard links inside
// the set are regular files and count once per name.
type moveMeasurement struct {
	Files int64
	Bytes int64
	// Skipped counts runtime files: sockets, named pipes and device nodes
	// that the service recreates when it starts. They are never archived
	// and never block a move.
	Skipped int64
	// Problems are links that cannot be moved.
	Problems []string
}

// moveMeasure counts a set's files, bytes and runtime files and lists the
// links that cannot be moved. A missing directory is an empty set.
func moveMeasure(ctx context.Context, dir string) (moveMeasurement, error) {
	var measured moveMeasurement
	root, err := moveOpenSet(dir)
	if err != nil || root == nil {
		return measured, err
	}
	defer root.Close()
	more := 0
	problem := func(name, why string) {
		if len(measured.Problems) < moveFileMaxProblem {
			measured.Problems = append(measured.Problems, name+" ("+why+")")
		} else {
			more++
		}
	}
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) && name != "." {
				return nil
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		switch mode := info.Mode(); {
		case mode.IsDir():
		case mode.IsRegular():
			measured.Files++
			measured.Bytes += info.Size()
		case mode&fs.ModeSymlink != 0:
			if why := moveLinkProblem(root, name); why != "" {
				problem(name, why)
			}
		default:
			measured.Skipped++
		}
		return nil
	})
	if more > 0 {
		measured.Problems = append(measured.Problems, fmt.Sprintf("and %d more", more))
	}
	return measured, err
}

func moveFileMode(mode fs.FileMode, dir bool) int64 {
	perm := mode.Perm() & 0o775
	if dir {
		perm |= 0o700
	} else {
		perm |= 0o600
	}
	return int64(perm)
}

type moveArchiveStats struct {
	files, bytes, entries int64
	// skipped counts the runtime files (sockets, pipes, devices) the
	// archive left behind.
	skipped int64
}

// moveWriteArchive writes dir as a tar stream, compressed with compression
// (zstd on every core, gzip, or none; stream_compress.go): relative paths, sanitized modes (no set-id bits, no
// world write) and modification times, numeric owners only with
// preserveOwners. Links that leave the set fail the archive. Sockets,
// named pipes and device nodes are runtime state the service recreates
// when it starts: they are skipped and counted. Hard links inside the set
// are archived as regular files. A missing directory is an archive with
// only its root.
func moveWriteArchive(ctx context.Context, dir string, preserveOwners bool, compression string, out io.Writer, limit int64) (moveArchiveStats, error) {
	var stats moveArchiveStats
	counter, _ := ctx.Value(moveArchiveCounterKey{}).(*atomic.Int64)
	compressing, cancel := context.WithCancel(ctx)
	defer cancel()
	zw, err := compressedWriter(compressing, out, compression)
	if err != nil {
		return stats, err
	}
	defer zw.Close()
	tw := tar.NewWriter(zw)
	finish := func() (moveArchiveStats, error) {
		if err := tw.Close(); err != nil {
			return stats, err
		}
		return stats, zw.Close()
	}
	root, err := moveOpenSet(dir)
	if err != nil {
		return stats, err
	}
	if root == nil {
		stats.entries = 1
		if err := tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o775, ModTime: time.Now().Truncate(time.Second), Format: tar.FormatPAX}); err != nil {
			return stats, err
		}
		return finish()
	}
	defer root.Close()
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) && name != "." {
				return nil // removed while archiving
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		stats.entries++
		if stats.entries > moveMaxFileEntries {
			return errors.New("the set has too many entries to move")
		}
		h := &tar.Header{Name: name, ModTime: info.ModTime().Truncate(time.Second), Format: tar.FormatPAX}
		owners := func(info fs.FileInfo) {
			if st, ok := info.Sys().(*syscall.Stat_t); ok && preserveOwners {
				h.Uid, h.Gid = int(st.Uid), int(st.Gid)
			}
		}
		owners(info)
		switch mode := info.Mode(); {
		case mode.IsDir():
			h.Typeflag, h.Mode = tar.TypeDir, moveFileMode(mode, true)
			if name == "." {
				h.Name = "./"
			} else {
				h.Name += "/"
			}
			return tw.WriteHeader(h)
		case mode&fs.ModeSymlink != 0:
			if why := moveLinkProblem(root, name); why != "" {
				return fmt.Errorf("%s cannot be moved: %s", name, why)
			}
			target, err := root.Readlink(name)
			if err != nil {
				return err
			}
			h.Typeflag, h.Linkname, h.Mode = tar.TypeSymlink, filepath.ToSlash(target), 0o777
			return tw.WriteHeader(h)
		case mode.IsRegular():
			f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if errors.Is(err, fs.ErrNotExist) {
				stats.entries--
				return nil
			}
			if err != nil {
				return fmt.Errorf("%s could not be read", name)
			}
			defer f.Close()
			opened, err := f.Stat()
			if err != nil || !opened.Mode().IsRegular() {
				return fmt.Errorf("%s changed while it was archived; retry", name)
			}
			owners(opened)
			stats.files++
			stats.bytes += opened.Size()
			if stats.bytes > limit {
				return errors.New("the set exceeds the transfer limit")
			}
			h.Typeflag, h.Mode, h.Size = tar.TypeReg, moveFileMode(opened.Mode(), false), opened.Size()
			h.ModTime = opened.ModTime().Truncate(time.Second)
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			var src io.Reader = f
			if counter != nil {
				src = moveCountedReader{f, counter}
			}
			if n, err := io.CopyN(tw, src, opened.Size()); err != nil || n != opened.Size() {
				return fmt.Errorf("%s changed while it was archived; retry", name)
			}
			return nil
		default:
			// A socket, named pipe or device node: runtime state that is
			// not an entry of the archive.
			stats.entries--
			stats.skipped++
			return nil
		}
	})
	if err != nil {
		return stats, err
	}
	return finish()
}

// moveArchiveCounterKey carries a counter of the file bytes read into an
// archive, for live progress.
type moveArchiveCounterKey struct{}

func withMoveArchiveCounter(ctx context.Context, counter *atomic.Int64) context.Context {
	return context.WithValue(ctx, moveArchiveCounterKey{}, counter)
}

type moveCountedReader struct {
	r io.Reader
	n *atomic.Int64
}

func (r moveCountedReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n.Add(int64(n))
	return n, err
}

// moveOwner gives extracted entries to an account; zero leaves owners.
type moveOwner struct{ UID, GID int }

// moveExtractArchive extracts a compressed tar (gzip, or zstd from agents
// that negotiated it) into staging, which
// must not exist. It refuses absolute paths, traversal, entries inside
// links, links that leave the tree, duplicates and special files; after
// extracting it checks every link again where it really points. Entries
// get owner, or their recorded numeric owners with preserveOwners; a
// non-zero rootMode replaces the archived mode of the tree's root.
func moveExtractArchive(ctx context.Context, r io.Reader, staging string, preserveOwners bool, owner moveOwner, rootMode fs.FileMode, limit int64) (moveArchiveStats, error) {
	var stats moveArchiveStats
	zr, compression, err := decompressedReader(ctx, r)
	if err != nil {
		return stats, err
	}
	defer zr.Close()
	if compression == compressionNone {
		return stats, errors.New("the archive is not a compressed stream")
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		return stats, err
	}
	root, err := os.OpenRoot(staging)
	if err != nil {
		return stats, err
	}
	defer root.Close()
	tr := tar.NewReader(zr)
	type dirTime struct {
		name string
		time time.Time
	}
	var dirs []dirTime
	var links []string
	ownerOf := map[string][2]int{}
	seen := map[string]bool{}
	refuse := func(name, why string) error {
		return fmt.Errorf("the archive entry %q was refused: %s", name, why)
	}
	// parent creates the directories above name and refuses links on the way.
	parent := func(name string) error {
		dir := filepath.Dir(name)
		if dir == "." {
			return nil
		}
		current := ""
		for _, part := range strings.Split(dir, string(filepath.Separator)) {
			current = filepath.Join(current, part)
			info, err := root.Lstat(current)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				if err := root.Mkdir(current, 0o755); err != nil {
					return err
				}
			case err != nil:
				return err
			case info.Mode()&fs.ModeSymlink != 0:
				return errors.New("it is inside a link")
			case !info.IsDir():
				return errors.New("it is inside a file")
			}
		}
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return stats, errors.New("the archive is damaged")
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		stats.entries++
		if stats.entries > moveMaxFileEntries {
			return stats, errors.New("the archive has too many entries")
		}
		raw := strings.TrimSuffix(h.Name, "/")
		if h.Typeflag == tar.TypeDir && (raw == "." || raw == "") {
			if err := root.Chmod(".", fs.FileMode(moveFileMode(fs.FileMode(h.Mode), true))); err != nil {
				return stats, err
			}
			dirs = append(dirs, dirTime{".", h.ModTime})
			ownerOf["."] = [2]int{h.Uid, h.Gid}
			continue
		}
		clean, ok := moveCleanRelative(raw)
		if !ok || clean != raw && "./"+clean != raw {
			return stats, refuse(h.Name, "absolute path or traversal")
		}
		if seen[clean] {
			return stats, refuse(h.Name, "it appears twice")
		}
		seen[clean] = true
		name := filepath.FromSlash(clean)
		if err := parent(name); err != nil {
			return stats, refuse(h.Name, err.Error())
		}
		existing, statErr := root.Lstat(name)
		exists := statErr == nil
		switch h.Typeflag {
		case tar.TypeDir:
			mode := fs.FileMode(moveFileMode(fs.FileMode(h.Mode), true))
			if exists && !existing.IsDir() {
				return stats, refuse(h.Name, "it appears twice")
			}
			if !exists {
				if err := root.Mkdir(name, mode); err != nil {
					return stats, err
				}
			}
			if err := root.Chmod(name, mode); err != nil {
				return stats, err
			}
			dirs = append(dirs, dirTime{name, h.ModTime})
		case tar.TypeReg, tar.TypeRegA:
			if exists {
				return stats, refuse(h.Name, "it appears twice")
			}
			if h.Size < 0 {
				return stats, refuse(h.Name, "invalid size")
			}
			stats.files++
			stats.bytes += h.Size
			if stats.bytes > limit {
				return stats, errors.New("the archive exceeds the transfer limit")
			}
			mode := fs.FileMode(moveFileMode(fs.FileMode(h.Mode), false))
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
			if err != nil {
				return stats, err
			}
			n, err := io.CopyN(f, tr, h.Size)
			if err == nil {
				err = f.Chmod(mode)
			}
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
			if err != nil || n != h.Size {
				return stats, errors.New("the archive is damaged")
			}
			if err := root.Chtimes(name, h.ModTime, h.ModTime); err != nil {
				return stats, err
			}
		case tar.TypeSymlink:
			if exists {
				return stats, refuse(h.Name, "it appears twice")
			}
			if h.Linkname == "" || strings.HasPrefix(h.Linkname, "/") || strings.ContainsAny(h.Linkname, "\x00\\") {
				return stats, refuse(h.Name, "link to an absolute path")
			}
			lexical := filepath.Clean(filepath.Join(filepath.Dir(name), filepath.FromSlash(h.Linkname)))
			if lexical == ".." || strings.HasPrefix(lexical, ".."+string(filepath.Separator)) {
				return stats, refuse(h.Name, "link pointing outside the set")
			}
			if err := root.Symlink(filepath.FromSlash(h.Linkname), name); err != nil {
				return stats, err
			}
			links = append(links, name)
		case tar.TypeLink:
			if exists {
				return stats, refuse(h.Name, "it appears twice")
			}
			linked, ok := moveCleanRelative(strings.TrimPrefix(h.Linkname, "./"))
			if !ok || !seen[linked] {
				return stats, refuse(h.Name, "hard link to a file that is not in the archive")
			}
			source := filepath.FromSlash(linked)
			if err := parent(source); err != nil {
				return stats, refuse(h.Name, "hard link "+err.Error())
			}
			info, err := root.Lstat(source)
			if err != nil || !info.Mode().IsRegular() {
				return stats, refuse(h.Name, "hard link to a file that is not in the archive")
			}
			if err := root.Link(source, name); err != nil {
				return stats, err
			}
			stats.files++
		default:
			return stats, refuse(h.Name, "special files are not moved")
		}
		ownerOf[name] = [2]int{h.Uid, h.Gid}
	}
	// Links are checked again where they really point, now that the whole
	// tree exists.
	for _, link := range links {
		if why := moveLinkProblem(root, link); why != "" {
			return stats, refuse(filepath.ToSlash(link), why)
		}
	}
	if rootMode != 0 {
		if err := root.Chmod(".", rootMode); err != nil {
			return stats, err
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = root.Chtimes(dirs[i].name, dirs[i].time, dirs[i].time)
	}
	if os.Geteuid() == 0 {
		if err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			uid, gid := owner.UID, owner.GID
			if preserveOwners {
				recorded := ownerOf[filepath.FromSlash(name)]
				uid, gid = recorded[0], recorded[1]
			}
			if uid == 0 && gid == 0 && !preserveOwners {
				return nil
			}
			return root.Lchown(name, uid, gid)
		}); err != nil {
			return stats, err
		}
	}
	return stats, nil
}
