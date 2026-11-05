package agent

import (
	"encoding/hex"
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
	Management           *ManagementCapabilities `json:"management_capabilities,omitempty"`
	Astro                *AstroConfig            `json:"astro,omitempty"`
	Build                BuildConfig             `json:"build,omitempty,omitzero"`
	Native               *NativeApplication      `json:"native,omitempty"`
	Runtime              string                  `json:"runtime,omitempty"`
	Rails                *RailsConfig            `json:"rails,omitempty"`
	RuntimeProfile       string                  `json:"runtime_profile,omitempty"`
	StackManifest        *StackManifest          `json:"-"`
	StackSelection       *StackSelectionSnapshot `json:"-"`
	BindingNetworks      map[string]string       `json:"-"`
	DatabaseBudgets      []StackBudget           `json:"-"`
	UseRecordedSelection bool                    `json:"-"`
	domainCheckKey       string
	AdditionalDomains    []string          `json:"additional_domains,omitempty"`
	DomainStatuses       []DomainStatus    `json:"domain_statuses,omitempty"`
	ResourceID           string            `json:"resource_id,omitempty"` // agent-owned incarnation; separates reused app IDs from retained data
	CustomerHosts        bool              `json:"customer_hosts"`
	Stopped              bool              `json:"stopped"`
	DefaultsApplied      bool              `json:"defaults_applied,omitempty"`
	EnvSources           map[string]string `json:"env_sources,omitempty"`
	EnvKinds             map[string]string `json:"env_kinds,omitempty"`
	StorageVolume        string            `json:"-"`
	Preview              bool              `json:"preview,omitempty"`
	PullRequest          int               `json:"pull_request,omitempty"`
	Restoring            bool              `json:"restoring,omitempty"`
	Maintenance          Maintenance       `json:"maintenance"`
	DomainStatus         *DomainStatus     `json:"domain_status,omitempty"`
	Bindings             map[string]string `json:"database_bindings,omitempty"`
	BackupBeforeDeploy   *bool             `json:"backup_before_deploy,omitempty"`
	Networks             []string          `json:"-"`
	Network              string            `json:"-"`
	Retiring             bool              `json:"retiring,omitempty"`
	ID                   string            `json:"id"`
	Repository           string            `json:"repository"` // github owner/name, never a credential-bearing URL
	Branch               string            `json:"branch"`
	Domain               string            `json:"domain"`
	Env                  map[string]string `json:"env,omitempty"`
	Current              *Release          `json:"current,omitempty"`
	Previous             *Release          `json:"previous,omitempty"`
}

const (
	EnvironmentSetting    = "setting"
	EnvironmentCredential = "credential"
)

func environmentKind(key, value string) string {
	upper := strings.ToUpper(key)
	for _, part := range strings.FieldsFunc(upper, func(r rune) bool { return r == '_' || r == '-' }) {
		switch part {
		case "KEY", "SECRET", "TOKEN", "PASSWORD", "PASS", "CREDENTIAL", "CREDENTIALS", "PRIVATE", "AUTH", "AUTHORIZATION", "AUTHENTICATION", "JWT", "PAT", "BEARER", "SIGNATURE":
			return EnvironmentCredential
		}
	}
	compact := strings.NewReplacer("_", "", "-", "").Replace(upper)
	for _, part := range []string{"PASSWORD", "PASSPHRASE", "APIKEY", "ACCESSTOKEN", "CLIENTSECRET", "CREDENTIAL", "PRIVATEKEY"} {
		if strings.Contains(compact, part) {
			return EnvironmentCredential
		}
	}
	if strings.Contains(value, "-----BEGIN") && strings.Contains(value, "PRIVATE KEY-----") {
		return EnvironmentCredential
	}
	if parsed, err := url.Parse(value); err == nil {
		if parsed.User != nil {
			return EnvironmentCredential
		}
		for field, values := range parsed.Query() {
			switch strings.ToLower(field) {
			case "token", "access_token", "api_key", "apikey", "signature", "sig", "password", "secret", "credential":
				if len(values) > 0 && values[0] != "" {
					return EnvironmentCredential
				}
			}
		}
	}
	return EnvironmentSetting
}

func normalizeEnvironmentKinds(env map[string]string, kinds map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for key, value := range env {
		kind := kinds[key]
		if key == "LIDZA_MASTER_KEY" {
			out[key] = EnvironmentCredential
			continue
		}
		if kind != EnvironmentSetting && kind != EnvironmentCredential {
			kind = environmentKind(key, value)
		}
		out[key] = kind
	}
	return out
}

type Release struct {
	HealthHost    string        `json:"health_host,omitempty"`
	HealthPath    string        `json:"health_path,omitempty"`
	Stack         *StackRelease `json:"stack,omitempty"`
	CommitMessage string        `json:"commit_message,omitempty"`
	DatabaseIDs   []string      `json:"database_ids,omitempty"`
	Image         string        `json:"image,omitempty"`
	ID            string        `json:"id"`
	Commit        string        `json:"commit"`
	Container     string        `json:"container"`
	Port          string        `json:"port"`
	Created       time.Time     `json:"created"`
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
	resume   bool              // internal start request; never accepted from JSON
	Defaults map[string]string `json:"application_defaults,omitempty"`
	// DefaultsRuntime names the runtime whose workspace defaults Defaults
	// carries; empty means the Līdza (Go) set.
	DefaultsRuntime  string `json:"application_defaults_runtime,omitempty"`
	CredentialURL    string `json:"credential_url,omitempty"`
	CredentialTicket string `json:"credential_ticket,omitempty"`
	CredentialServer string `json:"credential_server,omitempty"`
	Token            string `json:"github_token,omitempty"`
	Key              string `json:"key,omitempty"`
}

