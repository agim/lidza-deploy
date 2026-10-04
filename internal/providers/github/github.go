// Package github contains the control plane's GitHub repository and webhook API; OAuth lives in Līdza.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

type Client struct {
	HTTP *http.Client
	API  string
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func (c *Client) api() string {
	if c.API != "" {
		return c.API
	}
	return "https://api.github.com"
}
func (c *Client) Request(ctx context.Context, token, method, path string, body, out any) error {
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api()+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client().Do(req)
	if err != nil {
		return errors.New("GitHub API unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("GitHub API returned %d", res.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
}

type Repository struct {
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

func (c *Client) Repositories(ctx context.Context, token string, page int) ([]Repository, error) {
	var repos []Repository
	err := c.Request(ctx, token, "GET", "/user/repos?per_page=100&sort=updated&page="+strconv.Itoa(page), nil, &repos)
	return repos, err
}
func (c *Client) Hook(ctx context.Context, token, repo, callback, secret string, existing int64) (int64, error) {
	body := map[string]any{"name": "web", "active": true, "events": []string{"push", "pull_request"}, "config": map[string]string{"url": callback, "content_type": "json", "secret": secret, "insecure_ssl": "0"}}
	path := "/repos/" + repo + "/hooks"
	method := "POST"
	if existing != 0 {
		method = "PATCH"
		path += "/" + strconv.FormatInt(existing, 10)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	err := c.Request(ctx, token, method, path, body, &out)
	return out.ID, err
}
