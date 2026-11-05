package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/detect"
	gh "github.com/agim/lidza-deploy/internal/providers/github"
)

// Repository detection for the new-application wizard: the repository is
// read through the GitHub API with the installation's read-only token, and
// internal/detect proposes how to build and run it.

const (
	detectCacheTTL     = 60 * time.Second
	detectCacheEntries = 128
	detectTreeEntries  = 20000
	detectTimeout      = 25 * time.Second
)

// RepositoryDetection is the wizard's view of a repository branch.
type RepositoryDetection struct {
	detect.Result
	Repository    string   `json:"repository"`
	Branch        string   `json:"branch"`
	DefaultBranch string   `json:"default_branch"`
	Branches      []string `json:"branches"`
	Commit        string   `json:"commit,omitempty"`
	Installation  int64    `json:"installation_id,omitempty"`
}

type detectCacheEntry struct {
	expires time.Time
	value   RepositoryDetection
}

var detectCache = struct {
	sync.Mutex
	entries map[string]detectCacheEntry
}{entries: map[string]detectCacheEntry{}}

func (c *Control) detectRepository(w http.ResponseWriter, r *http.Request) {
	// Detection reads repository contents: it needs the same permission as
	// browsing repositories, not only fleet visibility.
	if err := fleetRoles.Check(r.Context(), fleetScope, "deploy.repositories"); err != nil {
		permissionError(w, err)
		return
	}
	q := r.URL.Query()
	repo, branch, directory := q.Get("repository"), q.Get("branch"), strings.Trim(q.Get("directory"), "/")
	if !githubRepositoryName.MatchString(repo) || strings.Contains(repo, "..") {
		agent.Fail(w, 400, errors.New("enter a GitHub owner/repository"))
		return
	}
	if branch != "" && !validBranchName(branch) {
		agent.Fail(w, 400, errors.New("invalid branch"))
		return
	}
	if directory != "" && (len(directory) > 200 || !sourceDirectoryName.MatchString(directory) || path.Clean(directory) != directory || strings.HasPrefix(directory, "../") || directory == "..") {
		agent.Fail(w, 400, errors.New("the repository directory must stay inside the repository"))
		return
	}
	installation := int64(0)
	if raw := q.Get("installation"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 0 {
			agent.Fail(w, 400, errors.New("invalid GitHub installation"))
			return
		}
		installation = id
	}
	out, err := c.repositoryDetection(r.Context(), repo, branch, directory, installation)
	if err != nil {
		agent.Fail(w, 409, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, 200, out)
}

var sourceDirectoryName = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

func validBranchName(b string) bool {
	return b != "" && len(b) <= 200 && !strings.HasPrefix(b, "-") && !strings.ContainsAny(b, " ~^:?*[\\\r\n\t") && !strings.Contains(b, "..") && !strings.Contains(b, "@{")
}

func (c *Control) repositoryDetection(ctx context.Context, repo, branch, directory string, installation int64) (RepositoryDetection, error) {
	if c.cfg.GitHub == nil {
		return RepositoryDetection{}, errors.New("connect GitHub first")
	}
	if installation == 0 {
		if cfg := c.githubApp(); cfg != nil {
			if id, err := c.cfg.GitHub.RepositoryInstallation(ctx, cfg.App, repo); err == nil && cfg.Installations[id] != "" {
				installation = id
			}
		}
	}
	key := fmt.Sprintf("%p|%d|%s|%s|%s", c, installation, repo, branch, directory)
	now := time.Now()
	detectCache.Lock()
	if entry, ok := detectCache.entries[key]; ok && now.Before(entry.expires) {
		detectCache.Unlock()
		return entry.value, nil
	}
	detectCache.Unlock()

	token, err := c.tokenFor(ctx, Application{Repository: repo, GitHubInstallation: installation})
	if err != nil {
		return RepositoryDetection{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()
	g := githubRepository{client: c.cfg.GitHub, token: token, repo: repo}
	out, err := g.detect(ctx, branch, directory)
	if err != nil {
		return RepositoryDetection{}, err
	}
	out.Installation = installation

	detectCache.Lock()
	defer detectCache.Unlock()
	for k, entry := range detectCache.entries {
		if !now.Before(entry.expires) {
			delete(detectCache.entries, k)
		}
	}
	for k := range detectCache.entries {
		if len(detectCache.entries) < detectCacheEntries {
			break
		}
		delete(detectCache.entries, k)
	}
	detectCache.entries[key] = detectCacheEntry{expires: now.Add(detectCacheTTL), value: out}
	return out, nil
}

// githubRepository reads one repository with one token.
type githubRepository struct {
	client *gh.Client
	token  string
	repo   string
}

func (g githubRepository) get(ctx context.Context, path string, out any) error {
	return g.client.Request(ctx, g.token, "GET", "/repos/"+g.repo+path, nil, out)
}

func githubNotFound(err error) bool {
	var api *gh.APIError
	return errors.As(err, &api) && api.Status == http.StatusNotFound
}

// escapePath escapes each element of a slash-separated path.
func escapeGitHubPath(p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func (g githubRepository) detect(ctx context.Context, branch, directory string) (RepositoryDetection, error) {
	out := RepositoryDetection{Repository: g.repo, Branches: []string{}}
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.get(ctx, "", &info); err != nil {
		return out, errors.New("repository unavailable; check that the GitHub App can read it")
	}
	out.DefaultBranch = info.DefaultBranch
	for page := 1; page <= 10; page++ {
		var batch []struct {
			Name string `json:"name"`
		}
		if err := g.get(ctx, "/branches?per_page=100&page="+strconv.Itoa(page), &batch); err != nil {
			return out, errors.New("branches unavailable; check repository access")
		}
		for _, b := range batch {
			out.Branches = append(out.Branches, b.Name)
		}
		if len(batch) < 100 {
			break
		}
	}
	if branch == "" {
		branch = out.DefaultBranch
	}
	if branch == "" {
		return out, errors.New("the repository has no default branch; choose a branch")
	}
	out.Branch = branch
	var head struct {
		Commit struct {
			SHA    string `json:"sha"`
			Commit struct {
				Tree struct {
					SHA string `json:"sha"`
				} `json:"tree"`
			} `json:"commit"`
		} `json:"commit"`
	}
	if err := g.get(ctx, "/branches/"+escapeGitHubPath(branch), &head); err != nil {
		if githubNotFound(err) {
			return out, fmt.Errorf("branch %s was not found", branch)
		}
		return out, errors.New("branch unavailable; check repository access")
	}
	out.Commit = head.Commit.SHA
	src := g.source(ctx, head.Commit.SHA, head.Commit.Commit.Tree.SHA)
	result, err := detect.DetectDirectory(src, directory)
	if err != nil {
		if ctx.Err() != nil {
			return out, errors.New("repository detection timed out; retry or choose the runtime manually")
		}
		return out, errors.New("repository files could not be read; retry or choose the runtime manually")
	}
	out.Result = result
	return out, nil
}

// source returns a detection Source for one commit: the recursive tree when
// it is complete and small enough, otherwise directory-by-directory
// listings through the contents API.
func (g githubRepository) source(ctx context.Context, commit, treeSHA string) detect.Source {
	if treeSHA != "" {
		var tree struct {
			Tree []struct {
				Path string `json:"path"`
				Mode string `json:"mode"`
				Type string `json:"type"`
				SHA  string `json:"sha"`
				Size int64  `json:"size"`
			} `json:"tree"`
			Truncated bool `json:"truncated"`
		}
		if err := g.get(ctx, "/git/trees/"+url.PathEscape(treeSHA)+"?recursive=1", &tree); err == nil && !tree.Truncated && len(tree.Tree) <= detectTreeEntries {
			type blob struct {
				sha  string
				size int64
			}
			blobs := map[string]blob{}
			paths := make([]string, 0, len(tree.Tree))
			for _, entry := range tree.Tree {
				switch {
				case entry.Type == "tree":
					paths = append(paths, entry.Path+"/")
				case entry.Type == "blob" && entry.Mode != "120000":
					// Symbolic links are skipped: their blob is a target path.
					paths = append(paths, entry.Path)
					blobs[entry.Path] = blob{entry.SHA, entry.Size}
				}
			}
			return detect.Tree(paths, func(p string) ([]byte, error) {
				b, ok := blobs[p]
				if !ok {
					return nil, fs.ErrNotExist
				}
				if b.size > detect.MaxFileSize {
					return nil, detect.ErrTooLarge
				}
				var reply struct {
					Content  string `json:"content"`
					Encoding string `json:"encoding"`
				}
				if err := g.get(ctx, "/git/blobs/"+url.PathEscape(b.sha), &reply); err != nil {
					return nil, err
				}
				return decodeGitHubContent(reply.Content, reply.Encoding)
			})
		}
	}
	return contentsSource{ctx: ctx, repo: g, ref: commit}
}

func decodeGitHubContent(content, encoding string) ([]byte, error) {
	if encoding != "base64" {
		return nil, detect.ErrTooLarge
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(content, "\n", ""))
	if err != nil {
		return nil, errors.New("GitHub returned unreadable file content")
	}
	if len(data) > detect.MaxFileSize {
		return nil, detect.ErrTooLarge
	}
	return data, nil
}

// contentsSource lists and reads a large repository lazily.
type contentsSource struct {
	ctx  context.Context
	repo githubRepository
	ref  string
}

func (s contentsSource) path(p string) string {
	location := "/contents"
	if p = strings.Trim(p, "/"); p != "" {
		location += "/" + escapeGitHubPath(p)
	}
	return location + "?ref=" + url.QueryEscape(s.ref)
}

func (s contentsSource) List(dir string) ([]string, error) {
	var raw json.RawMessage
	if err := s.repo.get(s.ctx, s.path(dir), &raw); err != nil {
		if githubNotFound(err) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	var entries []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &entries) != nil {
		return nil, fs.ErrNotExist // a file, not a directory
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		switch e.Type {
		case "dir":
			names = append(names, e.Name+"/")
		case "file":
			names = append(names, e.Name)
		}
	}
	return names, nil
}

func (s contentsSource) Read(p string) ([]byte, error) {
	var reply struct {
		Type     string `json:"type"`
		Size     int64  `json:"size"`
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := s.repo.get(s.ctx, s.path(p), &reply); err != nil {
		if githubNotFound(err) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	if reply.Type != "file" {
		return nil, fs.ErrNotExist
	}
	if reply.Size > detect.MaxFileSize {
		return nil, detect.ErrTooLarge
	}
	return decodeGitHubContent(reply.Content, reply.Encoding)
}