// diskState is decoded with encoding/json, which ignores unknown keys. Keys of
// the retired native Rails layer (native_serving, native_managed_*,
// native_worker_*, native_runtime_candidates, native_prepared,
// native_retirements) may still be present in existing state files; they are
// dropped on load and never written again.
type diskState struct {
	NativeDatabases   map[string]NativeDatabaseBinding   `json:"native_databases,omitempty"`
	NativeIngress     map[string]NativeIngressRoute      `json:"native_ingress,omitempty"`
	NativeSettings    map[string]NativeSettingsOverride  `json:"native_settings,omitempty"`
	NativeReleases    map[string]NativeReleaseHistory    `json:"native_releases,omitempty"`
	NativeTransitions map[string]NativeReleaseTransition `json:"native_transitions,omitempty"`
	NativeBindings    map[string]map[string]string       `json:"native_bindings,omitempty"`
	NativeApps        map[string]NativeApplication       `json:"native_apps,omitempty"`
	GatewayPKI        map[string]gatewayPKI              `json:"gateway_pki,omitempty"`
	StackSelections   map[string]*StackSelectionSnapshot `json:"stack_selections,omitempty"`
	StackVolumes      map[string][]string                `json:"stack_volumes,omitempty"`
	StackTransitions  map[string]StackTransition         `json:"stack_transitions,omitempty"`
	Maintenance       *MaintenanceState                  `json:"maintenance,omitempty"`
	ClickHouses       map[string]ClickHouseResource      `json:"clickhouses,omitempty"`
	RetiredAppIDs     map[string]bool                    `json:"retired_app_ids,omitempty"`
	RetainedResources map[string]RetainedResource        `json:"retained_resources,omitempty"`
	StorageVolumes    map[string]string                  `json:"storage_volumes,omitempty"`
	Caches            map[string]CacheResource           `json:"caches,omitempty"`
	ErrorReporting    *ErrorReporting                    `json:"error_reporting,omitempty"`
	ErrorOutbox       []ConsoleError                     `json:"error_outbox,omitempty"`
	ErrorCursors      map[string]ConsoleCursor           `json:"error_cursors,omitempty"`
	Tasks             map[string]Task                    `json:"tasks,omitempty"`
	BindingsVersion   int                                `json:"bindings_version"`
	Databases         map[string]Database                `json:"databases,omitempty"`
	BackupStorage     *storage.Config                    `json:"backup_storage,omitempty"`
	Apps              map[string]App                     `json:"apps"`
	Deployments       []Deployment                       `json:"deployments"`

	// NativeCreated holds the release policies of applications this agent
	// created (POST /v1/native/apps); reviewed configuration takes
	// precedence for the same id.
	NativeCreated map[string]NativeReleasePolicy `json:"native_created,omitempty"`
	// NativeCreatedSpecs keeps each created application's request without
	// its environment or external database URL, so the application can be
	// recreated elsewhere with the same processes (move.go).
	NativeCreatedSpecs map[string]NativeCreateRequest `json:"native_created_specs,omitempty"`

	// NativeHostData records the host PostgreSQL/Redis provisioned for each
	// created application, with individually sealed passwords
	// (native_hostdata.go).
	NativeHostData map[string]NativeHostDataRecord `json:"native_host_data,omitempty"`

	// Host operating-system management: update/reboot policy with its job
	// history, and the managed firewall (host_updates.go, host_firewall.go).
	HostUpdates  *HostUpdatesState  `json:"host_updates,omitempty"`
	HostFirewall *HostFirewallState `json:"host_firewall,omitempty"`
}

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var domainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (a App) Validate() error {
	if a.Native != nil {
		return errors.New("register native services through the dedicated native application API")
	}
	if err := validateApplicationRuntime(a); err != nil {
		return err
	}
	if err := a.validateRuntime(); err != nil {
		return err
	}
	if a.RuntimeProfile != "" {
		if _, ok := profileByID(a.RuntimeProfile); !ok {
			return errors.New("unknown runtime profile")
		}
		if a.Preview {
			return errors.New("stack profiles are unavailable for previews")
		}
	}
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
	if len(a.AdditionalDomains) > 20 {
		return errors.New("at most 20 additional domains per application")
	}
	seen := map[string]bool{}
	for _, domain := range a.Hostnames() {
		if err := validateHostname(domain); err != nil {
			return fmt.Errorf("%s: %w", domain, err)
		}
		if seen[domain] {
			return errors.New("duplicate domain: " + domain)
		}
		seen[domain] = true
	}
	if len(a.Env) > 100 {
		return errors.New("at most 100 environment variables")
	}
	for k, v := range a.Env {
		if k == "LIDZA_MASTER_KEY" && v != "" {
			decoded, err := hex.DecodeString(v)
			if err != nil || len(decoded) != 32 {
				return errors.New("LIDZA_MASTER_KEY must contain exactly 64 hexadecimal characters")
			}
		}
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

// Hostnames are explicit routing and certificate assignments, distinct from
// application-approved customer hosts. The primary remains the canonical URL.
func (a App) Hostnames() []string { return append([]string{a.Domain}, a.AdditionalDomains...) }
func (a App) HasDomain(host string) bool {
	for _, domain := range a.Hostnames() {
		if domain == host {
			return true
		}
	}
	return false
}
func (a App) DomainsOverlap(other App) bool {
	for _, domain := range a.Hostnames() {
		if other.HasDomain(domain) {
			return true
		}
	}
	return false
}
func validateHostname(domain string) error {
	if !domainPattern.MatchString(domain) || !strings.Contains(domain, ".") || net.ParseIP(domain) != nil || strings.Contains(domain, "..") {
		return errors.New("invalid domain")
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return errors.New("invalid DNS label")
		}
	}
	return nil
}
