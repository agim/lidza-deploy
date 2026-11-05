package release

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Source fetches application code into the layout's bare mirror and exports
// exact commits from it. The remote URL never carries credentials: a token
// is passed to git through the environment for the duration of one command,
// so it is not written to the repository config or visible in argv.
type Source struct {
	URL   string // https://github.com/owner/name.git
	Token string // optional short-lived token (for example a GitHub App installation token)
	Git   string // git binary; default "git"
	Env   []string
}

func (s Source) git() string {
	if s.Git != "" {
		return s.Git
	}
	return "git"
}

func (s Source) env(network bool) []string {
	env := withEnv(s.Env, "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "GIT_CONFIG_NOSYSTEM=1")
	if network && s.Token != "" {
		credential := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + s.Token))
		env = withEnv(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+credential)
	}
	return env
}

func (s Source) validate() error {
	local := strings.HasPrefix(s.URL, "file:///") && s.Token == ""
	if !local && (!strings.HasPrefix(s.URL, "https://") || strings.Contains(s.URL, "@")) {
		return errors.New("release: source URL must be https without embedded credentials")
	}
	return nil
}

// Fetch creates or refreshes the bare mirror and returns the commit of ref
// (a branch name, or an exact commit already fetched).
func (s Source) Fetch(ctx context.Context, x Executor, l Layout, ref string) (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	repo := l.Repo()
	if _, err := os.Stat(filepath.Join(repo, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if err := l.ensureDir(repo); err != nil {
			return "", err
		}
		if _, err := output(ctx, x, Command{Name: "git init", Dir: repo, Env: s.env(false),
			Args: []string{s.git(), "init", "--bare", "--quiet", repo}}); err != nil {
			return "", err
		}
	}
	// Fetch from the explicit URL so a stale stored remote (or one carrying
	// someone else's credential) is never used.
	if _, err := output(ctx, x, Command{Name: "git fetch", Dir: repo, Env: s.env(true), Network: true,
		Args: []string{s.git(), "-C", repo, "fetch", "--quiet", "--prune", "--no-tags", s.URL,
			"+refs/heads/*:refs/heads/*"}}); err != nil {
		return "", err
	}
	return s.Resolve(ctx, x, l, ref)
}

// Resolve returns the full commit for ref in the mirror.
func (s Source) Resolve(ctx context.Context, x Executor, l Layout, ref string) (string, error) {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("release: invalid ref %q", ref)
	}
	if !commitPattern.MatchString(ref) {
		ref = "refs/heads/" + strings.TrimPrefix(ref, "refs/heads/")
	}
	commit, err := output(ctx, x, Command{Name: "git rev-parse", Dir: l.Repo(), Env: s.env(false),
		Args: []string{s.git(), "-C", l.Repo(), "rev-parse", "--verify", "--quiet", ref + "^{commit}"}})
	if err != nil {
		return "", fmt.Errorf("release: %s is not in the mirror: %w", ref, err)
	}
	if !commitPattern.MatchString(commit) {
		return "", fmt.Errorf("release: unexpected commit %q", commit)
	}
	return commit, nil
}

// Export writes the tree of commit into release id and records REVISION.
func (s Source) Export(ctx context.Context, x Executor, l Layout, id, commit string) error {
	if !commitPattern.MatchString(commit) || !ValidID(id) {
		return errors.New("release: export needs an exact commit and release id")
	}
	dir := l.Release(id)
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := x.Run(ctx, Command{Name: "git archive", Dir: l.Repo(), Env: s.env(false), Stdout: writer,
			Args: []string{s.git(), "-C", l.Repo(), "archive", "--format=tar", commit}})
		writer.CloseWithError(err)
		done <- err
	}()
	extractErr := x.Run(ctx, Command{Name: "tar extract", Dir: dir, Env: s.env(false), Stdin: reader,
		Args: []string{"tar", "--extract", "--no-same-owner", "--no-same-permissions", "--file=-", "--directory=" + dir}})
	reader.CloseWithError(errors.New("extract finished"))
	if err := <-done; err != nil {
		return fmt.Errorf("release: git archive %s: %w", commit, err)
	}
	if extractErr != nil {
		return fmt.Errorf("release: extract %s: %w", commit, extractErr)
	}
	revision := filepath.Join(dir, RevisionFile)
	if err := os.WriteFile(revision, []byte(commit+"\n"), 0o644); err != nil {
		return err
	}
	return l.chown(revision)
}

// shortCommit is used in logs.
func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
