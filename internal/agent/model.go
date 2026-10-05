package agent

import (
	"errors"
	"fmt"
	"github.com/agim/lidza/packs/storage"
	"net"
	"regexp"
	"strings"
	"time"
)

type App struct {
	Preview            bool              `json:"preview,omitempty"`
	PullRequest        int               `json:"pull_request,omitempty"`
	Restoring          bool              `json:"restoring,omitempty"`
	Maintenance        Maintenance       `json:"maintenance"`
	DomainStatus       *DomainStatus     `json:"domain_status,omitempty"`
	Bindings           map[string]string `json:"database_bindings,omitempty"`
	BackupBeforeDeploy *bool             `json:"backup_before_deploy,omitempty"`
	Networks           []string          `json:"-"`
	Network            string            `json:"-"`
	Retiring           bool              `json:"retiring,omitempty"`
	ID                 string            `json:"id"`
	Repository         string            `json:"repository"` // github owner/name, never a credential-bearing URL
	Branch             string            `json:"branch"`
	Domain             string            `json:"domain"`
	Env                map[string]string `json:"env,omitempty"`
	Current            *Release          `json:"current,omitempty"`
	Previous           *Release          `json:"previous,omitempty"`
}
type Release struct {
	CommitMessage string    `json:"commit_message,omitempty"`
	DatabaseIDs   []string  `json:"database_ids,omitempty"`
	Image         string    `json:"image,omitempty"`
	ID            string    `json:"id"`
	Commit        string    `json:"commit"`
	Container     string    `json:"container"`
	Port          string    `json:"port"`
	Created       time.Time `json:"created"`
}
type Deployment struct {
	Log           string     `json:"log,omitempty"`
	Started       *time.Time `json:"started,omitempty"`
	Duration      float64    `json:"duration_seconds"`
	Branch        string     `json:"branch,omitempty"`
	Domain        string     `json:"domain,omitempty"`
	Changes       []string   `json:"changes,omitempty"`
	CommitMessage string     `json:"commit_message,omitempty"`
	Kind          string     `json:"kind,omitempty"`
	ID            string     `json:"id"`
	AppID         string     `json:"app_id"`
	Status        string     `json:"status"`
	Commit        string     `json:"commit,omitempty"`
	Error         string     `json:"error,omitempty"`
	Created       time.Time  `json:"created"`
	Finished      *time.Time `json:"finished,omitempty"`
	Key           string     `json:"key,omitempty"`
}
type DeployRequest struct {
	CredentialURL    string `json:"credential_url,omitempty"`
	CredentialTicket string `json:"credential_ticket,omitempty"`
	CredentialServer string `json:"credential_server,omitempty"`
	Token            string `json:"github_token,omitempty"`
	Key              string `json:"key,omitempty"`
}
type diskState struct {
	Tasks           map[string]Task     `json:"tasks,omitempty"`
	BindingsVersion int                 `json:"bindings_version"`
	Databases       map[string]Database `json:"databases,omitempty"`
	BackupStorage   *storage.Config     `json:"backup_storage,omitempty"`
	Apps            map[string]App      `json:"apps"`
	Deployments     []Deployment        `json:"deployments"`
}

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var domainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (a App) Validate() error {
	if a.PullRequest < 0 || a.PullRequest > 999999999 || a.PullRequest > 0 && !a.Preview {
		return errors.New("invalid preview pull request")
	}
	if !idPattern.MatchString(a.ID) {
		return errors.New("id must be a lowercase slug, up to 48 characters")
	}
	if !repoPattern.MatchString(a.Repository) || strings.Contains(a.Repository, "..") {
		return errors.New("repository must be a GitHub owner/name")
	}
	if a.Branch == "" || len(a.Branch) > 200 || strings.HasPrefix(a.Branch, "-") || strings.ContainsAny(a.Branch, " ~^:?*[\\\r\n\t") || strings.Contains(a.Branch, "..") || strings.Contains(a.Branch, "@{") {
		return errors.New("invalid branch")
	}
	if !domainPattern.MatchString(a.Domain) || !strings.Contains(a.Domain, ".") || net.ParseIP(a.Domain) != nil || strings.Contains(a.Domain, "..") {
		return errors.New("invalid domain")
	}
	for _, label := range strings.Split(a.Domain, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return errors.New("invalid DNS label")
		}
	}
	if len(a.Env) > 100 {
		return errors.New("at most 100 environment variables")
	}
	for k, v := range a.Env {
		if !envPattern.MatchString(k) || strings.ContainsAny(v, "\r\n\x00") {
			return fmt.Errorf("invalid environment variable %q", k)
		}
		if strings.HasPrefix(k, "LIDZA_") && k != "LIDZA_MASTER_KEY" {
			return fmt.Errorf("%s is managed by the agent", k)
		}
	}
	return nil
}
