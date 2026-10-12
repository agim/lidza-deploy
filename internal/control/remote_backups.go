package control

import (
	"errors"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/storage"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

func (c *Control) backupDestination() (storage.Config, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.BackupStorage == nil {
		return storage.Config{}, "", false
	}
	cfg := *c.data.BackupStorage
	legacy := c.data.BackupStorageLegacyPrefix
	if legacy == "" && spacesEndpoint(cfg) {
		legacy = path.Join(cfg.Bucket, cfg.Prefix)
	}
	return backupStorageConfig(cfg), legacy, true
}

func (c *Control) remoteBackups(w http.ResponseWriter, r *http.Request) {
	cfg, legacyPrefix, ok := c.backupDestination()
	if !ok {
		agent.Fail(w, 409, errors.New("configure off-site storage first"))
		return
	}
	target, err := storage.New(cfg)
	if err != nil {
		agent.Fail(w, 502, errors.New("storage configuration unavailable"))
		return
	}
	const limit = 1000
	objects, err := target.List(r.Context(), "databases/", limit)
	if err != nil {
		agent.Fail(w, 502, errors.New("cannot list remote backups; check bucket ListBucket permission, endpoint and credentials"))
		return
	}
	type remoteObject struct {
		storage.Object
		Prefix string `json:"prefix"`
		Legacy bool   `json:"legacy,omitempty"`
	}
	warning := ""
	filtered := []remoteObject{}
	for _, object := range objects {
		if validRemoteBackupKey(object.Key) {
			filtered = append(filtered, remoteObject{Object: object, Prefix: cfg.Prefix})
		}
	}
	// Older releases used path-style addressing even for bucket-specific Spaces
	// endpoints. Include objects written under that extra bucket-name folder.
	if legacyPrefix != "" && len(objects) < limit {
		legacy := cfg
		legacy.Prefix = legacyPrefix
		oldStore, _ := storage.New(legacy)
		oldObjects, listErr := oldStore.List(r.Context(), "databases/", limit-len(objects))
		if listErr != nil {
			warning = "The current prefix was checked, but the previous endpoint location could not be listed. Check ListBucket permission; additional older backups may exist."
		}
		for _, object := range oldObjects {
			if validRemoteBackupKey(object.Key) {
				filtered = append(filtered, remoteObject{Object: object, Prefix: legacy.Prefix, Legacy: true})
			}
		}
		objects = append(objects, oldObjects...)
	}
	w.Header().Set("Cache-Control", "no-store")
	agent.JSON(w, 200, map[string]any{"objects": filtered, "warning": warning, "bucket": cfg.Bucket, "prefix": cfg.Prefix, "checked": time.Now().UTC(), "limit": limit, "limit_reached": len(objects) >= limit})
}
func validRemoteBackupKey(key string) bool {
	return strings.HasPrefix(key, "databases/") && strings.HasSuffix(key, ".dump") && path.Clean(key) == key && !strings.ContainsAny(key, "\\\r\n") && len(strings.Split(key, "/")) == 3
}
func (c *Control) remoteBackupDownload(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if !validRemoteBackupKey(key) {
		agent.Fail(w, 400, errors.New("invalid backup object"))
		return
	}
	cfg, legacyPrefix, ok := c.backupDestination()
	if !ok {
		agent.Fail(w, 409, errors.New("configure off-site storage first"))
		return
	}
	if r.URL.Query().Get("legacy") == "1" {
		if legacyPrefix == "" {
			agent.Fail(w, 400, errors.New("invalid legacy location"))
			return
		}
		cfg.Prefix = legacyPrefix
	}
	target, err := storage.New(cfg)
	if err != nil {
		agent.Fail(w, 502, errors.New("storage configuration unavailable"))
		return
	}
	if _, err = target.Stat(r.Context(), key); err != nil {
		agent.Fail(w, 502, errors.New("remote backup unavailable; check object read permission"))
		return
	}
	signed, err := target.PresignGet(r.Context(), key, 5*time.Minute)
	if err != nil {
		agent.Fail(w, 502, errors.New("cannot authorize backup download"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, signed, http.StatusSeeOther)
}
func (c *Control) retryBackupUpload(w http.ResponseWriter, r *http.Request) {
	server, id, backup := r.PathValue("server"), r.PathValue("id"), r.PathValue("backup")
	if err := c.syncBackupStorage(r, server); err != nil {
		agent.Fail(w, 409, err)
		return
	}
	if err := c.agentCall(r, server, "POST", "/v1/databases/"+id+"/backups/"+backup+"/upload", nil, nil); err != nil {
		agent.Fail(w, 502, err)
		return
	}
	agent.JSON(w, 202, map[string]string{"status": "queued"})
}

// Use the framework's existing addressing option for Spaces bucket URLs.
// Non-Spaces/custom endpoints retain their configured addressing mode.
func spacesBucketEndpoint(cfg storage.Config) bool {
	u, err := url.Parse(cfg.Endpoint)
	return err == nil && strings.HasSuffix(u.Hostname(), ".digitaloceanspaces.com") && strings.HasPrefix(u.Hostname(), cfg.Bucket+".")
}
func backupStorageConfig(cfg storage.Config) storage.Config {
	if spacesBucketEndpoint(cfg) {
		u, _ := url.Parse(cfg.Endpoint)
		u.Host = strings.TrimPrefix(u.Host, cfg.Bucket+".")
		cfg.Endpoint = u.String()
		value := false
		cfg.PathStyle = &value
	}
	return cfg
}

func spacesEndpoint(cfg storage.Config) bool {
	u, err := url.Parse(cfg.Endpoint)
	return err == nil && strings.HasSuffix(u.Hostname(), ".digitaloceanspaces.com") && !strings.Contains(u.Hostname(), ".cdn.")
}
