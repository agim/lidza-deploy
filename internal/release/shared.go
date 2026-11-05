package release

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Shared lists release paths that are replaced by links into shared/, so
// their contents survive every release.
type Shared struct {
	Dirs  []string // created in shared/ when missing
	Files []string // must already exist in shared/
}

func cleanRelative(path string) (string, error) {
	clean := filepath.Clean(path)
	if path == "" || filepath.IsAbs(path) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("release: shared path %q must stay inside the release", path)
	}
	return clean, nil
}

// LinkShared replaces each shared path in release id with a relative link to
// the same path under shared/, matching links like log -> ../../shared/log.
func (l Layout) LinkShared(id string, shared Shared) error {
	if !ValidID(id) {
		return fmt.Errorf("release: invalid id %q", id)
	}
	release := l.Release(id)
	link := func(path string, dir bool) error {
		clean, err := cleanRelative(path)
		if err != nil {
			return err
		}
		target := filepath.Join(l.Shared(), clean)
		if dir {
			if err := l.mkdirOwned(l.Shared(), clean); err != nil {
				return err
			}
		} else if info, err := os.Stat(target); err != nil || info.IsDir() {
			return fmt.Errorf("release: shared file %s is missing", target)
		}
		location := filepath.Join(release, clean)
		if err := l.mkdirOwned(release, filepath.Dir(clean)); err != nil {
			return err
		}
		if err := os.RemoveAll(location); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		relative, err := filepath.Rel(filepath.Dir(location), target)
		if err != nil {
			return err
		}
		if err := os.Symlink(relative, location); err != nil {
			return err
		}
		return l.chown(location)
	}
	for _, dir := range shared.Dirs {
		if err := link(dir, true); err != nil {
			return err
		}
	}
	for _, file := range shared.Files {
		if err := link(file, false); err != nil {
			return err
		}
	}
	return nil
}

// mkdirOwned creates base/relative one level at a time, giving every new
// directory to the application owner. Existing symlinks are refused so a
// release cannot redirect writes outside its tree.
func (l Layout) mkdirOwned(base, relative string) error {
	if info, err := os.Lstat(base); errors.Is(err, os.ErrNotExist) {
		if err := l.ensureDir(base); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("release: %s is not a directory", base)
	}
	if relative == "." || relative == "" {
		return nil
	}
	current := base
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		switch {
		case err == nil && info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("release: %s is a symlink", current)
		case err == nil && !info.IsDir():
			return fmt.Errorf("release: %s is not a directory", current)
		case err == nil:
			continue
		case !errors.Is(err, os.ErrNotExist):
			return err
		}
		if err := os.Mkdir(current, 0o775); err != nil {
			return err
		}
		if err := os.Chmod(current, 0o775); err != nil {
			return err
		}
		if err := l.chown(current); err != nil {
			return err
		}
	}
	return nil
}
