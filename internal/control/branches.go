package control

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/agim/lidza-deploy/internal/agent"
)

var githubRepositoryName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func (c *Control) branches(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repository")
	if !githubRepositoryName.MatchString(repo) || strings.HasPrefix(repo, "../") || strings.HasPrefix(repo, "./") || strings.HasSuffix(repo, "/..") || strings.HasSuffix(repo, "/.") {
		agent.Fail(w, 400, errors.New("enter a GitHub owner/repository"))
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("installation"), 10, 64)
	if r.URL.Query().Get("installation") == "" {
		id = 0
		err = nil
	}
	if err != nil || id < 0 {
		agent.Fail(w, 400, errors.New("invalid GitHub installation"))
		return
	}
	if c.cfg.GitHub == nil {
		agent.Fail(w, 409, errors.New("connect GitHub first"))
		return
	}
	token, err := c.tokenFor(r.Context(), Application{Repository: repo, GitHubInstallation: id})
	if err != nil {
		agent.Fail(w, 409, err)
		return
	}
	rows := []struct {
		Name string `json:"name"`
	}{}
	for page := 1; page <= 10; page++ {
		batch := []struct {
			Name string `json:"name"`
		}{}
		if err = c.cfg.GitHub.Request(r.Context(), token, "GET", "/repos/"+repo+"/branches?per_page=100&page="+strconv.Itoa(page), nil, &batch); err != nil {
			agent.Fail(w, 409, errors.New("branches unavailable; check repository access or enter the branch manually"))
			return
		}
		rows = append(rows, batch...)
		if len(batch) < 100 {
			break
		}
	}
	agent.JSON(w, 200, rows)
}
