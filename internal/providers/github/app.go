package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"golang.org/x/sync/singleflight"
)

// App credentials never leave the control panel. Installation tokens are
// obtained afresh for checkout; concurrent identical requests share one mint.
type App struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
}
type Installation struct {
	ID          int64      `json:"id"`
	AppID       int64      `json:"app_id"`
	SuspendedAt *time.Time `json:"suspended_at"`
	Account     struct {
		Login string `json:"login"`
	} `json:"account"`
	Permissions map[string]string `json:"permissions"`
}

var appMints singleflight.Group
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)

func (a App) JWT() (string, error) {
	block, _ := pem.Decode([]byte(a.PEM))
	if block == nil {
		return "", errors.New("GitHub App key is invalid")
	}
	var key *rsa.PrivateKey
	if parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = parsed
	} else {
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return "", errors.New("GitHub App key is invalid")
		}
		key, _ = parsed.(*rsa.PrivateKey)
	}
	if key == nil || key.N.BitLen() < 2048 || a.ID <= 0 {
		return "", errors.New("GitHub App key or ID is invalid")
	}
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(8 * time.Minute).Unix(), "iss": strconv.FormatInt(a.ID, 10)})
	text := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(text))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", errors.New("GitHub App signing failed")
	}
	return text + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
func (c *Client) ConvertManifest(ctx context.Context, code string) (App, error) {
	var a App
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,512}$`).MatchString(code) {
		return a, errors.New("invalid manifest code")
	}
	if err := c.Request(ctx, "", "POST", "/app-manifests/"+code+"/conversions", nil, &a); err != nil {
		return a, err
	}
	if !slugPattern.MatchString(a.Slug) || a.WebhookSecret == "" {
		return App{}, errors.New("invalid GitHub App response")
	}
	_, err := a.JWT()
	if err != nil {
		return App{}, err
	}
	return a, nil
}
func (c *Client) Installation(ctx context.Context, a App, id int64) (Installation, error) {
	var in Installation
	jwt, err := a.JWT()
	if err != nil {
		return in, err
	}
	if id <= 0 {
		return in, errors.New("invalid installation")
	}
	err = c.Request(ctx, jwt, "GET", fmt.Sprintf("/app/installations/%d", id), nil, &in)
	if err != nil {
		return in, err
	}
	if in.ID != id || in.AppID != a.ID || in.SuspendedAt != nil || in.Permissions["contents"] != "read" || in.Permissions["pull_requests"] != "read" {
		return in, errors.New("GitHub installation is suspended or lacks required read permissions")
	}
	return in, nil
}
func (c *Client) RepositoryInstallation(ctx context.Context, a App, repo string) (int64, error) {
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repo) {
		return 0, errors.New("invalid repository")
	}
	jwt, err := a.JWT()
	if err != nil {
		return 0, err
	}
	var in Installation
	if err = c.Request(ctx, jwt, "GET", "/repos/"+repo+"/installation", nil, &in); err != nil {
		return 0, err
	}
	if in.AppID != a.ID || in.ID <= 0 || in.SuspendedAt != nil {
		return 0, errors.New("repository installation unavailable")
	}
	return in.ID, nil
}
func (c *Client) InstallationToken(ctx context.Context, a App, id int64, repo string) (string, error) {
	// API destination is part of the key so tests/different clients cannot share tokens.
	key := fmt.Sprintf("%s:%d:%d:%s", c.api(), a.ID, id, repo)
	value, err, _ := appMints.Do(key, func() (any, error) {
		if _, err := c.Installation(ctx, a, id); err != nil {
			return "", err
		}
		jwt, err := a.JWT()
		if err != nil {
			return "", err
		}
		body := map[string]any{"permissions": map[string]string{"contents": "read", "pull_requests": "read"}}
		if repo != "" {
			found, err := c.RepositoryInstallation(ctx, a, repo)
			if err != nil || found != id {
				return "", errors.New("repository is not in this installation")
			}
			_, name, ok := cutRepository(repo)
			if !ok {
				return "", errors.New("invalid repository")
			}
			body["repositories"] = []string{name}
		}
		var reply struct {
			Token   string    `json:"token"`
			Expires time.Time `json:"expires_at"`
		}
		if err = c.Request(ctx, jwt, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", id), body, &reply); err != nil {
			return "", err
		}
		if reply.Token == "" || !reply.Expires.After(time.Now().Add(2*time.Minute)) {
			return "", errors.New("GitHub returned an expired installation token")
		}
		return reply.Token, nil
	})
	if err != nil {
		return "", err
	}
	return value.(string), nil
}
func cutRepository(repo string) (string, string, bool) {
	for i, c := range repo {
		if c == '/' && i > 0 && i < len(repo)-1 {
			return repo[:i], repo[i+1:], true
		}
	}
	return "", "", false
}
func (c *Client) InstallationRepositories(ctx context.Context, a App, id int64, page int) ([]Repository, error) {
	token, err := c.InstallationToken(ctx, a, id, "")
	if err != nil {
		return nil, err
	}
	var out struct {
		Repositories []Repository `json:"repositories"`
	}
	err = c.Request(ctx, token, "GET", fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page), nil, &out)
	for i := range out.Repositories {
		out.Repositories[i].InstallationID = id
	}
	return out.Repositories, err
}
