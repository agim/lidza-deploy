package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/movetunnel"
)

const (
	moveTestToken     = "move-fixture-agent-token-0123456789"
	moveTestCommit    = "0123456789abcdef0123456789abcdef01234567"
	moveTestSecret    = "runtime-secret-value"
	moveTestSourceIP  = "198.51.100.7"
	moveTestTargetIP  = "203.0.113.9"
	moveTestTargetIP6 = "2001:db8::9"
)

// moveFake is one agent: a move source, a destination, or both.
type moveFake struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex
	// mux takes further routes (move_clickhouse_test.go); streams the
	// upgraded tunnel streams, served without the fake's lock since they
	// live long (move_tunnel_test.go).
	mux     *http.ServeMux
	streams *http.ServeMux

	calls []string

	// Source side.
	source         *agent.MoveSource
	env            agent.MoveEnvironment
	tables         map[string]int
	dumps          map[string][]byte // database → dump content
	backups        map[string][]agent.BackupRecord
	corrupt        bool
	maintenance    []bool
	provisionError string // a requested database fails its provisioning
	provisions     int    // POST /v1/databases/{id}/provision calls
	nativeSource   *agent.NativeStatus
	memory         []uint64 // server health: total and available bytes

	// Destination side.
	host       agent.MoveHost
	apps       []agent.App
	settings   *agent.Settings
	created    *agent.NativeCreateRequest
	native     *agent.NativeStatus
	targetEnv  agent.MoveEnvironment
	patches    []agent.SettingsPatch
	databases  map[string]*agent.DatabaseView
	restores   map[string][][]byte
	keeps      []string // the ?keep= of each base restore
	failKeep   bool     // refuse a restore that keeps tables
	deploys    []map[string]any
	deployment agent.Deployment
	holdDeploy bool
	deleted    []string
	unitCalls  []string
	// removals are the bodies of DELETE /v1/apps/{id} and
	// DELETE /v1/native/apps/{id}, by URL; retained lists the retained
	// resources, which a removal without delete_data adds to, and
	// retainedRemovals the removal requests of retained resources. A
	// retained row marked removing disappears at the next listing.
	removals         map[string]agent.AppRemoveRequest
	retained         []agent.RetainedResourceView
	retainedRemovals []agent.RetainedRemoveRequest

	// Files, source side: archive bytes by set (empty: an empty set) and
	// the runtime files (sockets, pipes) each archive leaves behind.
	fileData     map[string][]byte
	fileSkipped  map[string]int64
	corruptFiles bool
	archives     []agent.MoveFileArchiveRequest
	// Files, destination side.
	fileRestores map[string][][]byte
	fileFetches  []string
	failFetch    bool
	cleanups     []string
	storage      *moveStorageFake

	// Fault injection (move_retry_test.go): faults answer matching calls
	// ("POST /v1/...") with an error, once per entry; ranges serves
	// ranged downloads; stallAt makes the next dump download stop after
	// that many bytes until the request is cancelled; resume enables
	// resumable uploads on the destination, and dropAt drops the next
	// resumable upload after that many bytes.
	faults  map[string][]moveFault
	ranges  bool
	stallAt int
	resume  bool
	dropAt  int
	uploads map[string][]byte
	offsets []string
	// uploadSize and uploadDigest describe the artifact a resuming
	// destination stages; excluded are the tables the source's backups
	// leave out (move_tables.go).
	uploadSize   int64
	uploadDigest string
	excluded     []string
}

type moveFault struct {
	status  int
	message string
}

