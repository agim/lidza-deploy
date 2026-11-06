package release

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// bundleDeploymentPath returns the application-local BUNDLE_PATH that a
// release's .bundle/config pins in deployment mode, or "" when the release
// resolves gems any other way. Only such a bundle is ever cleaned: bundle
// clean then removes gems from that directory alone, never system gems.
func bundleDeploymentPath(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, ".bundle", "config"))
	if err != nil {
		return ""
	}
	path, deployment := "", false
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "BUNDLE_PATH":
			path = value
		case "BUNDLE_DEPLOYMENT":
			deployment = value == "true"
		}
	}
	if !deployment || !filepath.IsAbs(path) {
		return ""
	}
	return path
}

// bundleClean removes gems the lockfile no longer needs from the release's
// deployment bundle path. It logs one line when it removed something and a
// warning when it could not run; it never fails the deploy.
func bundleClean(ctx context.Context, x Executor, dir string, env []string, logf func(string, ...any)) {
	path := bundleDeploymentPath(dir)
	if path == "" {
		return
	}
	before := treeSize(path)
	var out bytes.Buffer
	err := x.Run(ctx, Command{Name: "bundle clean", Dir: dir, Args: []string{"bundle", "clean"}, Env: env, Stdout: &out, Stderr: &out})
	if err != nil {
		detail := strings.TrimSpace(tail(out.String(), 600))
		if detail != "" {
			detail = ": " + detail
		}
		logf("warning: bundle clean failed, unused gems stay in %s (%v)%s", path, err, detail)
		return
	}
	removed := 0
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "Removing ") {
			removed++
		}
	}
	if removed == 0 {
		return
	}
	logf("bundle clean removed %d unused %s from %s, freeing %s", removed, plural(removed, "gem"), path, formatBytes(before-treeSize(path)))
}

// treeSize sums the regular files below root; unreadable entries count as
// nothing.
func treeSize(root string) int64 {
	var size int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			size += info.Size()
		}
		return nil
	})
	return size
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func formatBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
