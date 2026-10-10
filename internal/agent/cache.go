package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	lidzacache "github.com/agim/lidza/packs/cache"
	"github.com/agim/lidza/pkg/config"
	"github.com/agim/lidza/pkg/credentials"
	"github.com/agim/lidza/pkg/pack"
)

// Cache resources are stored only in the agent's encrypted state. API views never
// expose URLs or credentials. Switching to an external service retains local data.
type CacheResource struct {
	Previous      *CacheResource `json:"previous,omitempty"`
	Mode          string         `json:"mode"`
	URL           string         `json:"url"`
	LocalPassword string         `json:"local_password,omitempty"`
	Network       string         `json:"network,omitempty"`
	Ready         bool           `json:"ready"`
	Operation     bool           `json:"operation,omitempty"`
	Error         string         `json:"error,omitempty"`
}
type CacheView struct {
	Mode      string `json:"mode"`
	Ready     bool   `json:"ready"`
	Operation bool   `json:"operation"`
	Error     string `json:"error,omitempty"`
	Managed   bool   `json:"managed"`
}
type CacheRequest struct {
	Mode string `json:"mode"`
	URL  string `json:"url,omitempty"`
}

func (r CacheRequest) Validate() error {
	if r.Mode == "local" {
		if r.URL != "" {
			return errors.New("local cache does not accept a connection URL")
		}
		return nil
	}
	if r.Mode != "external" {
		return errors.New("choose local or external cache")
	}
	u, err := url.Parse(r.URL)
	if err != nil || len(r.URL) > 4096 || strings.ContainsAny(r.URL, "\r\n\x00") || u.Hostname() == "" || u.Fragment != "" || !slices.Contains([]string{"redis", "rediss", "valkey", "valkeys"}, u.Scheme) {
		return errors.New("cache URL must use redis://, rediss://, valkey:// or valkeys://")
	}
	if u.Hostname() == "localhost" || net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback() {
		return errors.New("external cache must be reachable from application containers; localhost is not the hosting server")
	}
	if u.User == nil {
		return errors.New("external cache needs an authenticated connection URL")
	}
	password, ok := u.User.Password()
	if !ok || password == "" {
		return errors.New("external cache needs an authenticated connection URL")
	}
	return nil
}
func (m *Manager) cacheViewLocked(id string) CacheView {
	c, ok := m.data.Caches[id]
	if !ok {
		return CacheView{Mode: map[bool]string{true: "custom", false: "automatic"}[m.data.Apps[id].Env["CACHE_URL"] != ""], Ready: m.data.Apps[id].Env["CACHE_URL"] != ""}
	}
	return CacheView{Mode: c.Mode, Ready: c.Ready, Operation: c.Operation, Error: c.Error, Managed: true}
}
func (m *Manager) reserveCacheLocked(id string, r CacheRequest, automatic bool) (CacheResource, CacheResource, error) {
	a, ok := m.data.Apps[id]
	if !ok || a.Retiring || m.ctx.Err() != nil {
		return CacheResource{}, CacheResource{}, errors.New("application unavailable")
	}
	old := m.data.Caches[id]
	if old.Operation || (!automatic && (m.busy(id) || a.Restoring)) {
		return old, old, errors.New("application or cache operation is active")
	}
	next := old
	next.Previous = nil
	if old.Ready {
		next.Previous = &old
	}
	next.Mode = r.Mode
	next.Operation = true
	next.Error = ""
	next.Ready = false
	if r.Mode == "local" {
		if next.LocalPassword == "" {
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return old, old, err
			}
			next.LocalPassword = hex.EncodeToString(b)
		}
		next.Network = "lidza-cache-" + id
		next.URL = "redis://:" + next.LocalPassword + "@" + next.Network + ":6379/0"
	} else {
		next.URL = r.URL
	}
	m.data.Caches[id] = next
	if err := m.save(); err != nil {
		if old.Mode == "" {
			delete(m.data.Caches, id)
		} else {
			m.data.Caches[id] = old
		}
		return old, old, err
	}
	return next, old, nil
}
func (m *Manager) finishCache(id string, next, old CacheResource, provisionErr error, reload bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	original := m.data.Apps[id]
	a := original
	if a.Retiring || a.ID == "" {
		return errors.New("application unavailable")
	}
	next.Operation = false
	next.Previous = nil
	if provisionErr != nil {
		if old.Ready {
			next = old
		}
		next.Operation = false
		next.Error = "Cache connection/provisioning failed; check the hosting agent and service access. Previous connection is retained."
		m.data.Caches[id] = next
		if err := m.save(); err != nil {
			return err
		}
		return errors.New(next.Error)
	}
	next.Ready = true
	next.Error = ""
	a.Env = maps.Clone(a.Env)
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	a.Env["CACHE_URL"] = next.URL
	if a.Env["CACHE_PREFIX"] == "" {
		a.Env["CACHE_PREFIX"] = "lidza:" + id + ":"
	}
	err := a.Validate()
	if err == nil {
		m.data.Caches[id] = next
		m.data.Apps[id] = a
		if reload && a.Current != nil {
			_, err = m.queueLocked(a, DeployRequest{}, true, changeKeys(original, a))
		} else {
			err = m.save()
		}
	}
	if err != nil {
		m.data.Apps[id] = original
		if old.Mode != "" {
			old.Error = "Cache attachment could not be saved or reload could not be queued; previous connection retained."
			m.data.Caches[id] = old
		} else {
			next.Ready = false
			next.Error = "Cache attachment could not be saved; retry setup."
			m.data.Caches[id] = next
		}
		_ = m.save()
		return errors.New("could not persist cache attachment; previous app connection retained")
	}
	return nil
}
func (m *Manager) configureCache(id string, r CacheRequest) error {
	if err := r.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	next, old, err := m.reserveCacheLocked(id, r, false)
	if err == nil {
		m.wg.Add(1)
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, 3*time.Minute)
		defer cancel()
		err := m.provisionCache(ctx, next)
		_ = m.finishCache(id, next, old, err, true)
	}()
	return nil
}
func (m *Manager) provisionCache(ctx context.Context, c CacheResource) error {
	if m.cacheProvision != nil {
		return m.cacheProvision(ctx, c)
	}
	if c.Mode == "local" {
		return provisionLocalCache(ctx, c)
	}
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	store, err := lidzacache.NewValkey(probe, c.URL)
	if err != nil {
		return errors.New("external cache connection failed")
	}
	defer store.Close()
	return store.Ping(probe)
}
func (m *Manager) cacheRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/apps/{id}/cache", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if _, ok := m.data.Apps[r.PathValue("id")]; !ok {
			http.NotFound(w, r)
			return
		}
		JSON(w, 200, m.cacheViewLocked(r.PathValue("id")))
	})
	mux.HandleFunc("POST /v1/apps/{id}/cache", func(w http.ResponseWriter, r *http.Request) {
		var in CacheRequest
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.configureCache(r.PathValue("id"), in); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 202, map[string]string{"status": "provisioning"})
	})
}

