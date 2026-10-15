package agent

import (
	"errors"
	"fmt"
	"github.com/agim/lidza/packs/storage"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type App struct {
	ResourceID         string            `json:"resource_id,omitempty"` // agent-owned incarnation; separates reused app IDs from retained data
	CustomerHosts      bool              `json:"customer_hosts"`
	Stopped            bool              `json:"stopped"`
	DefaultsApplied    bool              `json:"defaults_applied,omitempty"`
	EnvSources         map[string]string `json:"env_sources,omitempty"`
	EnvKinds           map[string]string `json:"env_kinds,omitempty"`
	StorageVolume      string            `json:"-"`
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

const (
	EnvironmentSetting    = "setting"
	EnvironmentCredential = "credential"
)

func environmentKind(key, value string) string {
	upper := strings.ToUpper(key)
	for _, part := range strings.FieldsFunc(upper, func(r rune) bool { return r == '_' || r == '-' }) {
		switch part {
		case "KEY", "SECRET", "TOKEN", "PASSWORD", "PASS", "CREDENTIAL", "PRIVATE":
			return EnvironmentCredential
		}
	}
	if environmentURLKey(key) {
		if parsed, err := url.Parse(value); err == nil && parsed.User != nil {
			if _, present := parsed.User.Password(); present {
				return EnvironmentCredential
			}
		}
	}
	return EnvironmentSetting
}

func normalizeEnvironmentKinds(env map[string]string, kinds map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for key, value := range env {
		kind := kinds[key]
		if kind != EnvironmentSetting && kind != EnvironmentCredential {
			kind = environmentKind(key, value)
		}
		out[key] = kind
	}
	return out
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
	resume           bool              // internal start request; never accepted from JSON
	Defaults         map[string]string `json:"application_defaults,omitempty"`
	CredentialURL    string            `json:"credential_url,omitempty"`
	CredentialTicket string            `json:"credential_ticket,omitempty"`
	CredentialServer string            `json:"credential_server,omitempty"`
	Token            string            `json:"github_token,omitempty"`
	Key              string            `json:"key,omitempty"`
}
type diskState struct {
	Maintenance       *MaintenanceState             `json:"maintenance,omitempty"`
	ClickHouses       map[string]ClickHouseResource `json:"clickhouses,omitempty"`
	RetiredAppIDs     map[string]bool               `json:"retired_app_ids,omitempty"`
	RetainedResources map[string]RetainedResource   `json:"retained_resources,omitempty"`
	StorageVolumes    map[string]string             `json:"storage_volumes,omitempty"`
	Caches            map[string]CacheResource      `json:"caches,omitempty"`
	ErrorReporting    *ErrorReporting               `json:"error_reporting,omitempty"`
	ErrorOutbox       []ConsoleError                `json:"error_outbox,omitempty"`
	ErrorCursors      map[string]ConsoleCursor      `json:"error_cursors,omitempty"`
	Tasks             map[string]Task               `json:"tasks,omitempty"`
	BindingsVersion   int                           `json:"bindings_version"`
	Databases         map[string]Database           `json:"databases,omitempty"`
	BackupStorage     *storage.Config               `json:"backup_storage,omitempty"`
	Apps              map[string]App                `json:"apps"`
	Deployments       []Deployment                  `json:"deployments"`
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
		if environmentURLKey(k) && !validEnvironmentURL(v) {
			return fmt.Errorf("%s must be an absolute URL, not an email address or other text", k)
		}
	}
	for k, kind := range a.EnvKinds {
		if _, ok := a.Env[k]; !ok {
			return fmt.Errorf("environment classification references missing variable %q", k)
		}
		if kind != EnvironmentSetting && kind != EnvironmentCredential {
			return fmt.Errorf("invalid environment classification for %q", k)
		}
	}
	return nil
}

func environmentURLKey(key string) bool {
	upper := strings.ToUpper(key)
	return strings.HasSuffix(upper, "_URL") || strings.HasSuffix(upper, "_BASE_URL")
}

func validEnvironmentURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme != "" && u.Host != "" && strings.Contains(value, "://")
}
