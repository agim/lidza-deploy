package agent

import (
	"context"
	"errors"
)

// Creating native applications. This file is the shared contract between
// application creation (native_create*.go), data provisioning
// (native_hostdata*.go), repository detection (internal/detect) and the
// control panel. Types here are stable; function bodies marked "provided by"
// are implemented in the named files.

// NativeCreateRequest asks an agent to create a new release-managed
// application on its host: an isolated account, the application root,
// environment, data services, service units and the web route. The first
// release is a normal deploy started afterwards.
type NativeCreateRequest struct {
	ID              string `json:"id"`         // lowercase letters, digits and dashes
	Repository      string `json:"repository"` // owner/name
	Branch          string `json:"branch"`
	Runtime         string `json:"runtime"` // go, rails, node, static
	SourceDirectory string `json:"source_directory,omitempty"`
	// StaticOutput is the build output directory a static site publishes.
	StaticOutput      string              `json:"static_output,omitempty"`
	Domain            string              `json:"domain"`
	AdditionalDomains []string            `json:"additional_domains,omitempty"`
	HealthPath        string              `json:"health_path,omitempty"`
	Processes         []NativeProcessSpec `json:"processes,omitempty"`
	Data              NativeDataRequest   `json:"data"`
	Env               map[string]string   `json:"env,omitempty"`
	EnvKinds          map[string]string   `json:"env_kinds,omitempty"`
	BuildMemoryMiB    int                 `json:"build_memory_mib,omitempty"`
	MemoryMiB         int                 `json:"memory_mib,omitempty"`  // runtime cap per process; 0 = default
	CPUPercent        int                 `json:"cpu_percent,omitempty"` // runtime CPUQuota per process; 0 = none
	AutoDeploy        bool                `json:"auto_deploy,omitempty"`
}

// NativeProcessSpec is one long-running process. Exactly one process is the
// web process; it listens on the loopback port the agent assigns in PORT.
type NativeProcessSpec struct {
	Name    string `json:"name"`    // web, worker, jobs, ... (unit suffix)
	Command string `json:"command"` // run from <root>/current, e.g. "bin/rails server"
	Web     bool   `json:"web,omitempty"`
}

// NativeDataRequest describes the data services a new application needs.
type NativeDataRequest struct {
	Postgres            bool   `json:"postgres,omitempty"`
	Redis               bool   `json:"redis,omitempty"`
	ExternalDatabaseURL string `json:"external_database_url,omitempty"`
	BackupHours         int    `json:"backup_hours,omitempty"`
	Offsite             bool   `json:"offsite,omitempty"`
}

// NativeDataGrant is what provisioning hands back: environment values to add
// (DATABASE_URL, REDIS_URL) and the protected database resource ids that the
// pre-release backup gate covers.
type NativeDataGrant struct {
	Env       map[string]string `json:"-"`
	Databases []string          `json:"databases,omitempty"`
}

// NativeAppAccount is the isolated system account one application runs as.
type NativeAppAccount struct {
	User string // e.g. app-<id>
	UID  int
	GID  int
	Home string // the application root's parent directory for the account
	Root string // application root (releases/current/shared)
}

var errNativeCreateUnavailable = errors.New("native application creation is not available on this agent")

// ensureNativeAppAccount creates (or returns) the isolated system account and
// application root for id. Provided by native_create_account.go.
var ensureNativeAppAccount = func(ctx context.Context, m *Manager, id string) (NativeAppAccount, error) {
	return NativeAppAccount{}, errNativeCreateUnavailable
}

// provisionNativeData creates the application's PostgreSQL database and role
// and its Redis instance on this host, registers them for backups and returns
// the environment to add. Provided by native_hostdata.go.
var provisionNativeData = func(ctx context.Context, m *Manager, id string, account NativeAppAccount, req NativeDataRequest) (NativeDataGrant, error) {
	return NativeDataGrant{}, errNativeCreateUnavailable
}

// releaseNativeData removes the application's provisioned data services after
// a final backup. Provided by native_hostdata.go.
var releaseNativeData = func(ctx context.Context, m *Manager, id string) error {
	return errNativeCreateUnavailable
}

// ensureDocker installs and starts the container runtime on demand, for
// applications that the wizard deploys as containers. Provided by
// host_docker.go.
var ensureDocker = func(ctx context.Context, m *Manager) error {
	return errNativeCreateUnavailable
}