// moveStorageFake is workspace storage shared by both agents.
type moveStorageFake struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (s *moveStorageFake) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := []string{}
	for key := range s.objects {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func newMoveFake(t *testing.T) *moveFake {
	f := &moveFake{t: t, tables: map[string]int{}, dumps: map[string][]byte{}, backups: map[string][]agent.BackupRecord{}, databases: map[string]*agent.DatabaseView{}, restores: map[string][][]byte{},
		fileData: map[string][]byte{}, fileRestores: map[string][][]byte{}, removals: map[string]agent.AppRemoveRequest{}}
	mux := http.NewServeMux()
	f.mux = mux
	f.streams = http.NewServeMux()
	j := func(w http.ResponseWriter, status int, v any) { agent.JSON(w, status, v) }
	mux.HandleFunc("GET /v1/move/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		if f.source == nil || f.source.AppID != r.PathValue("id") {
			j(w, 404, map[string]string{"error": "application unavailable"})
			return
		}
		j(w, 200, f.source)
	})
	mux.HandleFunc("GET /v1/move/host", func(w http.ResponseWriter, r *http.Request) { j(w, 200, f.host) })
	mux.HandleFunc("GET /v1/apps", func(w http.ResponseWriter, r *http.Request) { j(w, 200, f.apps) })
	mux.HandleFunc("GET /v1/server-health", func(w http.ResponseWriter, r *http.Request) {
		health := agent.ServerHealth{MemoryTotal: 4 << 30, MemoryAvailable: 2 << 30, Disks: []agent.DiskHealth{{Path: "/", Available: 50 << 30}}, DatabaseBytes: map[string]int64{"shop": 4096}}
		if f.memory != nil {
			health.MemoryTotal, health.MemoryAvailable = f.memory[0], f.memory[1]
		}
		j(w, 200, health)
	})
	mux.HandleFunc("GET /v1/move/apps/{id}/environment", func(w http.ResponseWriter, r *http.Request) {
		if f.source != nil && f.source.AppID == r.PathValue("id") {
			j(w, 200, f.env)
			return
		}
		j(w, 200, f.targetEnv)
	})
	mux.HandleFunc("GET /v1/move/databases/{id}/tables", func(w http.ResponseWriter, r *http.Request) {
		j(w, 200, map[string]int{"tables": f.tables[r.PathValue("id")]})
	})
	mux.HandleFunc("GET /v1/databases", func(w http.ResponseWriter, r *http.Request) {
		views := []agent.DatabaseView{}
		for id, view := range f.databases {
			copy := *view
			copy.AppID = id
			copy.Backups = f.backups[id]
			views = append(views, copy)
		}
		for id := range f.dumps {
			if _, listed := f.databases[id]; !listed {
				views = append(views, agent.DatabaseView{AppID: id, Mode: "local", Ready: true, Backups: f.backups[id]})
			}
		}
		j(w, 200, views)
	})
	mux.HandleFunc("POST /v1/databases/{id}/backup", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		content, ok := f.dumps[id]
		if !ok {
			j(w, 409, map[string]string{"error": "no database configured"})
			return
		}
		sum := sha256.Sum256(content)
		record := agent.BackupRecord{ID: "b" + strconv.Itoa(len(f.backups[id])+1), Created: time.Now().UTC(), Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:]), ExcludedTableData: f.excluded}
		f.backups[id] = append(f.backups[id], record)
		agent.JSON(w, 202, map[string]string{"status": "queued"})
	})
	mux.HandleFunc("GET /v1/move/databases/{id}/backups/{backup}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		for _, record := range f.backups[id] {
			if record.ID == r.PathValue("backup") {
				content := bytes.Clone(f.dumps[id])
				if f.corrupt {
					content[len(content)-1] ^= 0xff
				}
				w.Header().Set(agent.MoveSHA256Header, record.SHA256)
				status := 200
				if start, ok := strings.CutPrefix(r.Header.Get("Range"), "bytes="); ok && f.ranges {
					offset, _ := strconv.Atoi(strings.TrimSuffix(start, "-"))
					w.Header().Set("Content-Range", "bytes "+strconv.Itoa(offset)+"-"+strconv.Itoa(len(content)-1)+"/"+strconv.Itoa(len(content)))
					content, status = content[offset:], http.StatusPartialContent
				}
				if f.ranges {
					w.Header().Set("Accept-Ranges", "bytes")
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(content)))
				w.WriteHeader(status)
				if f.stallAt > 0 {
					stall := f.stallAt
					f.stallAt = 0
					w.Write(content[:stall])
					w.(http.Flusher).Flush()
					// A stalled source: nothing more until the request ends.
					f.mu.Unlock()
					<-r.Context().Done()
					f.mu.Lock()
					return
				}
				w.Write(content)
				return
			}
		}
		j(w, 404, map[string]string{"error": "unknown backup"})
	})
	mux.HandleFunc("POST /v1/move/databases/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		if upload := r.Header.Get(agent.MoveUploadHeader); upload != "" {
			f.resumableRestore(w, r, upload)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || int64(len(body)) != r.ContentLength {
			j(w, 400, map[string]string{"error": "the upload was incomplete; nothing was restored"})
			return
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != r.Header.Get(agent.MoveSHA256Header) {
			j(w, 422, map[string]string{"error": "checksum mismatch"})
			return
		}
		if r.URL.Query().Get("replace") != "1" {
			t.Error("move restores replace the destination's own database")
		}
		f.keeps = append(f.keeps, r.URL.Query().Get("keep"))
		if f.failKeep && r.URL.Query().Get("keep") != "" {
			j(w, 409, map[string]string{"error": "PostgreSQL restore failed; the destination database was left unchanged"})
			return
		}
		f.restores[r.PathValue("id")] = append(f.restores[r.PathValue("id")], body)
		j(w, 200, map[string]string{"status": "restored"})
	})
	// Files.
	digest := func(content []byte) string {
		sum := sha256.Sum256(content)
		return hex.EncodeToString(sum[:])
	}
	mux.HandleFunc("POST /v1/move/apps/{id}/files/{set}/archive", func(w http.ResponseWriter, r *http.Request) {
		var in agent.MoveFileArchiveRequest
		json.NewDecoder(r.Body).Decode(&in)
		f.archives = append(f.archives, in)
		set := r.PathValue("set")
		content, ok := f.fileData[set]
		if !ok {
			j(w, 404, map[string]string{"error": "unknown file set"})
			return
		}
		out := agent.MoveFileArchive{Set: set, Empty: len(content) == 0, Skipped: f.fileSkipped[set]}
		if !out.Empty {
			out.Files, out.Bytes, out.Size, out.SHA256 = 2, 3*int64(len(content)), int64(len(content)), digest(content)
			if in.Upload && f.storage != nil {
				key := "moves/" + in.Move + "/files/" + set + ".tar.gz"
				f.storage.mu.Lock()
				f.storage.objects[key] = bytes.Clone(content)
				f.storage.mu.Unlock()
				out.Key = key
			}
		}
		j(w, 200, out)
	})
	mux.HandleFunc("GET /v1/move/apps/{id}/files/{set}/archive", func(w http.ResponseWriter, r *http.Request) {
		content := bytes.Clone(f.fileData[r.PathValue("set")])
		w.Header().Set(agent.MoveSHA256Header, digest(content))
		if f.corruptFiles {
			content[len(content)-1] ^= 0xff
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(200)
		w.Write(content)
	})
	mux.HandleFunc("POST /v1/move/apps/{id}/files/{set}/restore", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || int64(len(body)) != r.ContentLength || r.URL.Query().Get("move") == "" {
			j(w, 400, map[string]string{"error": "the archive was incomplete; nothing was restored"})
			return
		}
		if digest(body) != r.Header.Get(agent.MoveSHA256Header) {
			j(w, 422, map[string]string{"error": "checksum mismatch"})
			return
		}
		f.fileRestores[r.PathValue("set")] = append(f.fileRestores[r.PathValue("set")], body)
		j(w, 200, agent.MoveFileRestore{Set: r.PathValue("set"), Files: 2, Bytes: 3 * int64(len(body)), Path: "volume lidza-storage-shop"})
	})
	mux.HandleFunc("POST /v1/move/apps/{id}/files/{set}/fetch", func(w http.ResponseWriter, r *http.Request) {
		var in agent.MoveFileFetchRequest
		json.NewDecoder(r.Body).Decode(&in)
		if f.failFetch || f.storage == nil {
			j(w, 502, map[string]string{"error": "the archive could not be downloaded from off-site storage"})
			return
		}
		f.storage.mu.Lock()
		body, ok := f.storage.objects[in.Key]
		f.storage.mu.Unlock()
		if !ok || digest(body) != in.SHA256 || int64(len(body)) != in.Size {
			j(w, 422, map[string]string{"error": "checksum mismatch"})
			return
		}
		f.fileFetches = append(f.fileFetches, in.Key)
		f.fileRestores[r.PathValue("set")] = append(f.fileRestores[r.PathValue("set")], body)
		j(w, 200, agent.MoveFileRestore{Set: r.PathValue("set"), Files: 2, Bytes: 3 * int64(len(body)), Path: "volume lidza-storage-shop"})
	})
	mux.HandleFunc("DELETE /v1/move/files/{move}", func(w http.ResponseWriter, r *http.Request) {
		f.cleanups = append(f.cleanups, r.PathValue("move"))
		if f.storage != nil {
			f.storage.mu.Lock()
			for key := range f.storage.objects {
				if strings.HasPrefix(key, "moves/"+r.PathValue("move")+"/") {
					delete(f.storage.objects, key)
				}
			}
			f.storage.mu.Unlock()
		}
		j(w, 200, map[string]string{"status": "removed"})
	})
	mux.HandleFunc("PUT /v1/backup-storage", func(w http.ResponseWriter, r *http.Request) { j(w, 200, map[string]string{"status": "saved"}) })
	// Container destination.
	mux.HandleFunc("GET /v1/apps/{id}/settings", func(w http.ResponseWriter, r *http.Request) {
		if f.settings == nil || f.settings.ID != r.PathValue("id") {
			j(w, 404, map[string]string{"error": "unknown application"})
			return
		}
		j(w, 200, f.settings)
	})
	mux.HandleFunc("PUT /v1/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		var spec agent.App
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil || spec.Env != nil {
			t.Error("the destination is created without environment values", err)
		}
		f.settings = &agent.Settings{ID: r.PathValue("id"), Bindings: map[string]string{}}
		f.apps = append(f.apps, agent.App{ID: r.PathValue("id"), Domain: spec.Domain})
		j(w, 200, map[string]string{"status": "saved"})
	})
	mux.HandleFunc("POST /v1/apps/{id}/database", func(w http.ResponseWriter, r *http.Request) {
		var in agent.DatabaseRequest
		json.NewDecoder(r.Body).Decode(&in)
		id := in.ID
		if id == "" {
			id = r.PathValue("id")
		}
		f.settings.Bindings[in.EnvKey] = id
		f.databases[id] = &agent.DatabaseView{Mode: "local", Ready: true}
		if f.provisionError != "" {
			f.databases[id] = &agent.DatabaseView{Mode: "local", Error: f.provisionError}
		}
		j(w, 202, map[string]string{"status": "provisioning"})
	})
	mux.HandleFunc("POST /v1/databases/{id}/provision", func(w http.ResponseWriter, r *http.Request) {
		f.provisions++
		f.databases[r.PathValue("id")] = &agent.DatabaseView{Mode: "local", Ready: true}
		j(w, 202, map[string]string{"status": "queued"})
	})
	mux.HandleFunc("GET /v1/apps/{id}/cache", func(w http.ResponseWriter, r *http.Request) { j(w, 200, agent.CacheView{Mode: "automatic"}) })
	mux.HandleFunc("POST /v1/apps/{id}/cache", func(w http.ResponseWriter, r *http.Request) { j(w, 202, map[string]string{"status": "provisioning"}) })
	mux.HandleFunc("PATCH /v1/apps/{id}/settings", func(w http.ResponseWriter, r *http.Request) {
		var p agent.SettingsPatch
		json.NewDecoder(r.Body).Decode(&p)
		f.patches = append(f.patches, p)
		j(w, 200, map[string]string{"status": "saved"})
	})
	deploy := func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.deploys = append(f.deploys, body)
		status := "live"
		if f.holdDeploy {
			status = "building"
		}
		f.deployment = agent.Deployment{ID: "d" + strconv.Itoa(len(f.deploys)), AppID: r.PathValue("id"), Status: status, Commit: moveTestCommit}
		j(w, 202, f.deployment)
	}
	mux.HandleFunc("POST /v1/apps/{id}/deploy", deploy)
	mux.HandleFunc("GET /v1/deployments/{id}", func(w http.ResponseWriter, r *http.Request) {
		if f.deployment.ID != r.PathValue("id") {
			http.NotFound(w, r)
			return
		}
		if !f.holdDeploy && f.deployment.Status == "building" {
			f.deployment.Status = "live"
		}
		j(w, 200, f.deployment)
	})
	mux.HandleFunc("POST /v1/move/apps/{id}/health", func(w http.ResponseWriter, r *http.Request) {
		j(w, 200, agent.MoveHealth{Healthy: true, Detail: "the web process answered /up on its loopback port"})
	})
	mux.HandleFunc("PUT /v1/apps/{id}/maintenance", func(w http.ResponseWriter, r *http.Request) {
		var m agent.Maintenance
		json.NewDecoder(r.Body).Decode(&m)
		f.maintenance = append(f.maintenance, m.Enabled)
		j(w, 200, m)
	})
	mux.HandleFunc("DELETE /v1/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.deleted = append(f.deleted, r.URL.Path)
		f.removed(r)
		j(w, 200, map[string]string{"status": "removed"})
	})
	mux.HandleFunc("GET /v1/retained-resources", func(w http.ResponseWriter, r *http.Request) {
		rows := []agent.RetainedResourceView{}
		kept := f.retained[:0]
		for _, row := range f.retained {
			rows = append(rows, row)
			if !row.Removing {
				kept = append(kept, row)
			}
		}
		f.retained = kept
		j(w, 200, rows)
	})
	mux.HandleFunc("DELETE /v1/retained-resources/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req agent.RetainedRemoveRequest
		json.NewDecoder(r.Body).Decode(&req)
		for i, row := range f.retained {
			if row.ID != r.PathValue("id") {
				continue
			}
			switch {
			case req.Confirm != row.ID:
				j(w, 409, map[string]string{"error": "confirm the retained resource id to remove it"})
			case !req.FinalBackup && row.FinalBackupRequired:
				j(w, 409, map[string]string{"error": "database has no verified backup taken after its application stopped writing; take a final backup first"})
			default:
				f.retained[i].Removing = true
				f.retainedRemovals = append(f.retainedRemovals, req)
				j(w, 202, map[string]string{"status": "removing"})
			}
			return
		}
		j(w, 404, map[string]string{"error": "no such retained resource on this server"})
	})
	// Native source and destination.
	mux.HandleFunc("GET /v1/native/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case f.nativeSource != nil && f.nativeSource.Application.ID == r.PathValue("id"):
			j(w, 200, f.nativeSource)
		case f.native != nil && f.native.Application.ID == r.PathValue("id"):
			j(w, 200, f.native)
		default:
			j(w, 409, map[string]string{"error": "native application is not registered"})
		}
	})
	mux.HandleFunc("POST /v1/native/apps", func(w http.ResponseWriter, r *http.Request) {
		var spec agent.NativeCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil || spec.Env != nil {
			t.Error("the destination is created without environment values", err)
		}
		for _, row := range f.retained {
			if row.AppID == spec.ID {
				j(w, 409, map[string]string{"error": "the data services of an earlier application " + spec.ID + " are retained on this server; remove them under the server's retained resources, or choose another id"})
				return
			}
		}
		f.created = &spec
		f.native = &agent.NativeStatus{Application: agent.NativeApplication{ID: spec.ID, Name: spec.ID, Runtime: spec.Runtime, Repository: spec.Repository, Branch: spec.Branch, Domain: spec.Domain, Services: []string{"app-" + spec.ID + "-web.service"}}, ReleaseManaged: true, DeploymentSupported: true}
		f.apps = append(f.apps, agent.App{ID: spec.ID, Domain: spec.Domain, Native: &f.native.Application})
		f.targetEnv = agent.MoveEnvironment{AppID: spec.ID, Native: true, Values: map[string]string{"DATABASE_URL": "postgres://app_x:destination-db@127.0.0.1:5432/app_x", "REDIS_URL": "redis://:destination-redis@localhost/0", "PORT": "3101"}}
		j(w, 201, agent.App{ID: spec.ID})
	})
	mux.HandleFunc("GET /v1/native/apps/{id}/data", func(w http.ResponseWriter, r *http.Request) {
		view := agent.NativeHostDataView{AppID: r.PathValue("id")}
		if f.created != nil && f.created.Data.Postgres {
			view.Postgres = &agent.NativeHostPostgresView{Resource: "shop-db0001", Ready: true, Version: 17}
		}
		if f.created != nil && f.created.Data.Redis {
			view.Redis = &agent.NativeHostRedisView{Ready: true}
		}
		if f.created != nil && f.created.Data.ClickHouse {
			view.ClickHouse = &agent.NativeHostClickHouseView{Engine: "clickhouse", Database: "app_" + f.created.ID, Ready: true}
		}
		j(w, 200, view)
	})
	mux.HandleFunc("PATCH /v1/native/apps/{id}/settings", func(w http.ResponseWriter, r *http.Request) {
		var p agent.SettingsPatch
		json.NewDecoder(r.Body).Decode(&p)
		f.patches = append(f.patches, p)
		j(w, 200, map[string]string{"status": "saved"})
	})
	mux.HandleFunc("POST /v1/native/apps/{id}/deploy", deploy)
	mux.HandleFunc("POST /v1/native/apps/{id}/services/{unit}/{action}", func(w http.ResponseWriter, r *http.Request) {
		f.unitCalls = append(f.unitCalls, r.PathValue("action")+" "+r.PathValue("unit"))
		j(w, 200, map[string]string{"status": "requested"})
	})
	mux.HandleFunc("DELETE /v1/native/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.deleted = append(f.deleted, r.URL.String())
		f.removed(r)
		if f.nativeSource != nil && f.nativeSource.Application.ID == r.PathValue("id") {
			f.nativeSource = nil
		} else {
			f.native = nil
		}
		j(w, 200, map[string]string{"status": "removed"})
	})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+moveTestToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if movetunnel.Requested(r) {
			f.streams.ServeHTTP(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/diagnostics") {
			// Diagnostics answer while a transfer holds the fake.
			j(w, 404, map[string]string{"error": "no diagnostics in this fake"})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		call := r.Method + " " + r.URL.Path
		f.calls = append(f.calls, call)
		if faults := f.faults[call]; len(faults) > 0 {
			f.faults[call] = faults[1:]
			j(w, faults[0].status, map[string]string{"error": faults[0].message})
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *moveFake) called(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.calls, call)
}

// removed records an application removal's body and, unless the data is
// deleted with the application, retains the data with the request's origin.
func (f *moveFake) removed(r *http.Request) {
	var req agent.AppRemoveRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			f.t.Error("removal body", err)
		}
	}
	f.removals[r.URL.String()] = req
	if !req.DeleteData {
		f.retained = append(f.retained, agent.RetainedResourceView{ID: "r" + strconv.Itoa(len(f.retained)+1), AppID: r.PathValue("id"), Kind: "application", Origin: req.Origin, Copy: req.Origin != nil && !req.Origin.Switched})
	}
}

