// Package release deploys applications into the conventional
// releases/current/shared directory layout:
//
//	<root>/repo                bare mirror of the source repository
//	<root>/releases/<id>       one immutable tree per release (UTC timestamp)
//	<root>/shared/             state that survives releases (logs, uploads, gems)
//	<root>/current -> releases/<id>
//
// A release is built beside the running one, then published by atomically
// replacing the current symlink. Processes are restarted through their
// existing service manager units, so socket-activated servers keep accepting
// connections while they restart. Rollback republishes an earlier release.
package release

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"
)

// IncompleteMarker exists in a release directory until its build finishes.
// Releases carrying it are never published, rolled back to, or kept by Prune.
const IncompleteMarker = ".release-incomplete"

// RevisionFile records the exact source commit of a release.
const RevisionFile = "REVISION"

var (
	idPattern = regexp.MustCompile(`^[0-9]{14}$`)
	// namePattern admits releases made by earlier tools, which may use other
	// identifiers; the engine only ever creates timestamp identifiers.
	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// Owner is the account that owns application files and runs its processes.
type Owner struct {
	UID int
	GID int
}

// Layout locates one application's release tree.
type Layout struct {
	Root  string
	Owner Owner
}

func (l Layout) Repo() string             { return filepath.Join(l.Root, "repo") }
func (l Layout) Releases() string         { return filepath.Join(l.Root, "releases") }
func (l Layout) Shared() string           { return filepath.Join(l.Root, "shared") }
func (l Layout) Current() string          { return filepath.Join(l.Root, "current") }
func (l Layout) Release(id string) string { return filepath.Join(l.Releases(), id) }

// ValidID reports whether id has the release timestamp form the engine creates.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// ValidName reports whether id names a release directory the engine will
// publish or roll back to, including releases made by earlier tools.
func ValidName(id string) bool { return namePattern.MatchString(id) && id != "." && id != ".." }

// NewID returns the release identifier for t.
func NewID(t time.Time) string { return t.UTC().Format("20060102150405") }

// Create makes a new, empty, incomplete release directory owned by the
// application owner. Identifiers are unique: a collision moves to the next
// second.
func (l Layout) Create(now time.Time) (string, error) {
	if err := l.ensureDir(l.Releases()); err != nil {
		return "", err
	}
	for attempt := 0; attempt < 60; attempt++ {
		id := NewID(now.Add(time.Duration(attempt) * time.Second))
		dir := l.Release(id)
		if err := os.Mkdir(dir, 0o775); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", err
		}
		// Mkdir honours the caller's umask; releases must stay readable by
		// the web server and other owner processes.
		if err := os.Chmod(dir, 0o775); err != nil {
			return "", err
		}
		if err := l.chown(dir); err != nil {
			return "", err
		}
		marker := filepath.Join(dir, IncompleteMarker)
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			return "", err
		}
		if err := l.chown(marker); err != nil {
			return "", err
		}
		return id, nil
	}
	return "", errors.New("release: no free release identifier")
}

// Complete marks a built release as publishable.
func (l Layout) Complete(id string) error {
	if !ValidName(id) {
		return fmt.Errorf("release: invalid id %q", id)
	}
	if _, err := os.Stat(filepath.Join(l.Release(id), RevisionFile)); err != nil {
		return fmt.Errorf("release %s has no %s", id, RevisionFile)
	}
	err := os.Remove(filepath.Join(l.Release(id), IncompleteMarker))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Publishable reports whether release id finished building.
func (l Layout) Publishable(id string) bool {
	if !ValidName(id) {
		return false
	}
	info, err := os.Lstat(l.Release(id))
	if err != nil || !info.IsDir() {
		return false
	}
	if _, err := os.Lstat(filepath.Join(l.Release(id), IncompleteMarker)); err == nil {
		return false
	}
	_, err = os.Stat(filepath.Join(l.Release(id), RevisionFile))
	return err == nil
}

// Revision returns the source commit recorded in release id.
func (l Layout) Revision(id string) (string, error) {
	if !ValidName(id) {
		return "", fmt.Errorf("release: invalid id %q", id)
	}
	raw, err := os.ReadFile(filepath.Join(l.Release(id), RevisionFile))
	if err != nil {
		return "", err
	}
	return string(trimSpace(raw)), nil
}

type releaseEntry struct {
	id       string
	modified time.Time
	complete bool
}

// entries returns release directories oldest first. Order is by the time a
// release was last changed (its build completion or import), then by name,
// so releases made by earlier tools with other identifiers sort correctly
// beside timestamp identifiers.
func (l Layout) entries() ([]releaseEntry, error) {
	dirents, err := os.ReadDir(l.Releases())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []releaseEntry
	for _, dirent := range dirents {
		id := dirent.Name()
		if !ValidName(id) || !dirent.IsDir() {
			continue
		}
		info, err := dirent.Info()
		if err != nil {
			continue
		}
		result = append(result, releaseEntry{id: id, modified: info.ModTime(), complete: l.Publishable(id)})
	}
	sort.SliceStable(result, func(i, j int) bool {
		if !result[i].modified.Equal(result[j].modified) {
			return result[i].modified.Before(result[j].modified)
		}
		return result[i].id < result[j].id
	})
	return result, nil
}

// List returns publishable release identifiers, oldest first.
func (l Layout) List() ([]string, error) {
	entries, err := l.entries()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		if entry.complete {
			ids = append(ids, entry.id)
		}
	}
	return ids, nil
}

