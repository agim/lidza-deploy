package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// Fault injection for the move framework: retries, resumed transfers,
// operator-actionable failures, stalls, the final-sync lock, source changes
// during the background copy, jobs recorded by earlier versions, and tables
// the source's backups leave out.

// resumableRestore is the destination side of a resumable dump upload
// (agent move_resume.go): it appends at the staged offset and verifies the
// whole artifact before it restores.
func (f *moveFake) resumableRestore(w http.ResponseWriter, r *http.Request, upload string) {
	if f.uploads == nil {
		f.uploads = map[string][]byte{}
	}
	size, _ := strconv.Atoi(r.Header.Get(agent.MoveSizeHeader))
	offset, _ := strconv.Atoi(r.Header.Get(agent.MoveOffsetHeader))
	f.offsets = append(f.offsets, r.Header.Get(agent.MoveOffsetHeader))
	staged := f.uploads[upload]
	if offset == 0 {
		staged = nil
	}
	if offset != len(staged) {
		agent.JSON(w, 409, map[string]string{"error": "the upload continues at byte " + strconv.Itoa(len(staged))})
		return
	}
	if f.dropAt > 0 {
		drop := f.dropAt
		f.dropAt = 0
		part := make([]byte, drop)
		n, _ := io.ReadFull(r.Body, part)
		f.uploads[upload] = append(staged, part[:n]...)
		// The connection drops in the middle of the stream.
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			conn.Close()
		}
		return
	}
	rest, err := io.ReadAll(r.Body)
	staged = append(staged, rest...)
	f.uploads[upload] = staged
	if err != nil || len(staged) != size {
		agent.JSON(w, 400, map[string]string{"error": "the upload ended before the artifact's last byte"})
		return
	}
	sum := sha256.Sum256(staged)
	if hex.EncodeToString(sum[:]) != r.Header.Get(agent.MoveSHA256Header) {
		delete(f.uploads, upload)
		agent.JSON(w, 422, map[string]string{"error": "the artifact does not match its SHA-256 digest"})
		return
	}
	f.restores[r.PathValue("id")] = append(f.restores[r.PathValue("id")], bytes.Clone(staged))
	delete(f.uploads, upload)
	w.Header().Set(agent.MoveVerifiedHeader, hex.EncodeToString(sum[:]))
	agent.JSON(w, 200, map[string]string{"status": "restored"})
}

// enableResume serves the upload state route of a resuming destination.
func (f *moveFake) enableResume() {
	f.resume = true
	f.mux.HandleFunc("GET /v1/move/uploads/{upload}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("upload")
		agent.JSON(w, 200, agent.MoveUploadState{ID: id, Received: int64(len(f.uploads[id])), Size: f.uploadSize, SHA256: f.uploadDigest})
	})
}