func (f *moveFake) with(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

type moveResolverFake struct {
	mu      sync.Mutex
	answers map[string][]string
	ptr     map[string][]string // address → names
	txt     map[string][]string // name → TXT records
	mx      map[string][]string // name → exchangers
}

func (r *moveResolverFake) setPTR(address string, names ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ptr == nil {
		r.ptr = map[string][]string{}
	}
	r.ptr[address] = names
}

func (r *moveResolverFake) setTXT(name string, records ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.txt == nil {
		r.txt = map[string][]string{}
	}
	r.txt[name] = records
}

func (r *moveResolverFake) setMX(name string, hosts ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mx == nil {
		r.mx = map[string][]string{}
	}
	r.mx[name] = hosts
}

func (r *moveResolverFake) LookupAddr(_ context.Context, address string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	names, ok := r.ptr[address]
	if !ok || len(names) == 0 {
		return nil, errors.New("no such host")
	}
	return slices.Clone(names), nil
}

func (r *moveResolverFake) LookupTXT(_ context.Context, name string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, ok := r.txt[name]
	if !ok {
		return nil, errors.New("no such host")
	}
	return slices.Clone(records), nil
}

func (r *moveResolverFake) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	hosts, ok := r.mx[name]
	if !ok {
		return nil, errors.New("no such host")
	}
	out := []*net.MX{}
	for i, host := range hosts {
		out = append(out, &net.MX{Host: host, Pref: uint16(10 * (i + 1))})
	}
	return out, nil
}