// preflightRuntime runs in the disposable checkout before expensive compilation.
// It consumes framework production metadata and credentials without touching the
// agent's process environment or writing the app's manual master key to disk.
func (m *Manager) preflightRuntime(ctx context.Context, a App, source string) (App, error) {
	if err := regularFile(filepath.Join(source, "lidza.json"), false); err != nil {
		return a, errors.New("application requires a regular lidza.json manifest")
	}
	cfg, err := config.Load(source)
	if err != nil {
		return a, errors.New("application manifest could not be read")
	}
	effective, err := productionValues(source, cfg, a)
	if err != nil {
		return a, err
	}
	needsCache := slices.Contains(cfg.Packs, pack.OfficialPrefix+"cache")
	m.mu.Lock()
	configured := m.data.Caches[a.ID]
	m.mu.Unlock()
	if needsCache && configured.Ready && configured.Mode == "local" {
		if err := m.provisionCache(ctx, configured); err != nil {
			return a, errors.New("managed cache unavailable; check Cache settings before deploying")
		}
	}
	if needsCache && strings.TrimSpace(effective["CACHE_URL"]) == "" {
		commandDiagnostic(ctx, "cache", "No CACHE_URL configured; provisioning a private authenticated Valkey service.")
		m.mu.Lock()
		existing := m.data.Caches[a.ID]
		request := CacheRequest{Mode: "local"}
		if existing.Mode != "" {
			request.Mode = existing.Mode
			request.URL = existing.URL
			if request.Mode == "local" {
				request.URL = ""
			}
		}
		next, old, e := m.reserveCacheLocked(a.ID, request, true)
		m.mu.Unlock()
		if e != nil {
			return a, e
		}
		e = m.provisionCache(ctx, next)
		if e = m.finishCache(a.ID, next, old, e, false); e != nil {
			return a, e
		}
		m.mu.Lock()
		a.Env = maps.Clone(m.data.Apps[a.ID].Env)
		resource := m.data.Caches[a.ID]
		m.mu.Unlock()
		if resource.Mode == "local" {
			a.Networks = slices.Compact(slices.Sorted(slices.Values(append(a.Networks, resource.Network))))
		}
		effective["CACHE_URL"] = a.Env["CACHE_URL"]
		commandDiagnostic(ctx, "cache", "Valkey is ready; CACHE_URL and the application network are attached. Credentials retained for future releases.")
	}
	a, err = m.prepareAppStorage(ctx, a, cfg, effective)
	if err != nil {
		return a, err
	}
	if a.StorageVolume != "" && effective["STORAGE_PROVIDER"] == "local" {
		commandDiagnostic(ctx, "storage", "Persistent local storage is attached; files are retained across deployments and reloads.")
	}
	// Manifest defaults are non-secret production settings. Explicit app env wins.
	merged := maps.Clone(cfg.Deploy.Env)
	if merged == nil {
		merged = map[string]string{}
	}
	maps.Copy(merged, a.Env)
	a.Env = merged
	if err := productionRequirements(cfg, effective, a.StorageVolume != ""); err != nil {
		return a, err
	}
	return a, a.Validate()
}
func productionValues(source string, cfg *config.Config, a App) (map[string]string, error) {
	values := map[string]string{}
	path := filepath.Join(source, credentials.File)
	if _, err := os.Lstat(path); err == nil {
		if err = regularFile(path, false); err != nil {
			return nil, err
		}
		sealed, err := os.ReadFile(path)
		if err != nil {
			return nil, errors.New("application credentials are unavailable")
		}
		key, err := hex.DecodeString(strings.TrimSpace(a.Env["LIDZA_MASTER_KEY"]))
		if err != nil || len(key) != 32 {
			return nil, errors.New("sealed application credentials require its existing LIDZA_MASTER_KEY in Settings; this key remains manual")
		}
		plain, err := credentials.Decrypt(key, strings.TrimSpace(string(sealed)))
		if err != nil {
			return nil, errors.New("LIDZA_MASTER_KEY could not open application credentials")
		}
		raw, err := credentials.Parse(string(plain))
		if err != nil {
			return nil, errors.New("application credentials could not be parsed")
		}
		values = credentials.Resolve(raw, "production")
	} else if !os.IsNotExist(err) {
		return nil, errors.New("application credentials are unavailable")
	}
	maps.Copy(values, cfg.Deploy.Env)
	maps.Copy(values, a.Env)
	if values["APP_URL"] == "" {
		values["APP_URL"] = "https://" + a.Domain
	}
	return values, nil
}
func productionRequirements(cfg *config.Config, values map[string]string, persistentLocalStorage ...bool) error {
	var missing []string
	for _, enabled := range cfg.Packs {
		for _, official := range pack.Officials {
			if enabled != pack.OfficialPrefix+official.Name {
				continue
			}
			for _, setting := range official.Production {
				v := strings.TrimSpace(values[setting.Name])
				if enabled == "lidza/storage" && setting.Name == "STORAGE_PROVIDER" && v == "local" && len(persistentLocalStorage) > 0 && persistentLocalStorage[0] && values["STORAGE_DIR"] == appStorageMount {
					continue
				}
				if v == "" && !setting.Optional {
					missing = append(missing, setting.Name+" ("+enabled+")")
				} else if slices.Contains(setting.Dev, strings.ToLower(v)) {
					missing = append(missing, setting.Name+" (production configuration required)")
				}
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("production configuration missing or development-only: %s; configure these in application Settings before deploying", strings.Join(slices.Compact(slices.Sorted(slices.Values(missing))), ", "))
	}
	return nil
}
