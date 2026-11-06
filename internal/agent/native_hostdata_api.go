package agent

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// NativeHostDataView is the panel's Data & Storage summary for one created
// application. It never contains a password or connection URL.
type NativeHostDataView struct {
	AppID      string                    `json:"app_id"`
	Postgres   *NativeHostPostgresView   `json:"postgres,omitempty"`
	Redis      *NativeHostRedisView      `json:"redis,omitempty"`
	ClickHouse *NativeHostClickHouseView `json:"clickhouse,omitempty"`
}

type NativeHostPostgresView struct {
	Engine        string       `json:"engine"` // postgresql
	Version       int          `json:"version"`
	Cluster       string       `json:"cluster"`
	Port          int          `json:"port"`
	Database      string       `json:"database"`
	Role          string       `json:"role"`
	Resource      string       `json:"resource"`
	Ready         bool         `json:"ready"`
	SizeBytes     int64        `json:"size_bytes,omitempty"`
	Backup        BackupPolicy `json:"backup"`
	NextBackup    time.Time    `json:"next_backup,omitzero"`
	LastBackup    time.Time    `json:"last_backup,omitzero"`
	LastOffsite   time.Time    `json:"last_offsite_backup,omitzero"`
	Backups       int          `json:"backups"`
	BackupError   string       `json:"backup_error,omitempty"`
	MeasureError  string       `json:"measure_error,omitempty"`
	BackupCovered bool         `json:"backup_covered"`
}

type NativeHostRedisView struct {
	Engine          string `json:"engine"` // redis
	Unit            string `json:"unit"`
	Active          string `json:"active"`
	Ready           bool   `json:"ready"`
	Socket          string `json:"socket"`
	UsedMemoryBytes int64  `json:"used_memory_bytes,omitempty"`
	MaxMemoryBytes  int64  `json:"max_memory_bytes"`
	Persistence     string `json:"persistence"`
	Backup          string `json:"backup"`
	MeasureError    string `json:"measure_error,omitempty"`
}

func (m *Manager) nativeHostDataRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/native/apps/{id}/data", func(w http.ResponseWriter, r *http.Request) {
		view, err := m.nativeHostDataView(r.Context(), r.PathValue("id"))
		if err != nil {
			Fail(w, 404, err)
			return
		}
		JSON(w, 200, view)
	})
}

// nativeHostDataView reads state and measures sizes with short timeouts: a
// database size query as postgres and INFO memory over the Redis socket.
// Neither starts a stopped service.
func (m *Manager) nativeHostDataView(ctx context.Context, id string) (NativeHostDataView, error) {
	view := NativeHostDataView{AppID: id}
	if !idPattern.MatchString(id) {
		return view, errors.New("invalid application id")
	}
	record, ok := m.hostDataRecord(id)
	if !ok {
		return view, errors.New("no data services are provisioned for this application")
	}
	h := nativeHostDataHost(m)
	if pg := record.Postgres; pg != nil {
		v := &NativeHostPostgresView{Engine: "postgresql", Version: pg.Major, Cluster: pg.Cluster, Port: pg.Port, Database: pg.Name, Role: pg.Name, Resource: pg.Resource, Ready: pg.Ready}
		m.mu.Lock()
		d, exists := m.data.Databases[pg.Resource]
		v.BackupCovered = m.nativeDatabaseInUseLocked(pg.Resource)
		m.mu.Unlock()
		if exists {
			v.Backup, v.NextBackup, v.Backups, v.BackupError = d.Backup, d.NextBackup, len(d.Backups), d.Error
			for _, b := range d.Backups {
				if b.Created.After(v.LastBackup) {
					v.LastBackup = b.Created
				}
				if b.Offsite && b.Created.After(v.LastOffsite) {
					v.LastOffsite = b.Created
				}
			}
		}
		if pg.Ready {
			measure, cancel := context.WithTimeout(ctx, 5*time.Second)
			literal, _ := pgQuoteLiteral(pg.Name)
			out, err := h.psql(measure, pg.Port, "SELECT pg_catalog.pg_database_size("+literal+");\n")
			cancel()
			if size, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil && perr == nil {
				v.SizeBytes = size
			} else {
				v.MeasureError = "database size is unavailable"
			}
		}
		view.Postgres = v
	}
	if redis := record.Redis; redis != nil {
		v := &NativeHostRedisView{Engine: "redis", Unit: redis.Unit, Ready: redis.Ready, Socket: redis.Socket, MaxMemoryBytes: int64(redis.MaxMemoryMiB) << 20, Persistence: "append-only file and snapshots in the application's shared/redis directory", Backup: "not separately backed up; the files stay with the application's shared directory"}
		measure, cancel := context.WithTimeout(ctx, 5*time.Second)
		active, _ := h.run(measure, "", "/usr/bin/systemctl", "is-active", redis.Unit)
		v.Active = strings.TrimSpace(active)
		if v.Active == "active" {
			if password, err := m.unsealHostSecret(redis.Password); err == nil {
				if memory, err := h.redisMemory(measure, h.path(redis.Socket), password); err == nil {
					v.UsedMemoryBytes = memory.Used
					if memory.Max > 0 {
						v.MaxMemoryBytes = memory.Max
					}
				} else {
					v.MeasureError = "Redis memory is unavailable"
				}
			}
		}
		cancel()
		view.Redis = v
	}
	if ch := record.ClickHouse; ch != nil {
		view.ClickHouse = m.nativeHostClickHouseView(ctx, id, *ch)
	}
	return view, nil
}