func randomDump(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func moveLogged(job MoveJob, step, text string) bool {
	s := job.step(step)
	return s != nil && slices.ContainsFunc(s.Log, func(line MoveLogLine) bool { return strings.Contains(line.Message, text) })
}

func TestMoveRetriesWhenTheAgentIsUnavailable(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	// The destination answers 503 twice while its agent restarts.
	fx.b.faults = map[string][]moveFault{"POST /v1/move/databases/shop/restore": {{503, "agent restarting"}, {503, "agent restarting"}}}
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatal(status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	if job.Failure != nil || !moveLogged(job, moveStepData, "attempt 2/6 in") || !moveLogged(job, moveStepData, "attempt 3/6 in") || !moveLogged(job, moveStepData, "Succeeded after 2 failed attempts") {
		t.Fatalf("retries not logged: %+v", job.step(moveStepData).Log)
	}
	fx.b.with(func() {
		if len(fx.b.restores["shop"]) != 1 {
			t.Fatalf("restores %d", len(fx.b.restores["shop"]))
		}
	})
	// The retry reused the backup the first attempt took.
	fx.a.with(func() {
		if len(fx.a.backups["shop"]) != 1 {
			t.Fatalf("the retry takes no new backup: %d", len(fx.a.backups["shop"]))
		}
	})
}

func TestMoveResumesAnInterruptedTransferByOffset(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	dump := randomDump(t, 256<<10)
	sum := sha256.Sum256(dump)
	fx.a.dumps["shop"] = dump
	fx.a.ranges = true
	fx.b.enableResume()
	fx.b.uploadSize, fx.b.uploadDigest = int64(len(dump)), hex.EncodeToString(sum[:])
	fx.b.dropAt = 100 << 10
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatal(status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.with(func() {
		if got := fx.b.restores["shop"]; len(got) != 1 || !bytes.Equal(got[0], dump) {
			t.Fatal("the resumed dump differs from the source's")
		}
		if !slices.Equal(fx.b.offsets, []string{"0", strconv.Itoa(100 << 10)}) {
			t.Fatalf("upload offsets %v", fx.b.offsets)
		}
	})
	if !moveLogged(job, moveStepData, "resuming from the destination's offset") || job.Failure != nil {
		t.Fatalf("resume not logged: %+v", job.step(moveStepData).Log)
	}
	if !fx.a.called("GET /v1/move/databases/shop/backups/b1") {
		t.Fatal("no download")
	}
}

func TestMoveStopsAtOnceOnPermissionErrors(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.a.faults = map[string][]moveFault{"POST /v1/databases/shop/backup": {{409, "pg_dump: could not open output file: permission denied: /var/lib/lidza/backups/shop"}}}
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatal(status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateFailed)
	f := job.Failure
	if f == nil || f.Class != moveClassOperator || f.Code != "permission" || f.Attempt != 1 || f.NextRetry != nil || f.Step != moveStepData || f.Engine != "PostgreSQL shop" || f.Server != "alpha" {
		t.Fatalf("failure %+v", f)
	}
	if !strings.Contains(f.Remediation, "permissions") || !strings.Contains(f.Message, "permission denied") || !strings.Contains(job.Error, "permission denied") {
		t.Fatalf("message %+v", f)
	}
	if moveLogged(job, moveStepData, "attempt 2/") {
		t.Fatal("an operator error is not retried")
	}
	view = moveJobView(job)
	if !slices.Contains(view.Actions, "continue") || view.Locked {
		t.Fatalf("a failed background step can be retried and does not lock: %+v", view.Actions)
	}
}

func TestMoveStopsAtOnceWhenTheDestinationDiskIsFull(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.b.faults = map[string][]moveFault{"POST /v1/move/databases/shop/restore": {{507, "not enough free disk space for the dump: 1.2 GiB is free and the server keeps a 2.0 GiB reserve"}}}
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatal(status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateFailed)
	if f := job.Failure; f == nil || f.Class != moveClassOperator || f.Code != "disk_space" || f.Server != "beta" || !strings.Contains(f.Remediation, "Free disk space on Beta") || f.Attempt != 1 {
		t.Fatalf("failure %+v", job.Failure)
	}
}

func TestMoveCancelsAStalledTransferAndRetries(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.a.dumps["shop"] = randomDump(t, 64<<10)
	fx.a.stallAt = 1000
	fx.c.moveHeartbeat, fx.c.moveStallWarn, fx.c.moveStallCancel = 5*time.Millisecond, 40*time.Millisecond, 120*time.Millisecond
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatal(status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	for _, want := range []string{"Warning: no progress", "cancelling the attempt", "Transient failure (stalled); attempt 2/6", "Diagnostics: Alpha: diagnostics unavailable"} {
		if !moveLogged(job, moveStepData, want) {
			t.Fatalf("%q not logged: %+v", want, job.step(moveStepData).Log)
		}
	}
	if job.Stall != nil || job.Failure != nil {
		t.Fatalf("the retried attempt clears the stall: %+v %+v", job.Stall, job.Failure)
	}
	fx.b.with(func() {
		if len(fx.b.restores["shop"]) != 1 {
			t.Fatalf("restores %d", len(fx.b.restores["shop"]))
		}
	})
}

func TestMoveLocksOnlyFromTheFinalSync(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatal(status)
	}
	settings := fx.c.moveLock("PATCH /api/control/apps/{id}/settings", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	call := func() int {
		r := httptest.NewRequest("PATCH", "/", nil)
		r.SetPathValue("id", "shop")
		w := httptest.NewRecorder()
		settings(w, r)
		return w.Code
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	if live := fx.c.moveLiveView(job); live.Locked || live.Phase != "live" || call() != 200 {
		t.Fatalf("the background phase keeps the application usable: %+v", live.Phase)
	}
	if summary := fx.c.activeMoveSummaries()["shop"]; summary.Locked || summary.Phase != "live" {
		t.Fatalf("summary %+v", summary)
	}
	fx.b.with(func() { fx.b.holdDeploy = true })
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if job, _ := fx.c.moveJob(view.ID); job.Deployment != "" && job.DeployAttempt == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the final sync did not redeploy")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if call() != 409 || !fx.c.moveLiveView(mustMove(t, fx.c, view.ID)).Locked {
		t.Fatal("the final sync locks the application")
	}
	if summary := fx.c.activeMoveSummaries()["shop"]; !summary.Locked || summary.Phase != "locked" {
		t.Fatalf("summary %+v", summary)
	}
	fx.b.with(func() { fx.b.holdDeploy = false })
	job = waitMove(t, fx.c, view.ID, moveStateCutover)
	if !job.SourceFrozen || call() != 409 {
		t.Fatal("the frozen source stays locked until the move ends")
	}
	if code := moveAction(t, fx.c, view.ID, "abort", nil); code != 202 {
		t.Fatal(code)
	}
	waitMove(t, fx.c, view.ID, moveStateAborted)
	if call() != 200 {
		t.Fatal("an aborted move releases the application")
	}
}

func mustMove(t *testing.T, c *Control, id string) MoveJob {
	t.Helper()
	job, ok := c.moveJob(id)
	if !ok {
		t.Fatal("move not found")
	}
	return job
}

func TestMovePushDuringTheBackgroundCopyRedeploysTheDestination(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatal(status)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	// A push deploys the live source normally.
	const newer = "fedcba9876543210fedcba9876543210fedcba98"
	fx.a.with(func() {
		fx.a.settings = &agent.Settings{ID: "shop", Bindings: map[string]string{}}
		fx.a.source.Commit = newer
	})
	payload, _ := json.Marshal(pushJob{AppID: "shop", Delivery: "delivery-7", Generation: "g-source"})
	if err := fx.c.dispatchPush(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	if !fx.a.called("POST /v1/apps/shop/deploy") {
		t.Fatal("a push during the background copy deploys the source")
	}
	job := mustMove(t, fx.c, view.ID)
	if !job.Stale || len(job.SourceChanges) != 1 || job.SourceChanges[0].Kind != moveChangeRelease {
		t.Fatalf("the destination release is stale: %+v", job.SourceChanges)
	}
	fx.b.with(func() {
		if len(fx.b.deploys) != 1 {
			t.Fatalf("deploys %d", len(fx.b.deploys))
		}
	})
	// The final sync redeploys the source's current commit before the switch.
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	job = waitMove(t, fx.c, view.ID, moveStateCutover)
	if job.Commit != newer || job.Stale || !moveLogged(job, moveStepFinalSync, "The source now runs commit fedcba987654") {
		t.Fatalf("commit %s stale %t: %+v", job.Commit, job.Stale, job.step(moveStepFinalSync).Log)
	}
	if !moveLogged(job, moveStepFinalSync, "Environment exported again: unchanged") {
		t.Fatalf("environment diff not logged: %+v", job.step(moveStepFinalSync).Log)
	}
	fx.b.with(func() {
		if len(fx.b.deploys) != 2 {
			t.Fatalf("deploys %d", len(fx.b.deploys))
		}
	})
}

func TestMoveFinalSyncShowsEnvironmentChanges(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatal(status)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.a.with(func() {
		fx.a.env.Values["PUBLIC_NAME"] = "Shop 2"
		fx.a.env.Values["NEW_FLAG"] = "1"
		delete(fx.a.env.Values, "APP_SECRET")
	})
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	if !moveLogged(job, moveStepFinalSync, "1 changed (PUBLIC_NAME), 1 added (NEW_FLAG), 1 removed (APP_SECRET)") {
		t.Fatalf("diff: %+v", job.step(moveStepFinalSync).Log)
	}
	fx.b.with(func() {
		last := fx.b.patches[len(fx.b.patches)-1]
		if value, removed := last.EnvChanges["APP_SECRET"]; !removed || value != nil || *last.EnvChanges["PUBLIC_NAME"] != "Shop 2" {
			t.Fatalf("patch %+v", last)
		}
	})
	encoded, _ := json.Marshal(job)
	if strings.Contains(string(encoded), "Shop 2") || strings.Contains(string(encoded), moveTestSecret) {
		t.Fatal("the job records fingerprints, never values")
	}
}

func TestMoveLoadsAJobRecordedBeforeRetries(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.b.holdDeploy = true
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatal(status)
	}
	deadline := time.Now().Add(5 * time.Second)
	for mustMove(t, fx.c, view.ID).Deployment == "" {
		if time.Now().After(deadline) {
			t.Fatal("deployment did not start")
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Rewrite the job as an earlier version recorded it: no failure,
	// checkpoints, fingerprints or limits, the old step title, and the data
	// step still running when that control panel stopped.
	raw, _ := json.Marshal(mustMove(t, fx.c, view.ID))
	var record map[string]any
	json.Unmarshal(raw, &record)
	for _, key := range []string{"failure", "max_attempts", "checkpoints", "source_changes", "deployed_commit", "destination_stale", "env_digests", "stall"} {
		delete(record, key)
	}
	for _, step := range record["steps"].([]any) {
		step := step.(map[string]any)
		switch step["key"] {
		case moveStepFinalSync:
			step["title"] = "Final data sync (optional)"
		case moveStepData:
			step["status"] = "running"
		case moveStepFiles, moveStepDeploy:
			step["status"] = "pending"
		}
	}
	record["deployment"], record["deploy_attempt"] = "", 0
	old, _ := json.Marshal(record)
	var job MoveJob
	if err := json.Unmarshal(old, &job); err != nil {
		t.Fatal(err)
	}
	fx.c.mu.Lock()
	fx.c.data.Moves[view.ID] = job
	fx.c.mu.Unlock()
	if err := fx.c.save(); err != nil {
		t.Fatal(err)
	}
	fx.b.with(func() { fx.b.holdDeploy = false })
	restarted, err := New(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	fx.wire(restarted)
	loaded := mustMove(t, restarted, view.ID)
	if loaded.State != moveStateRunning || loaded.Failure != nil || loaded.Checkpoints != nil || moveLocks(loaded) {
		t.Fatalf("old job: %+v", loaded)
	}
	restarted.moveTick(context.Background())
	job = waitMove(t, restarted, view.ID, moveStateCutover)
	if title := moveJobView(job).Steps[7].Title; title != "Final sync (application locked)" {
		t.Fatalf("step titles follow the current wording: %q", title)
	}
	if job.Checkpoints[moveCheckpointKey(movePhaseBulk, moveEnginePostgres, "shop")].Done == nil {
		t.Fatalf("checkpoints start with the resumed step: %+v", job.Checkpoints)
	}
}

// ---- tables the backups leave out ------------------------------------------------

type moveTablesFake struct {
	listed   int
	exports  []agent.MoveTableDataRequest
	restored []string
	rows     map[string]int64 // destination rows by table
	source   map[string]int64 // source rows by table
	// Incremental copies (move_table_delta.go): keys the bulk dump
	// reports, the delta requests the source saw, the tables the
	// destination appended to, and the source's answer to a delta.
	keys      map[string]agent.MoveTableKey
	deltas    []agent.MoveTableDeltaRequest
	appended  []string
	fallback  string
	deltaRows int64
	maximum   string
	// Derived keys (move_table_key.go): the key the source detects on
	// request, the destination's loaded maximum, and the source's count up
	// to a watermark.
	detected  *agent.MoveTableKey
	loadedMax string
	upTo      int64
}

// excludedSource gives the source database two tables whose data its
// backups leave out, and both agents the table-data routes.
func (fx *moveFixture) excludedSource() *moveTablesFake {
	fx.containerSource()
	fake := &moveTablesFake{rows: map[string]int64{}, source: map[string]int64{"public.events": 39_900, "public.audit_log": 686}}
	source := fake.source
	fx.a.source.Databases[0].ExcludedTables = []agent.MoveExcludedTable{{Name: "public.events", Rows: 39_900, Bytes: 27 << 20}, {Name: "public.audit_log", Rows: 686, Bytes: 1 << 20}}
	fx.a.excluded = []string{"public.events", "public.audit_log"}
	fx.b.host.TableData = true
	data := []byte("PGDMP data-only dump of the excluded tables")
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	fx.a.mux.HandleFunc("POST /v1/move/databases/{id}/table-data", func(w http.ResponseWriter, r *http.Request) {
		var in agent.MoveTableDataRequest
		json.NewDecoder(r.Body).Decode(&in)
		fake.exports = append(fake.exports, in)
		rows := map[string]int64{}
		for _, table := range in.Tables {
			rows[table] = source[table]
		}
		agent.JSON(w, 200, agent.MoveTableDataExport{ID: strings.Repeat("e", 64), Size: int64(len(data)), SHA256: digest, Tables: in.Tables, Rows: rows, Keys: fake.keys})
	})
	fx.a.mux.HandleFunc("POST /v1/move/databases/{id}/table-key", func(w http.ResponseWriter, r *http.Request) {
		var in agent.MoveTableKeyRequest
		json.NewDecoder(r.Body).Decode(&in)
		if in.Key == nil {
			agent.JSON(w, 200, agent.MoveTableKeyView{Table: in.Table, Key: fake.detected})
			return
		}
		agent.JSON(w, 200, agent.MoveTableKeyView{Table: in.Table, Total: source[in.Table], Max: fake.maximum, UpTo: fake.upTo})
	})
	fx.b.mux.HandleFunc("POST /v1/move/databases/{id}/table-key", func(w http.ResponseWriter, r *http.Request) {
		var in agent.MoveTableKeyRequest
		json.NewDecoder(r.Body).Decode(&in)
		agent.JSON(w, 200, agent.MoveTableKeyView{Table: in.Table, Total: fake.rows[in.Table], Max: fake.loadedMax})
	})
	delta := []byte("PGCOPY binary rows added since the watermark")
	deltaSum := sha256.Sum256(delta)
	deltaDigest := hex.EncodeToString(deltaSum[:])
	fx.a.mux.HandleFunc("POST /v1/move/databases/{id}/table-delta", func(w http.ResponseWriter, r *http.Request) {
		var in agent.MoveTableDeltaRequest
		json.NewDecoder(r.Body).Decode(&in)
		fake.deltas = append(fake.deltas, in)
		if fake.fallback != "" {
			agent.JSON(w, 200, agent.MoveTableDeltaExport{Table: in.Table, Fallback: fake.fallback})
			return
		}
		agent.JSON(w, 200, agent.MoveTableDeltaExport{ID: strings.Repeat("d", 64), Size: int64(len(delta)), SHA256: deltaDigest, Table: in.Table, Rows: fake.deltaRows, Total: source[in.Table], Max: fake.maximum})
	})
	fx.a.mux.HandleFunc("GET /v1/move/databases/{id}/table-delta/{export}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(agent.MoveSHA256Header, deltaDigest)
		w.Header().Set("Content-Length", strconv.Itoa(len(delta)))
		w.Write(delta)
	})
	fx.b.mux.HandleFunc("POST /v1/move/databases/{id}/table-delta/restore", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, delta) {
			agent.JSON(w, 422, map[string]string{"error": "checksum mismatch"})
			return
		}
		table := r.URL.Query().Get("table")
		fake.appended = append(fake.appended, table+" after "+r.URL.Query().Get("after"))
		fake.rows[table] = source[table]
		agent.JSON(w, 200, agent.MoveTableDeltaResult{Table: table, Total: source[table], Max: fake.maximum})
	})
	fx.a.mux.HandleFunc("GET /v1/move/databases/{id}/table-data/{export}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(agent.MoveSHA256Header, digest)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Write(data)
	})
	fx.a.mux.HandleFunc("GET /v1/databases/{id}/tables", func(w http.ResponseWriter, r *http.Request) {
		fake.listed++
		agent.JSON(w, 200, agent.DatabaseTables{Tables: []agent.DatabaseTable{
			{Name: "public.events", Rows: 41_000, Bytes: 28 << 20, Excludable: true, Excluded: true},
			{Name: "public.audit_log", Rows: 700, Bytes: 1 << 20, Excludable: true, Excluded: true},
			{Name: "public.orders", Rows: 12, Bytes: 16384, Excludable: true},
		}, Total: 3, Excluded: []string{"public.audit_log", "public.events"}})
	})
	fx.a.mux.HandleFunc("DELETE /v1/move/databases/{id}/table-data", func(w http.ResponseWriter, r *http.Request) {
		agent.JSON(w, 200, map[string]string{"status": "removed"})
	})
	fx.a.mux.HandleFunc("POST /v1/move/databases/{id}/row-counts", func(w http.ResponseWriter, r *http.Request) {
		var in agent.MoveTableDataRequest
		json.NewDecoder(r.Body).Decode(&in)
		rows := map[string]int64{}
		for _, table := range in.Tables {
			rows[table] = source[table]
		}
		agent.JSON(w, 200, agent.MoveTableRows{Rows: rows})
	})
	fx.b.mux.HandleFunc("POST /v1/move/databases/{id}/table-data/restore", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, data) {
			agent.JSON(w, 422, map[string]string{"error": "checksum mismatch"})
			return
		}
		tables := strings.Split(r.URL.Query().Get("tables"), ",")
		fake.restored = append(fake.restored, tables...)
		rows := map[string]int64{}
		for _, table := range tables {
			fake.rows[table] = source[table]
			rows[table] = source[table]
		}
		agent.JSON(w, 200, agent.MoveTableRows{Rows: rows})
	})
	fx.b.mux.HandleFunc("POST /v1/move/databases/{id}/row-counts", func(w http.ResponseWriter, r *http.Request) {
		var in agent.MoveTableDataRequest
		json.NewDecoder(r.Body).Decode(&in)
		rows := map[string]int64{}
		for _, table := range in.Tables {
			rows[table] = fake.rows[table]
		}
		agent.JSON(w, 200, agent.MoveTableRows{Rows: rows})
	})
	return fake
}

func TestMoveCopiesTheDataOfTablesTheBackupsLeaveOut(t *testing.T) {
	fx := newMoveFixture(t)
	tables := fx.excludedSource()
	plan, _, err := fx.c.computeMovePlan(httptest.NewRequest("GET", "/", nil), "shop", moveStartInput{TargetServer: "beta"})
	if err != nil || len(plan.Databases[0].ExcludedTables) != 2 || plan.Databases[0].ExcludedTables[0].Choice != moveTableCopy || !slices.ContainsFunc(plan.Notes, func(n string) bool {
		return strings.Contains(n, "public.events (about 39k rows, 27.0 MiB: data copied)")
	}) {
		t.Fatalf("plan: %v %+v %q", err, plan.Databases, plan.Notes)
	}
	// A measured inventory needs no table listing.
	fx.a.with(func() {
		if tables.listed != 0 {
			t.Fatalf("listed %d times", tables.listed)
		}
	})
	// Leave the audit log behind; copy the events.
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "excluded_tables": map[string]map[string]string{"shop": {"public.audit_log": "leave"}}})
	if status != 202 {
		t.Fatal(status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.with(func() {
		if !slices.Equal(tables.restored, []string{"public.events"}) || len(tables.exports) != 1 {
			t.Fatalf("restored %v, exports %+v", tables.restored, tables.exports)
		}
	})
	for _, want := range []string{"Left behind as chosen (schema only, no rows): public.audit_log", "Copied the data of public.events: 39900 rows", "1 copied excluded tables match the source's row counts; 1 left-behind tables are empty"} {
		if !moveLogged(job, moveStepData, want) {
			t.Fatalf("%q not logged: %+v", want, job.step(moveStepData).Log)
		}
	}
	// The final sync copies the chosen tables again.
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.with(func() {
		if len(tables.restored) != 2 {
			t.Fatalf("final sync restores %v", tables.restored)
		}
	})
	fx.resolver.set("shop.example.com", moveTestTargetIP)
	fx.resolver.set("www.shop.example.com", moveTestTargetIP)
	fx.probe.set(true)
	fx.c.moveTick(context.Background())
	waitMove(t, fx.c, view.ID, moveStateReady)
	if code := moveAction(t, fx.c, view.ID, "finish", nil); code != 202 {
		t.Fatal(code)
	}
	job = waitMove(t, fx.c, view.ID, moveStateFinished)
	if !moveLogged(job, moveStepFinish, "Summary: tables left behind as chosen (schema only, no rows on beta): shop: public.audit_log") {
		t.Fatalf("finish summary: %+v", job.step(moveStepFinish).Log)
	}
}

func TestMoveStopsForTablesWithoutAChoiceAndLeavesThemBehindOnRequest(t *testing.T) {
	fx := newMoveFixture(t)
	tables := fx.excludedSource()
	// An agent that does not report its exclusions in the inventory (or a
	// policy changed after planning): the backup still names them.
	fx.a.source.Databases[0].ExcludedTables = nil
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatal(status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateFailed)
	if f := job.Failure; f == nil || f.Code != "excluded_tables" || f.Class != moveClassOperator || !strings.Contains(f.Message, "public.events, public.audit_log") {
		t.Fatalf("failure %+v", job.Failure)
	}
	if len(job.Databases[0].Excluded) != 2 || job.Databases[0].Excluded[0].Choice != "" || !slices.Contains(moveJobView(job).Actions, "excluded-tables") {
		t.Fatalf("undecided tables: %+v", job.Databases[0].Excluded)
	}
	// The choice shows the source's estimates, not "rows unknown".
	if e := job.Databases[0].Excluded[0]; e.Name != "public.events" || e.Rows != 41_000 || e.Bytes != 28<<20 {
		t.Fatalf("undecided tables are not measured: %+v", job.Databases[0].Excluded)
	}
	if code := moveAction(t, fx.c, view.ID, "excluded-tables", map[string]any{"database": "shop", "tables": map[string]string{"public.events": "leave", "public.audit_log": "leave"}}); code != 202 {
		t.Fatal(code)
	}
	if code := moveAction(t, fx.c, view.ID, "continue", nil); code != 202 {
		t.Fatal(code)
	}
	job = waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.with(func() {
		if len(tables.restored) != 0 || len(tables.exports) != 0 {
			t.Fatalf("left-behind tables are not copied: %v", tables.restored)
		}
	})
	if !moveLogged(job, moveStepData, "0 copied excluded tables match the source's row counts; 2 left-behind tables are empty as expected") {
		t.Fatalf("verification: %+v", job.step(moveStepData).Log)
	}
}

// moveSimulateRestartDuringData makes a move that reached the cutover look
// like the control panel stopped during its data step after the engine's
// last checkpoint: the step is running again, the engine's checkpoint is
// not done, and change edits its reuse data.
func moveSimulateRestartDuringData(t *testing.T, c *Control, id, key string, change func(*MoveCheckpoint)) {
	t.Helper()
	if _, err := c.updateMove(id, func(job *MoveJob) error {
		job.State = moveStateRunning
		job.setStep(moveStepData, "running")
		job.setStep(moveStepCutover, "pending")
		point, ok := job.Checkpoints[key]
		if !ok || point.Done == nil {
			t.Fatalf("checkpoint %s not finished: %+v", key, job.Checkpoints)
		}
		point.Done = nil
		if change != nil {
			change(&point)
		}
		job.Checkpoints[key] = point
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMoveResumeSkipsTheVerifiedRestoreAndTableLoad(t *testing.T) {
	fx := newMoveFixture(t)
	tables := fx.excludedSource()
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "excluded_tables": map[string]map[string]string{"shop": {"public.audit_log": "leave"}}})
	if status != 202 {
		t.Fatal(status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	key := moveCheckpointKey(movePhaseBulk, moveEnginePostgres, "shop")
	var point movePostgresCheckpoint
	if err := json.Unmarshal(job.Checkpoints[key].Meta, &point); err != nil || point.Restored != job.Databases[0].Copied || point.Tables == nil || point.Tables.Dump != point.Restored || !slices.Equal(point.Tables.Tables, []string{"public.events"}) || point.Tables.Rows["public.events"] != 39_900 {
		t.Fatalf("checkpoint after the copy: %v %+v", err, point)
	}
	// The control panel restarted right after the excluded tables' load
	// finished and verified: neither the dump nor the load is repeated.
	moveSimulateRestartDuringData(t, fx.c, view.ID, key, nil)
	restarted, err := New(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	fx.wire(restarted)
	restarted.moveTick(context.Background())
	job = waitMove(t, restarted, view.ID, moveStateCutover)
	fx.b.with(func() {
		if len(fx.b.restores["shop"]) != 1 || !slices.Equal(tables.restored, []string{"public.events"}) || len(tables.exports) != 1 {
			t.Fatalf("resume repeated a verified copy: %d restores, loads %v, exports %d", len(fx.b.restores["shop"]), tables.restored, len(tables.exports))
		}
	})
	for _, want := range []string{"was restored into shop on beta before this attempt", "The data of public.events was loaded into shop on beta before this attempt", "1 copied excluded tables match the source's row counts"} {
		if !moveLogged(job, moveStepData, want) {
			t.Fatalf("%q not logged:\n%s", want, moveStepLog(job, moveStepData))
		}
	}
	// A restart during the load itself: the restored dump is kept and only
	// the excluded tables are loaded again. An interrupted load leaves no
	// rows behind (it runs in one transaction), so the destination's counts
	// fall outside the dump's range and the shortcut does not apply.
	fx.b.with(func() { tables.rows = map[string]int64{} })
	moveSimulateRestartDuringData(t, restarted, view.ID, key, func(p *MoveCheckpoint) {
		p.Meta, _ = json.Marshal(movePostgresCheckpoint{Restored: point.Restored})
	})
	again, err := New(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	fx.wire(again)
	again.moveTick(context.Background())
	waitMove(t, again, view.ID, moveStateCutover)
	fx.b.with(func() {
		if len(fx.b.restores["shop"]) != 1 || !slices.Equal(tables.restored, []string{"public.events", "public.events"}) {
			t.Fatalf("resume during the load: %d restores, loads %v", len(fx.b.restores["shop"]), tables.restored)
		}
	})
	// A checkpoint of another dump (a retry took a fresh backup) copies
	// everything again.
	moveSimulateRestartDuringData(t, again, view.ID, key, func(p *MoveCheckpoint) {
		p.Meta, _ = json.Marshal(movePostgresCheckpoint{Restored: strings.Repeat("0", 64)})
	})
	last, err := New(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	fx.wire(last)
	last.moveTick(context.Background())
	waitMove(t, last, view.ID, moveStateCutover)
	fx.b.with(func() {
		if len(fx.b.restores["shop"]) != 2 || len(tables.restored) != 3 {
			t.Fatalf("a different dump must be restored: %d restores, loads %v", len(fx.b.restores["shop"]), tables.restored)
		}
	})
}
