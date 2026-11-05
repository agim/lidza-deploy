package detect

import (
	"io/fs"
	"path"
	"strings"
)

// Tree is a Source over a known list of paths (for example a recursive Git
// tree listing). Directories are implied by the paths; a path ending in "/"
// names an empty directory. Read is called only for listed files.
func Tree(paths []string, read func(path string) ([]byte, error)) Source {
	t := &tree{dirs: map[string]map[string]bool{"": {}}, read: read}
	for _, p := range paths {
		dir := strings.HasSuffix(p, "/")
		p = strings.Trim(path.Clean("/"+p), "/")
		if p == "" {
			continue
		}
		t.add(p, dir)
	}
	return t
}

// Files is an in-memory Source, mainly for tests: keys are file paths and
// values their contents.
func Files(files map[string]string) Source {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	return Tree(paths, func(p string) ([]byte, error) {
		content, ok := files[p]
		if !ok {
			return nil, fs.ErrNotExist
		}
		if len(content) > MaxFileSize {
			return nil, ErrTooLarge
		}
		return []byte(content), nil
	})
}

type tree struct {
	dirs  map[string]map[string]bool // dir -> entry names (directories end in "/")
	files map[string]bool
	read  func(string) ([]byte, error)
}

func (t *tree) add(p string, dir bool) {
	if t.files == nil {
		t.files = map[string]bool{}
	}
	parts := strings.Split(p, "/")
	for i := range parts {
		parent := strings.Join(parts[:i], "/")
		name := parts[i]
		last := i == len(parts)-1
		if !last || dir {
			name += "/"
			child := strings.Join(parts[:i+1], "/")
			if t.dirs[child] == nil {
				t.dirs[child] = map[string]bool{}
			}
		} else {
			t.files[p] = true
		}
		if t.dirs[parent] == nil {
			t.dirs[parent] = map[string]bool{}
		}
		t.dirs[parent][name] = true
	}
}

func (t *tree) List(dir string) ([]string, error) {
	entries, ok := t.dirs[strings.Trim(dir, "/")]
	if !ok {
		return nil, fs.ErrNotExist
	}
	out := make([]string, 0, len(entries))
	for name := range entries {
		out = append(out, name)
	}
	return out, nil
}

func (t *tree) Read(p string) ([]byte, error) {
	p = strings.Trim(p, "/")
	if !t.files[p] {
		return nil, fs.ErrNotExist
	}
	return t.read(p)
}