func (r *moveResolverFake) set(host string, addresses ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answers[host] = addresses
}

func (r *moveResolverFake) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	answers, ok := r.answers[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	out := []net.IPAddr{}
	for _, a := range answers {
		out = append(out, net.IPAddr{IP: net.ParseIP(a)})
	}
	return out, nil
}

type moveProbeFake struct {
	mu    sync.Mutex
	ok    bool
	calls int
}

func (p *moveProbeFake) probe(context.Context, string, string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if !p.ok {
		return errors.New("no valid certificate yet")
	}
	return nil
}

func (p *moveProbeFake) set(ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ok = ok
}

type moveFixture struct {
	cfg      Config
	c        *Control
	a, b     *moveFake
	resolver *moveResolverFake
	probe    *moveProbeFake
}

func (fx *moveFixture) wire(c *Control) *Control {
	c.moveResolver, c.moveProbe, c.movePollInterval = fx.resolver, fx.probe.probe, time.Millisecond
	c.moveRetryDelays = []time.Duration{time.Millisecond}
	c.credentialsFixture = func(_ context.Context, a Application, key string) (agent.DeployRequest, error) {
		return agent.DeployRequest{Key: key, Token: "short-lived-checkout"}, nil
	}
	return c
}

func newMoveFixture(t *testing.T) *moveFixture {
	fx := &moveFixture{a: newMoveFake(t), b: newMoveFake(t), resolver: &moveResolverFake{answers: map[string][]string{}}, probe: &moveProbeFake{}}
	fx.cfg = Config{PublicURL: "https://deploy.example.com", User: "operator@example.com", Key: []byte(strings.Repeat("k", 32)), DataDir: t.TempDir(),
		Servers: []Server{{ID: "alpha", Name: "Alpha", URL: fx.a.srv.URL, Token: moveTestToken}, {ID: "beta", Name: "Beta", URL: fx.b.srv.URL, Token: moveTestToken}}}
	c, err := New(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	fx.c = fx.wire(c)
	t.Cleanup(func() {
		c.moveTunnelMu.Lock()
		running := maps.Clone(c.moveTunnels)
		c.moveTunnelMu.Unlock()
		for _, tunnel := range running {
			c.moveTunnelStop(tunnel.move, tunnel.sourceDB)
		}
	})
	fx.b.host = agent.MoveHost{IPv4: moveTestTargetIP, IPv6: moveTestTargetIP6, Native: true, HostData: true}
	fx.resolver.set("shop.example.com", moveTestSourceIP)
	fx.resolver.set("www.shop.example.com", moveTestSourceIP)
	return fx
}

// containerSource is a container application with a database and a cache.
func (fx *moveFixture) containerSource() {
	fx.c.data.Apps["shop"] = Application{ID: "shop", ServerID: "alpha", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", AdditionalDomains: []string{"www.shop.example.com"}, AutoDeploy: true, HookID: 42, Secret: "webhook-secret", GitHubInstallation: 7, Generation: "g-source"}
	fx.a.source = &agent.MoveSource{AppID: "shop", Runtime: "go", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", AdditionalDomains: []string{"www.shop.example.com"}, Commit: moveTestCommit,
		EnvironmentKeys: []string{"APP_SECRET", "CACHE_URL", "DATABASE_URL", "PORT", "PUBLIC_NAME"},
		Databases:       []agent.MoveSourceDatabase{{ID: "shop", Engine: "postgresql", Mode: "local", EnvironmentKeys: []string{"DATABASE_URL"}, Copyable: true, Tables: 3, Backup: agent.BackupPolicy{Hours: 24, Keep: 1}}},
		Redis:           true, CacheMode: "local", Volumes: []string{}, Tasks: []string{}, Blockers: []string{}, Notes: []string{},
		Container: &agent.App{ID: "shop", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", AdditionalDomains: []string{"www.shop.example.com"}}}
	fx.a.env = agent.MoveEnvironment{AppID: "shop", Values: map[string]string{"APP_SECRET": moveTestSecret, "PUBLIC_NAME": "Shop", "DATABASE_URL": "postgres://app:source-db-password@lidza-db-shop:5432/app", "CACHE_URL": "redis://:source-cache@cache:6379", "PORT": "8080"},
		Kinds: map[string]string{"APP_SECRET": "credential", "PUBLIC_NAME": "setting", "DATABASE_URL": "credential", "CACHE_URL": "credential", "PORT": "setting"}}
	fx.a.tables["shop"] = 3
	fx.a.dumps["shop"] = []byte("PGDMP container archive with rows")
}

func waitMove(t *testing.T, c *Control, id string, states ...string) MoveJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := c.moveJob(id)
		if ok && slices.Contains(states, job.State) {
			c.mu.Lock()
			running := c.moveRunning[id]
			c.mu.Unlock()
			if !running {
				return job
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	job, _ := c.moveJob(id)
	t.Fatalf("move stayed %s (%s), want %v: %+v", job.State, job.Error, states, job.Steps)
	return job
}

func startMove(t *testing.T, c *Control, appID string, body map[string]any) (int, moveView) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/control/apps/"+appID+"/move", bytes.NewReader(raw))
	req.SetPathValue("id", appID)
	w := httptest.NewRecorder()
	c.startMoveRoute(w, req)
	var view moveView
	json.Unmarshal(w.Body.Bytes(), &view)
	return w.Code, view
}

func moveAction(t *testing.T, c *Control, id, action string, body any) int {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest("POST", "/api/control/moves/"+id+"/"+action, reader)
	req.SetPathValue("id", id)
	req.SetPathValue("action", action)
	w := httptest.NewRecorder()
	c.moveActionRoute(w, req)
	return w.Code
}

func TestMoveContainerApplicationEndToEnd(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatalf("start: %d %+v", status, view)
	}
	if view.TargetRuntime != moveRuntimeContainer || view.Target.Secret != "" {
		t.Fatalf("without repository detection a container keeps its runtime: %+v", view.MoveJob)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	// The source is untouched while the destination is prepared.
	if live := fx.c.data.Apps["shop"]; live.ServerID != "alpha" || fx.a.called("DELETE /v1/apps/shop") || fx.a.called("PUT /v1/apps/shop/maintenance") {
		t.Fatalf("the source must keep serving until the move finishes: %+v", live)
	}
	if !fx.b.called("PUT /v1/apps/shop") || !fx.b.called("POST /v1/apps/shop/cache") {
		t.Fatal("destination not prepared")
	}
	fx.b.with(func() {
		if fx.b.settings.Bindings["DATABASE_URL"] != "shop" {
			t.Fatalf("destination database not bound: %+v", fx.b.settings)
		}
		if got := fx.b.restores["shop"]; len(got) != 1 || string(got[0]) != "PGDMP container archive with rows" {
			t.Fatalf("dump not restored on the destination: %q", got)
		}
		if len(fx.b.patches) != 1 {
			t.Fatalf("environment patches %+v", fx.b.patches)
		}
		patch := fx.b.patches[0]
		if *patch.EnvChanges["APP_SECRET"] != moveTestSecret || *patch.EnvChanges["PUBLIC_NAME"] != "Shop" || patch.EnvKindChanges["APP_SECRET"] != "credential" || patch.Domain != "shop.example.com" || patch.Branch != "main" {
			t.Fatalf("environment not copied: %+v", patch)
		}
		for _, owned := range []string{"DATABASE_URL", "CACHE_URL", "PORT"} {
			if _, copied := patch.EnvChanges[owned]; copied {
				t.Fatalf("%s belongs to the destination", owned)
			}
		}
		if len(fx.b.deploys) != 1 || fx.b.deploys[0]["key"] != "move:"+view.ID+":1" {
			t.Fatalf("deployments %+v", fx.b.deploys)
		}
	})
	// Secrets never reach the move record, its logs or the browser view.
	encoded, _ := json.Marshal(moveJobView(job))
	for _, secret := range []string{moveTestSecret, "source-db-password", "source-cache", "webhook-secret", "short-lived-checkout"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("move record leaked %s", secret)
		}
	}
	if records := moveRecords(job); len(records) != 4 || records[0].Type != "A" || records[1].Type != "AAAA" || records[1].Value != moveTestTargetIP6 {
		t.Fatalf("records %+v", records)
	}
	// DNS still points at the source: finishing is refused.
	if code := moveAction(t, fx.c, view.ID, "finish", nil); code != 409 {
		t.Fatalf("finish before DNS: %d", code)
	}
	fx.resolver.set("shop.example.com", moveTestTargetIP, moveTestTargetIP6)
	fx.resolver.set("www.shop.example.com", moveTestTargetIP)
	fx.probe.set(true)
	fx.c.moveTick(context.Background())
	job = waitMove(t, fx.c, view.ID, moveStateReady)
	if !job.TLSReady || job.step(moveStepCutover).Status != "done" {
		t.Fatalf("cutover not complete: %+v", job)
	}
	if code := moveAction(t, fx.c, view.ID, "finish", nil); code != 202 {
		t.Fatalf("finish: %d", code)
	}
	job = waitMove(t, fx.c, view.ID, moveStateFinished)
	live := fx.c.data.Apps["shop"]
	if live.ServerID != "beta" || !live.AutoDeploy || live.HookID != 42 || live.Secret != "webhook-secret" || live.GitHubInstallation != 7 || live.Generation != job.Target.Generation {
		t.Fatalf("live record not switched: %+v", live)
	}
	if !fx.a.called("DELETE /v1/apps/shop") || fx.b.called("DELETE /v1/apps/shop") {
		t.Fatalf("source retirement: A %v, B %v", fx.a.deleted, fx.b.deleted)
	}
}

func TestMoveResumesAfterControlRestart(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.b.holdDeploy = true
	if err := fx.c.save(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/", nil)
	plan, _, err := fx.c.computeMovePlan(req, "shop", moveStartInput{TargetServer: "beta"})
	if err != nil || len(plan.Blockers) > 0 {
		t.Fatalf("plan: %v %+v", err, plan.Blockers)
	}
	job, err := fx.c.createMove(req, plan)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { fx.c.runMove(ctx, job.ID); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		current, _ := fx.c.moveJob(job.ID)
		if current.Deployment != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment did not start: %+v", current)
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel() // the control panel stops mid-deployment
	<-done

	restarted, err := New(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	fx.wire(restarted)
	resumed, ok := restarted.moveJob(job.ID)
	if !ok || resumed.State != moveStateRunning || resumed.step(moveStepDeploy).Status != "running" || resumed.step(moveStepData).Status != "done" {
		t.Fatalf("move not persisted: %+v", resumed)
	}
	fx.b.with(func() { fx.b.holdDeploy = false })
	restarted.moveTick(context.Background())
	waitMove(t, restarted, job.ID, moveStateCutover)
	fx.b.with(func() {
		if len(fx.b.deploys) != 1 {
			t.Fatalf("the running deployment is followed, not started again: %d", len(fx.b.deploys))
		}
		if len(fx.b.restores["shop"]) != 1 {
			t.Fatalf("finished steps are not repeated: %d restores", len(fx.b.restores["shop"]))
		}
	})
}

func TestMoveFinalSyncAndAbortLeaveTheSourceServing(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatal(status)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.a.with(func() { fx.a.dumps["shop"] = []byte("PGDMP container archive with newer rows") })
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatalf("final sync: %d", code)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	if !job.FinalSynced || !job.SourceFrozen {
		t.Fatalf("final sync: %+v", job)
	}
	fx.a.with(func() {
		if !slices.Equal(fx.a.maintenance, []bool{true}) {
			t.Fatalf("maintenance %v", fx.a.maintenance)
		}
	})
	fx.b.with(func() {
		if got := fx.b.restores["shop"]; len(got) != 2 || string(got[1]) != "PGDMP container archive with newer rows" || len(fx.b.deploys) != 2 {
			t.Fatalf("final data not copied: %q, %d deploys", got, len(fx.b.deploys))
		}
		// The destination writes nothing while its data is replaced, and
		// serves again after the redeploy.
		if !slices.Equal(fx.b.maintenance, []bool{true, false}) {
			t.Fatalf("the destination must be in maintenance during the final sync: %v", fx.b.maintenance)
		}
	})
	if job.TargetFrozen {
		t.Fatalf("the destination stays frozen: %+v", job)
	}
	if code := moveAction(t, fx.c, view.ID, "abort", nil); code != 202 {
		t.Fatalf("abort: %d", code)
	}
	waitMove(t, fx.c, view.ID, moveStateAborted)
	fx.a.with(func() {
		if !slices.Equal(fx.a.maintenance, []bool{true, false}) {
			t.Fatalf("the source must leave maintenance: %v", fx.a.maintenance)
		}
	})
	if !fx.b.called("DELETE /v1/apps/shop") || fx.a.called("DELETE /v1/apps/shop") {
		t.Fatal("abort removes only the destination")
	}
	if live := fx.c.data.Apps["shop"]; live.ServerID != "alpha" || live.Secret != "webhook-secret" {
		t.Fatalf("source record changed: %+v", live)
	}
	if code := moveAction(t, fx.c, view.ID, "finish", nil); code != 409 {
		t.Fatalf("an aborted move cannot finish: %d", code)
	}
	// A new move may start after an abort.
	fx.b.with(func() { fx.b.apps, fx.b.settings = nil, nil })
	status, again := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatalf("restart after abort: %d", status)
	}
	waitMove(t, fx.c, again.ID, moveStateCutover)
}

func TestMoveRejectsDumpChecksumMismatch(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.a.corrupt = true
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatal(status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateFailed)
	if job.step(moveStepData).Status != "failed" || !strings.Contains(job.Error, "SHA-256") {
		t.Fatalf("mismatch not reported: %s %+v", job.Error, job.step(moveStepData))
	}
	fx.b.with(func() {
		if len(fx.b.restores["shop"]) != 0 {
			t.Fatal("a mismatching dump reached the destination database")
		}
	})
	if fx.b.called("POST /v1/apps/shop/deploy") {
		t.Fatal("the move continued after a failed copy")
	}
	// Retrying after the source serves the right bytes resumes at the data step.
	fx.a.with(func() { fx.a.corrupt = false })
	if code := moveAction(t, fx.c, view.ID, "continue", nil); code != 202 {
		t.Fatalf("continue: %d", code)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.with(func() {
		if len(fx.b.restores["shop"]) != 1 {
			t.Fatal("retry did not copy the data")
		}
	})
}

func TestMoveVerifyingReaderHoldsBackTheLastByte(t *testing.T) {
	content := []byte("0123456789")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	for _, scenario := range []struct {
		name  string
		input []byte
		ok    bool
	}{{"exact", content, true}, {"changed", []byte("0123456780"), false}, {"short", content[:9], false}, {"long", append(bytes.Clone(content), '!'), false}} {
		reader := &moveVerifyingReader{r: bytes.NewReader(scenario.input), hash: sha256.New(), expected: digest, size: int64(len(content))}
		out, err := io.ReadAll(io.LimitReader(reader, int64(len(content))))
		if scenario.ok != (err == nil && reader.verified) {
			t.Fatalf("%s: err %v verified %t", scenario.name, err, reader.verified)
		}
		if !scenario.ok && len(out) >= len(content) {
			t.Fatalf("%s: a mismatching stream was forwarded completely", scenario.name)
		}
	}
}

func TestMoveNativeApplicationEndToEnd(t *testing.T) {
	fx := newMoveFixture(t)
	native := agent.NativeApplication{ID: "shop", Name: "shop", Runtime: "rails", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", Services: []string{"app-shop-web.service", "app-shop-jobs.service"}}
	fx.c.data.Apps["shop"] = Application{ID: "shop", ServerID: "alpha", Native: &native, Runtime: "rails", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", GitHubInstallation: 7, Generation: "g-source"}
	spec := agent.NativeCreateRequest{ID: "shop", Runtime: "rails", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", HealthPath: "/up",
		Processes: []agent.NativeProcessSpec{{Name: "web", Command: `bin/rails server -p "$PORT"`, Web: true}, {Name: "jobs", Command: "bin/jobs"}}, Data: agent.NativeDataRequest{Postgres: true, Redis: true}}
	fx.a.source = &agent.MoveSource{AppID: "shop", Runtime: "rails", Native: true, Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", Commit: moveTestCommit,
		EnvironmentKeys: []string{"DATABASE_URL", "PORT", "RAILS_MASTER_KEY", "REDIS_URL", "SECRET_KEY_BASE"},
		Databases:       []agent.MoveSourceDatabase{{ID: "shop-dbsource", Engine: "postgresql", Mode: "host", EnvironmentKeys: []string{"DATABASE_URL"}, Copyable: true, Tables: 12, Backup: agent.BackupPolicy{Hours: 24, Keep: 1}}},
		Redis:           true, Volumes: []string{"files in shared/storage"}, Tasks: []string{}, Blockers: []string{}, Notes: []string{}, CreateSpec: &spec, SpecRecorded: true}
	fx.a.env = agent.MoveEnvironment{AppID: "shop", Native: true, Values: map[string]string{"DATABASE_URL": "postgres://app_shop:source-db@127.0.0.1:5432/app_shop", "REDIS_URL": "redis://:source-redis@localhost/0", "PORT": "3001", "RAILS_MASTER_KEY": moveTestSecret, "SECRET_KEY_BASE": "base-secret"},
		Kinds: map[string]string{"RAILS_MASTER_KEY": "credential", "SECRET_KEY_BASE": "credential"}}
	fx.a.nativeSource = &agent.NativeStatus{Application: native}
	fx.a.tables["shop-dbsource"] = 12
	fx.a.dumps["shop-dbsource"] = []byte("PGDMP native archive")
	if status, _ := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"}); status != 409 {
		t.Fatalf("warnings must be acknowledged first: %d", status)
	}
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
	if status != 202 || view.TargetRuntime != moveRuntimeNative {
		t.Fatalf("start: %d %+v", status, view.MoveJob)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.mu.Lock()
	created := fx.b.created
	if created == nil || created.ID != "shop" || !created.Data.Postgres || !created.Data.Redis || len(created.Processes) != 2 || created.Env != nil {
		t.Fatalf("destination request %+v", created)
	}
	if got := fx.b.restores["shop-db0001"]; len(got) != 1 || string(got[0]) != "PGDMP native archive" {
		t.Fatalf("native restore %q", got)
	}
	patch := fx.b.patches[0]
	if *patch.EnvChanges["RAILS_MASTER_KEY"] != moveTestSecret || *patch.EnvChanges["SECRET_KEY_BASE"] != "base-secret" {
		t.Fatalf("credentials not copied: %+v", patch.EnvChanges)
	}
	for _, owned := range []string{"DATABASE_URL", "REDIS_URL", "PORT"} {
		if _, copied := patch.EnvChanges[owned]; copied {
			t.Fatalf("%s belongs to the destination", owned)
		}
	}
	if fx.b.deploys[0]["ref"] != moveTestCommit {
		t.Fatalf("the destination deploys the source's commit: %+v", fx.b.deploys[0])
	}
	fx.b.mu.Unlock()
	// The destination's application is the move's, not inventory's.
	fx.c.mu.Lock()
	shadow := fx.c.moveShadowLocked("beta", "shop") && !fx.c.moveShadowLocked("alpha", "shop")
	fx.c.mu.Unlock()
	if !shadow {
		t.Fatal("inventory must ignore the destination before the switch")
	}
	// Final sync stops the native source's processes and restarts them on abort.
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.a.with(func() {
		if !slices.Equal(fx.a.unitCalls, []string{"stop app-shop-web.service", "stop app-shop-jobs.service"}) {
			t.Fatalf("source processes %v", fx.a.unitCalls)
		}
	})
	fx.resolver.set("shop.example.com", moveTestTargetIP)
	fx.probe.set(true)
	if code := moveAction(t, fx.c, view.ID, "recheck-dns", nil); code != 202 {
		t.Fatal(code)
	}
	if code := moveAction(t, fx.c, view.ID, "finish", nil); code != 202 {
		t.Fatal(code)
	}
	waitMove(t, fx.c, view.ID, moveStateFinished)
	live := fx.c.data.Apps["shop"]
	if live.ServerID != "beta" || live.Native == nil || live.Native.Services[0] != "app-shop-web.service" || live.GitHubInstallation != 7 {
		t.Fatalf("live record %+v", live)
	}
	if !fx.a.called("DELETE /v1/native/apps/shop") || fx.b.called("DELETE /v1/native/apps/shop") {
		t.Fatal("finishing retires the source only")
	}
}

func TestMoveDNSPollingRequiresEveryHostnameAtTheDestination(t *testing.T) {
	fx := newMoveFixture(t)
	job := MoveJob{ID: "m1", AppID: "shop", SourceServer: "alpha", TargetServer: "beta", State: moveStateCutover, Hostnames: []string{"shop.example.com", "www.shop.example.com"}, Addresses: []string{moveTestTargetIP, moveTestTargetIP6}}
	for _, s := range moveStepTitles {
		job.Steps = append(job.Steps, MoveStep{Key: s.key, Title: s.title, Status: "pending"})
	}
	fx.c.data.Moves = map[string]MoveJob{"m1": job}
	check := func(explicit bool) MoveJob {
		fx.c.checkMoveDNS(context.Background(), "m1", explicit)
		job, _ := fx.c.moveJob("m1")
		return job
	}
	// One name still has the source's address next to the destination's.
	fx.resolver.set("shop.example.com", moveTestTargetIP)
	fx.resolver.set("www.shop.example.com", moveTestTargetIP, moveTestSourceIP)
	if job := check(false); job.State != moveStateCutover || !job.DNS[0].Ready || job.DNS[1].Ready || fx.probe.calls != 0 {
		t.Fatalf("partial DNS: %+v, %d probes", job.DNS, fx.probe.calls)
	}
	// An explicit recheck probes HTTPS even before DNS converges.
	if job := check(true); job.TLSReady || fx.probe.calls != 1 || job.TLSDetail == "" {
		t.Fatalf("explicit recheck: %+v", job)
	}
	fx.resolver.set("www.shop.example.com", moveTestTargetIP6)
	if job := check(false); job.State != moveStateCutover || job.TLSReady {
		t.Fatalf("DNS ready but HTTPS missing must keep waiting: %+v", job)
	}
	fx.probe.set(true)
	if job := check(false); job.State != moveStateReady || !job.TLSReady {
		t.Fatalf("ready: %+v", job)
	}
	// DNS moving back reopens the cutover.
	fx.resolver.set("shop.example.com", moveTestSourceIP)
	if job := check(false); job.State != moveStateCutover {
		t.Fatalf("DNS reverted: %+v", job)
	}
	// Unresolvable names are not ready.
	fx.resolver.mu.Lock()
	delete(fx.resolver.answers, "shop.example.com")
	fx.resolver.mu.Unlock()
	if job := check(false); job.DNS[0].Ready || job.DNS[0].Error == "" {
		t.Fatalf("unresolvable: %+v", job.DNS)
	}
}

func TestMoveEnvironmentChangesFilterDestinationKeys(t *testing.T) {
	source := agent.MoveEnvironment{Values: map[string]string{
		"APP_SECRET": "s", "PUBLIC": "p", "DATABASE_URL": "postgres://source", "PRIMARY_DATABASE_URL": "postgres://source", "CACHE_URL": "redis://source",
		"REDIS_URL": "redis://source", "PORT": "1", "LIDZA_ADDR": "x", "LIDZA_MASTER_KEY": strings.Repeat("a", 64), "LIDZA_GATEWAY": "x", "STORAGE_DIR": "/s", "PATH": "/bin", "DEPLOY_GATEWAY_KEY": "x",
	}, Kinds: map[string]string{"APP_SECRET": "credential", "PUBLIC": "setting", "LIDZA_MASTER_KEY": "credential"}}
	job := MoveJob{TargetRuntime: moveRuntimeNative, Redis: true, CacheLocal: true, Databases: []MoveDatabase{{SourceID: "db", Copyable: true, EnvKeys: []string{"DATABASE_URL", "PRIMARY_DATABASE_URL"}}}}
	target := &agent.MoveEnvironment{Values: map[string]string{"DATABASE_URL": "postgres://destination", "REDIS_URL": "redis://destination"}}
	changes, kinds, report := moveEnvironmentChanges(job, source, target)
	want := map[string]string{"APP_SECRET": "s", "PUBLIC": "p", "LIDZA_MASTER_KEY": strings.Repeat("a", 64), "PRIMARY_DATABASE_URL": "postgres://destination", "CACHE_URL": "redis://destination"}
	if len(changes) != len(want) {
		t.Fatalf("changes %v", report)
	}
	for key, value := range want {
		if changes[key] == nil || *changes[key] != value {
			t.Fatalf("%s: %v", key, changes[key])
		}
	}
	if kinds["APP_SECRET"] != "credential" || kinds["PUBLIC"] != "setting" || kinds["CACHE_URL"] != "credential" {
		t.Fatalf("kinds %v", kinds)
	}
	if !slices.Equal(report.mapped, []string{"CACHE_URL", "PRIMARY_DATABASE_URL"}) || !slices.Contains(report.owned, "DATABASE_URL") || !slices.Contains(report.owned, "PATH") {
		t.Fatalf("report %+v", report)
	}
	// Container destinations bind their own databases and cache.
	job.TargetRuntime = moveRuntimeContainer
	changes, _, _ = moveEnvironmentChanges(job, source, nil)
	for _, key := range []string{"DATABASE_URL", "PRIMARY_DATABASE_URL", "CACHE_URL", "PORT", "STORAGE_DIR", "LIDZA_ADDR"} {
		if _, copied := changes[key]; copied {
			t.Fatalf("container destination copied %s", key)
		}
	}
	if changes["REDIS_URL"] == nil || changes["PATH"] == nil {
		t.Fatal("unmanaged keys are copied to containers")
	}
}

func TestMovePlanBlocksConflictsAndUnknownAddresses(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.b.host = agent.MoveHost{}
	fx.b.apps = []agent.App{{ID: "shop", Domain: "other.example.com"}, {ID: "blog", Domain: "www.shop.example.com"}}
	req := httptest.NewRequest("GET", "/", nil)
	plan, status, err := fx.c.computeMovePlan(req, "shop", moveStartInput{TargetServer: "beta"})
	if err != nil || status != 200 {
		t.Fatal(status, err)
	}
	joined := strings.Join(plan.Blockers, "\n")
	for _, want := range []string{"already has an application with this id", "already serves a hostname", "public address is unknown"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing blocker %q in %v", want, plan.Blockers)
		}
	}
	if status, _ := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"}); status != 409 {
		t.Fatalf("blocked move started: %d", status)
	}
	plan, _, _ = fx.c.computeMovePlan(req, "shop", moveStartInput{TargetServer: "beta", Addresses: []string{moveTestTargetIP}})
	if !slices.Equal(plan.Addresses, []string{moveTestTargetIP}) {
		t.Fatalf("explicit addresses %v", plan.Addresses)
	}
	if _, status, _ := fx.c.computeMovePlan(req, "shop", moveStartInput{TargetServer: "beta", Addresses: []string{"10.0.0.1"}}); status != 400 {
		t.Fatalf("private addresses are refused: %d", status)
	}
	if _, status, _ := fx.c.computeMovePlan(req, "shop", moveStartInput{TargetServer: "alpha"}); status != 400 {
		t.Fatal("moving to the same server is refused")
	}
	if !slices.Contains(plan.CopiedKeys, "APP_SECRET") || !slices.Contains(plan.DestinationKeys, "DATABASE_URL") || len(plan.Databases) != 1 || plan.Databases[0].Action != "copy" || plan.Databases[0].SizeBytes != 4096 {
		t.Fatalf("plan %+v", plan)
	}
}

func TestMoveCheckoutTicketsMatchTheDestination(t *testing.T) {
	c := &Control{data: saved{Apps: map[string]Application{"shop": {ID: "shop", ServerID: "alpha", Generation: "g-source"}}, Moves: map[string]MoveJob{
		"m1": {ID: "m1", AppID: "shop", State: moveStateRunning, SourceServer: "alpha", TargetServer: "beta", Target: Application{ID: "shop", ServerID: "beta", Generation: "g-target"}},
	}}}
	if target, ok := c.moveCheckoutTarget("shop", "beta", "g-target"); !ok || target.ServerID != "beta" {
		t.Fatal("the destination's checkout ticket is accepted")
	}
	for _, probe := range [][2]string{{"alpha", "g-target"}, {"beta", "g-source"}, {"beta", ""}} {
		if _, ok := c.moveCheckoutTarget("shop", probe[0], probe[1]); ok {
			t.Fatalf("checkout for %v accepted", probe)
		}
	}
	job := c.data.Moves["m1"]
	job.Switched = true
	c.data.Moves["m1"] = job
	if _, ok := c.moveCheckoutTarget("shop", "beta", "g-target"); ok {
		t.Fatal("after the switch the live record authorizes checkouts")
	}
}

// A destination database whose provisioning failed (the container runtime
// was not running yet) is provisioned again by Prepare instead of failing
// every retry with the stale error.
func TestMovePrepareProvisionsAFailedDestinationDatabaseAgain(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.b.with(func() { fx.b.provisionError = "Database operation failed: docker failed: exit status 1" })
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatalf("start: %d", status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.with(func() {
		if fx.b.provisions != 1 {
			t.Fatalf("provision requests: %d", fx.b.provisions)
		}
	})
	if !moveLogContains(job, "failed earlier (Database operation failed: docker failed: exit status 1); provisioning it again.") {
		t.Fatalf("log %+v", job.step(moveStepPrepare).Log)
	}
}
