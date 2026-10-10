package agent

import (
	"context"
	"errors"
	"github.com/agim/lidza/packs/storage"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func (m *Manager) databaseRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/databases/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		if err := m.databaseAction(r.PathValue("id"), r.PathValue("action")); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 202, map[string]string{"status": "queued"})
	})
	mux.HandleFunc("PATCH /v1/databases/{id}/backups", func(w http.ResponseWriter, r *http.Request) {
		var p BackupPolicy
		if err := Decode(w, r, &p); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.setBackupPolicy(r.PathValue("id"), p); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "saved"})
	})
	mux.HandleFunc("GET /v1/databases", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, m.databaseViews()) })
	mux.HandleFunc("POST /v1/apps/{id}/database", func(w http.ResponseWriter, r *http.Request) {
		var in DatabaseRequest
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.configureDatabase(r.PathValue("id"), in); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 202, map[string]string{"status": "provisioning"})
	})
	mux.HandleFunc("POST /v1/apps/{id}/database/{action}", func(w http.ResponseWriter, r *http.Request) {
		if err := m.databaseAction(m.primaryDatabaseID(r.PathValue("id")), r.PathValue("action")); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 202, map[string]string{"status": "queued"})
	})
	mux.HandleFunc("PATCH /v1/apps/{id}/backups", func(w http.ResponseWriter, r *http.Request) {
		var p BackupPolicy
		if err := Decode(w, r, &p); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.setBackupPolicy(m.primaryDatabaseID(r.PathValue("id")), p); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "saved"})
	})
	mux.HandleFunc("PUT /v1/backup-storage", func(w http.ResponseWriter, r *http.Request) {
		var cfg *storage.Config
		if err := Decode(w, r, &cfg); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.setBackupStorage(cfg); err != nil {
			Fail(w, 400, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "saved"})
	})
	mux.HandleFunc("GET /v1/apps/{id}/backups/{backup}", func(w http.ResponseWriter, r *http.Request) {
		id, key := r.PathValue("id"), r.PathValue("backup")
		m.mu.Lock()
		d, ok := m.data.Databases[id]
		valid := false
		for _, b := range d.Backups {
			if b.ID == key {
				valid = true
			}
		}
		m.mu.Unlock()
		if !ok || !valid {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(filepath.Join(m.backupDir(id), key+".dump"))
		if err != nil {
			Fail(w, 404, errors.New("backup file unavailable"))
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			Fail(w, 500, errors.New("backup file unavailable"))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+id+"-"+key+`.dump"`)
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Minute))
		http.ServeContent(w, r, info.Name(), info.ModTime(), f)
	})
	mux.HandleFunc("GET /v1/app-health", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, m.applicationHealth(r.Context())) })
}

type AppHealth struct {
	AppID   string `json:"app_id"`
	Release string `json:"release"`
	State   string `json:"state"`
}

func (m *Manager) applicationHealth(parent context.Context) []AppHealth {
	ctx, cancel := context.WithTimeout(parent, 4*time.Second)
	defer cancel()
	apps := m.Apps()
	out := make([]AppHealth, len(apps))
	slots := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, a := range apps {
		out[i] = AppHealth{AppID: a.ID, State: "not_deployed"}
		if a.Stopped {
			out[i].State = "stopped"
			continue
		}
		if a.Current == nil || a.Retiring {
			continue
		}
		out[i].Release = a.Current.ID
		out[i].State = "unknown"
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			probe, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			err := m.runtime.Ready(probe, a.Current)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				out[i].State = "unhealthy"
			} else {
				out[i].State = "healthy"
			}
		}()
	}
	wg.Wait()
	return out
}

func (m *Manager) primaryDatabaseID(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if db := m.data.Apps[id].Bindings["DATABASE_URL"]; db != "" {
		return db
	}
	return id
}