// CurrentID returns the release that current points to, or "" when the
// application has never been published.
func (l Layout) CurrentID() (string, error) {
	target, err := os.Readlink(l.Current())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("release: current must be a symlink: %w", err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(l.Root, target)
	}
	target = filepath.Clean(target)
	if filepath.Dir(target) != filepath.Clean(l.Releases()) || !ValidName(filepath.Base(target)) {
		return "", fmt.Errorf("release: current points outside releases: %s", target)
	}
	return filepath.Base(target), nil
}

// Previous returns the newest publishable release older than current.
func (l Layout) Previous() (string, error) {
	current, err := l.CurrentID()
	if err != nil {
		return "", err
	}
	ids, err := l.List()
	if err != nil {
		return "", err
	}
	position := len(ids)
	for i, id := range ids {
		if id == current {
			position = i
		}
	}
	if position == 0 {
		return "", errors.New("release: no earlier release to roll back to")
	}
	return ids[position-1], nil
}

// Activate atomically points current at release id. It refuses when current
// no longer points at expected ("" means no current release yet), so two
// publishers can never silently overwrite each other.
func (l Layout) Activate(id, expected string) error {
	if !l.Publishable(id) {
		return fmt.Errorf("release %s is not publishable", id)
	}
	actual, err := l.CurrentID()
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("release: current changed to %q (expected %q); publication refused", actual, expected)
	}
	link := l.Current()
	temporary := fmt.Sprintf("%s.next-%d", link, time.Now().UnixNano())
	if err := os.Symlink(filepath.Join("releases", id), temporary); err != nil {
		return err
	}
	defer os.Remove(temporary)
	owner := l.Owner
	if info, err := os.Lstat(link); err == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			owner = Owner{UID: int(stat.Uid), GID: int(stat.Gid)}
		}
	}
	if err := lchown(temporary, owner); err != nil {
		return err
	}
	if err := os.Rename(temporary, link); err != nil {
		return err
	}
	return syncDir(l.Root)
}

// Prune deletes the oldest releases beyond keep, never removing current,
// any protected id, or a release still being built by a running deploy.
// Abandoned incomplete releases older than every kept release are removed.
func (l Layout) Prune(keep int, protect ...string) ([]string, error) {
	if keep < 2 {
		return nil, errors.New("release: keep at least two releases for rollback")
	}
	current, err := l.CurrentID()
	if err != nil {
		return nil, err
	}
	entries, err := l.entries()
	if err != nil {
		return nil, err
	}
	keepSet := map[string]bool{current: true}
	for _, id := range protect {
		keepSet[id] = true
	}
	complete := 0
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].complete && complete < keep {
			keepSet[entries[i].id] = true
			complete++
		}
	}
	var oldestKept time.Time
	for _, entry := range entries {
		if keepSet[entry.id] && (oldestKept.IsZero() || entry.modified.Before(oldestKept)) {
			oldestKept = entry.modified
		}
	}
	var removed []string
	for _, entry := range entries {
		if keepSet[entry.id] {
			continue
		}
		if !entry.complete && !entry.modified.Before(oldestKept) {
			continue // may belong to a deploy in progress
		}
		if err := os.RemoveAll(l.Release(entry.id)); err != nil {
			return removed, err
		}
		removed = append(removed, entry.id)
	}
	return removed, nil
}

func (l Layout) ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o775); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o775); err != nil {
		return err
	}
	return l.chown(dir)
}

func (l Layout) chown(path string) error { return lchown(path, l.Owner) }

func lchown(path string, owner Owner) error {
	if owner.UID == 0 && owner.GID == 0 || os.Geteuid() != 0 {
		return nil
	}
	return os.Lchown(path, owner.UID, owner.GID)
}

func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

func trimSpace(raw []byte) []byte {
	start, end := 0, len(raw)
	for start < end && (raw[start] == ' ' || raw[start] == '\n' || raw[start] == '\r' || raw[start] == '\t') {
		start++
	}
	for end > start && (raw[end-1] == ' ' || raw[end-1] == '\n' || raw[end-1] == '\r' || raw[end-1] == '\t') {
		end--
	}
	return raw[start:end]
}
