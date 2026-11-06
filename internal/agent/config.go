package agent

import (
	"encoding/json"
	"errors"
	"github.com/agim/lidza-deploy/internal/platform/state"
	"log"
	"os"
	"path/filepath"
	"regexp"
)

// Adapted from monolithcms-app/agent/config.go. Configuration remains private
// and atomically replaced; registration is explicit rather than trust-on-first-use.
type Config struct {
	nativeConfigPath           string              // existing protected source file; never supplied by an API request
	Native                     *NativeHostConfig   `json:"native,omitempty"`
	ReservedDomains            []string            `json:"reserved_domains,omitempty"`
	IngressOnly                bool                `json:"ingress_only,omitempty"`
	NativeIngressTargets       map[string][]string `json:"native_ingress_targets,omitempty"`
	ServiceTLS                 *ServiceTLSConfig   `json:"service_tls,omitempty"`
	Gateway                    *GatewayConfig      `json:"gateway,omitempty"`
	TLSListen                  string              `json:"tls_listen"`
	Listen                     string              `json:"listen"`
	ProxyListen                string              `json:"proxy_listen"`
	DataDir                    string              `json:"data_dir"`
	APIKey                     string              `json:"api_key"`
	PredeploymentBackupMinutes int                 `json:"predeployment_backup_minutes,omitempty"`
	// Legacy: exact schema.table names, scoped to a database resource
	// identity, whose data backups skip. Schemas remain in dumps; no glob
	// patterns. The panel's per-database setting (backup_excluded_tables.go)
	// replaces it; this list is only a database's initial default until the
	// panel saves one.
	BackupExcludeTableData map[string][]string `json:"backup_exclude_table_data,omitempty"`
	// Exact database resource limits. An omitted resource keeps the 5 GiB default.
	BackupMaxBytes map[string]int64 `json:"backup_max_bytes,omitempty"`
	// MoveKeyColumns are the timestamp columns, in order of preference, that
	// mark a row's insertion for incremental copies of append-only tables
	// during a move (move_table_delta.go). Empty means created_at,
	// received_at, inserted_at, occurred_at, ts, timestamp.
	MoveKeyColumns []string `json:"move_key_columns,omitempty"`
}

func LoadConfig(path string) (Config, error) {
	c := Config{TLSListen: "127.0.0.1:443", Listen: "127.0.0.1:9090", ProxyListen: "127.0.0.1:8081", DataDir: "/var/lib/lidza-agent"}
	if err := state.Load(path, &c); err != nil {
		return c, err
	}
	if absolute, err := filepath.Abs(path); err == nil {
		c.nativeConfigPath = absolute
	}
	if v := os.Getenv("LIDZA_AGENT_TOKEN"); v != "" {
		c.APIKey = v
	}
	if err := checkRetiredConfigKeys(path); err != nil {
		return c, err
	}
	if c.IngressOnly && c.Native != nil {
		return c, errors.New("ingress_only cannot manage native applications")
	}
	if c.Native != nil {
		if err := c.Native.validate(); err != nil {
			return c, err
		}
	}
	if err := c.validateHostnames(); err != nil {
		return c, err
	}
	if c.IngressOnly && (!validateIngressListen(c.Listen) || !validateIngressListen(c.ProxyListen)) {
		return c, errors.New("independent ingress and health listeners must use explicit loopback addresses")
	}
	if !c.IngressOnly && len(c.APIKey) < 32 {
		return c, errors.New("agent API key must contain at least 32 characters")
	}
	if !filepath.IsAbs(c.DataDir) {
		return c, errors.New("data_dir must be absolute")
	}
	if c.PredeploymentBackupMinutes < 0 || c.PredeploymentBackupMinutes > 120 {
		return c, errors.New("predeployment_backup_minutes must be between 1 and 120, or 0 for the 10-minute default")
	}
	for database, limit := range c.BackupMaxBytes {
		minimum := maxBackupSize
		if c.Native != nil && limit < minimum {
			policy := c.Native.Databases[database]
			if policy.ArchiveDatabase != "" && validateNativeDatabasePolicy(database, policy) == nil {
				minimum = 1
			}
		}
		if !idPattern.MatchString(database) || limit < minimum || limit > maxExtendedBackupSize {
			return c, errors.New("backup_max_bytes requires exact database IDs and limits between 5 GiB and 64 GiB; standalone native archives may use a smaller positive limit")
		}
	}
	for database, tables := range c.BackupExcludeTableData {
		if database == "" || len(tables) > 100 {
			return c, errors.New("backup exclusions require a database identity and at most 100 exact tables")
		}
		for _, table := range tables {
			if !regexp.MustCompile(`^[a-z_][a-z0-9_]*\.[a-z_][a-z0-9_]*$`).MatchString(table) {
				return c, errors.New("backup exclusions must be exact lowercase schema.table names, without patterns")
			}
		}
	}
	return c, nil
}

// validateHostnames checks the hostname policies every agent role shares:
// native ingress targets and reserved hostnames.
func (c Config) validateHostnames() error {
	if err := c.validateNativeIngressTargets(); err != nil {
		return err
	}
	if err := validateReservedDomains(c.ReservedDomains); err != nil {
		return err
	}
	return nil
}

// checkRetiredConfigKeys handles keys from earlier releases. host_mode no
// longer changes behaviour and is ignored with one startup notice; every
// agent manages its host fully. native_only (an observation-only agent
// without container or host management) was removed; a configuration that
// still asks for it is refused rather than silently started as a manager.
func checkRetiredConfigKeys(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil // a missing file loads defaults, as state.Load does
	}
	var retired struct {
		HostMode   *string `json:"host_mode"`
		NativeOnly bool    `json:"native_only"`
	}
	if json.Unmarshal(raw, &retired) != nil {
		return nil // state.Load has already reported malformed JSON
	}
	if retired.NativeOnly {
		return errors.New("native_only is no longer supported: remove it from the agent configuration and run a full agent, or an ingress_only reader")
	}
	if retired.HostMode != nil {
		log.Printf("agent configuration: host_mode is no longer used and is ignored; remove it from %s", path)
	}
	return nil
}
