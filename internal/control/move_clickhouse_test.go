package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

const moveTestClickHousePassword = "source-clickhouse-password"

// clickhouseFake adds the ClickHouse move routes to a fake agent: exports on
// the source, prepare and restore on the destination.
type clickhouseFake struct {
	mu       sync.Mutex
	archive  []byte
	corrupt  bool
	formats  []string // requested export formats
	compress []string // requested export compression
	deletes  int
	restores [][]byte
	restored []string // formats of restored archives
	// rejectBackup refuses to restore BACKUP archives; short reports one row
	// fewer than the export.
	rejectBackup, short bool
	version             string
	database            string
	// drift makes the destination's current row counts differ from the
	// export; counted tallies the count requests.
	drift         bool
	counted       int
	frozen        func() bool // whether the source was frozen at export time
	frozenExports []bool
}

func withClickHouse(f *moveFake, database, version string) *clickhouseFake {
	ch := &clickhouseFake{version: version, database: database}
	digest := func(content []byte) string {
		sum := sha256.Sum256(content)
		return hex.EncodeToString(sum[:])
	}
	tables := []agent.ClickHouseManifestTable{{Name: "events", Engine: "MergeTree", Data: true, RowsBefore: 1200, RowsAfter: 1200}, {Name: "per_kind_mv", Engine: "MaterializedView"}}
	exportID := strings.Repeat("e", 64)
	f.mux.HandleFunc("POST /v1/move/apps/{id}/clickhouse/exports", func(w http.ResponseWriter, r *http.Request) {
		ch.mu.Lock()
		defer ch.mu.Unlock()
		format := r.URL.Query().Get("format")
		ch.formats = append(ch.formats, format)
		ch.compress = append(ch.compress, r.URL.Query().Get("compress"))
		// The archive names its format, so the destination fake can tell.
		ch.archive = []byte("lidza ClickHouse archive format=" + format)
		if ch.frozen != nil {
			ch.frozenExports = append(ch.frozenExports, ch.frozen())
		}
		agent.JSON(w, 200, agent.MoveClickHouseExport{ID: exportID, Format: format, Database: "telemetry", Version: "26.8.15.10", Size: int64(len(ch.archive)), SHA256: digest(ch.archive), Tables: tables})
	})
	f.mux.HandleFunc("GET /v1/move/apps/{id}/clickhouse/exports/{export}", func(w http.ResponseWriter, r *http.Request) {
		ch.mu.Lock()
		content := bytes.Clone(ch.archive)
		corrupt := ch.corrupt
		ch.mu.Unlock()
		w.Header().Set(agent.MoveSHA256Header, digest(content))
		if corrupt {
			content[len(content)-1] ^= 0xff
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(200)
		w.Write(content)
	})
	f.mux.HandleFunc("DELETE /v1/move/apps/{id}/clickhouse/exports", func(w http.ResponseWriter, r *http.Request) {
		ch.mu.Lock()
		ch.deletes++
		ch.mu.Unlock()
		agent.JSON(w, 200, map[string]string{"status": "removed"})
	})
	f.mux.HandleFunc("POST /v1/move/apps/{id}/clickhouse/prepare", func(w http.ResponseWriter, r *http.Request) {
		agent.JSON(w, 200, agent.MoveClickHouseTarget{Mode: "container", Database: ch.database, Version: ch.version, Ready: true})
	})
	f.mux.HandleFunc("GET /v1/move/apps/{id}/clickhouse", func(w http.ResponseWriter, r *http.Request) {
		target := agent.MoveClickHouseTarget{Mode: "container", Database: ch.database, Version: ch.version, Ready: true}
		if r.URL.Query().Get("counts") == "1" {
			ch.mu.Lock()
			ch.counted++
			rows := uint64(1200)
			if ch.drift {
				rows = 900
			}
			ch.mu.Unlock()
			target.Counts = map[string]uint64{"events": rows}
		}
		agent.JSON(w, 200, target)
	})
	f.mux.HandleFunc("POST /v1/move/apps/{id}/clickhouse/restore", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || int64(len(body)) != r.ContentLength || r.URL.Query().Get("replace") != "1" {
			agent.JSON(w, 400, map[string]string{"error": "the upload was incomplete; nothing was restored"})
			return
		}
		if digest(body) != r.Header.Get(agent.MoveSHA256Header) {
			agent.JSON(w, 422, map[string]string{"error": "checksum mismatch"})
			return
		}
		ch.mu.Lock()
		defer ch.mu.Unlock()
		_, format, _ := strings.Cut(string(body), "format=")
		if ch.rejectBackup && format == "backup" {
			agent.JSON(w, 409, map[string]string{"error": "the ClickHouse restore failed: ClickHouse restore failed: unknown backup format"})
			return
		}
		rows := uint64(1200)
		if ch.short {
			rows--
		}
		ch.restores = append(ch.restores, body)
		ch.restored = append(ch.restored, format)
		agent.JSON(w, 200, agent.ClickHouseRestoreResult{Status: "restored", Format: format, Source: "telemetry", Database: ch.database, Tables: 2, Rows: rows, Counts: map[string]uint64{"events": rows}, Bytes: int64(len(body)), SHA256: digest(body)})
	})
	return ch
}

func (fx *moveFixture) clickhouseSource() (*clickhouseFake, *clickhouseFake) {
	fx.containerSource()
	fx.a.source.ClickHouse = &agent.MoveSourceClickHouse{Mode: "container", Database: "telemetry", Version: "26.8.15.10", Tables: 2, Rows: 1200, SizeBytes: 64 << 20, EnvironmentKeys: agent.ClickHouseEnvironmentKeys, Copyable: true}
	fx.a.source.EnvironmentKeys = append(fx.a.source.EnvironmentKeys, append(slices.Clone(agent.ClickHouseEnvironmentKeys), "CLICKHOUSE_MANAGED")...)
	slices.Sort(fx.a.source.EnvironmentKeys)
	for key, value := range map[string]string{"CLICKHOUSE_URL": "http://lidza-clickhouse-shop1:8123", "CLICKHOUSE_DATABASE": "telemetry", "CLICKHOUSE_USER": "lidza", "CLICKHOUSE_PASSWORD": moveTestClickHousePassword, "CLICKHOUSE_MANAGED": "true"} {
		fx.a.env.Values[key] = value
	}
	fx.b.host.ClickHouse = true
	source, destination := withClickHouse(fx.a, "telemetry", "26.8.15.10"), withClickHouse(fx.b, "telemetry", "26.8.36.3")
	return source, destination
}

func TestMovePlanCopiesClickHouseInsteadOfBlocking(t *testing.T) {
	fx := newMoveFixture(t)
	fx.clickhouseSource()
	req := httptest.NewRequest("GET", "/api/control/apps/shop/move-plan?target_server=beta", nil)
	plan, status, err := fx.c.computeMovePlan(req, "shop", moveStartInput{TargetServer: "beta"})
	if err != nil || status != 200 {
		t.Fatal(status, err)
	}
	for _, blocker := range plan.Blockers {
		if strings.Contains(blocker, "ClickHouse") {
			t.Fatalf("ClickHouse blocks the move: %v", plan.Blockers)
		}
	}
	i := slices.IndexFunc(plan.Databases, func(d MovePlanDatabase) bool { return d.Engine == "clickhouse" })
	if i < 0 || plan.Databases[i].ID != "telemetry" || plan.Databases[i].Action != "copy" || plan.Databases[i].Tables != 2 || plan.Databases[i].SizeBytes != 64<<20 {
		t.Fatalf("databases %+v", plan.Databases)
	}
	if plan.Capacity == nil || plan.Capacity.Needed < 3*64<<20 {
		t.Fatalf("capacity %+v", plan.Capacity)
	}
	for _, key := range agent.ClickHouseEnvironmentKeys {
		if !slices.Contains(plan.DestinationKeys, key) || slices.Contains(plan.CopiedKeys, key) {
			t.Fatalf("%s must come from the destination: copied %v, destination %v", key, plan.CopiedKeys, plan.DestinationKeys)
		}
	}
	if !slices.Contains(plan.CopiedKeys, "CLICKHOUSE_MANAGED") {
		t.Fatalf("the managed flag is the application's setting: %v", plan.CopiedKeys)
	}
	// An older destination agent cannot receive the data.
	fx.b.host.ClickHouse = false
	plan, _, _ = fx.c.computeMovePlan(req, "shop", moveStartInput{TargetServer: "beta"})
	if !slices.ContainsFunc(plan.Blockers, func(b string) bool { return strings.Contains(b, "cannot receive ClickHouse data") }) {
		t.Fatalf("blockers %v", plan.Blockers)
	}
}

func TestMoveCopiesClickHouseAndRepeatsItFrozen(t *testing.T) {
	fx := newMoveFixture(t)
	source, destination := fx.clickhouseSource()
	// Runs inside the fake's handler, which holds fx.a.mu already.
	source.frozen = func() bool {
		return len(fx.a.maintenance) > 0 && fx.a.maintenance[len(fx.a.maintenance)-1]
	}
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatalf("start: %d %+v", status, view)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	if !fx.b.called("POST /v1/move/apps/shop/clickhouse/prepare") {
		t.Fatal("the destination's ClickHouse was not prepared")
	}
	destination.mu.Lock()
	if len(destination.restores) != 1 || !bytes.Equal(destination.restores[0], []byte("lidza ClickHouse archive format=backup")) || destination.restored[0] != "backup" {
		t.Fatalf("restores %q %v", destination.restores, destination.restored)
	}
	destination.mu.Unlock()
	ch := job.ClickHouse
	sum := sha256.Sum256([]byte("lidza ClickHouse archive format=backup"))
	if ch == nil || ch.Copied != hex.EncodeToString(sum[:]) || ch.Format != "backup" || ch.Rows != 1200 || ch.TargetDatabase != "telemetry" || ch.TargetVersion != "26.8.36.3" {
		t.Fatalf("job %+v", ch)
	}
	if log := moveStepLog(job, moveStepData); !strings.Contains(log, "row counts of 2 tables (1200 rows) match the source") {
		t.Fatalf("data log:\n%s", log)
	}
	fx.b.with(func() {
		for _, key := range agent.ClickHouseEnvironmentKeys {
			if _, copied := fx.b.patches[0].EnvChanges[key]; copied {
				t.Fatalf("%s belongs to the destination's ClickHouse", key)
			}
		}
		if value := fx.b.patches[0].EnvChanges["CLICKHOUSE_MANAGED"]; value == nil || *value != "true" {
			t.Fatal("CLICKHOUSE_MANAGED not copied")
		}
	})
	encoded, _ := json.Marshal(moveJobView(job))
	if strings.Contains(string(encoded), moveTestClickHousePassword) {
		t.Fatal("move record leaked the ClickHouse password")
	}
	// The final sync exports again with the source in maintenance.
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	job = waitMove(t, fx.c, view.ID, moveStateCutover)
	if !job.FinalSynced {
		t.Fatalf("final sync %s", job.Error)
	}
	source.mu.Lock()
	if !slices.Equal(source.frozenExports, []bool{false, true}) || source.deletes < 2 {
		t.Fatalf("exports frozen %v, cleanups %d", source.frozenExports, source.deletes)
	}
	source.mu.Unlock()
	// Aborting removes the destination and the source's exports.
	if code := moveAction(t, fx.c, view.ID, "abort", nil); code != 202 {
		t.Fatal(code)
	}
	waitMove(t, fx.c, view.ID, moveStateAborted)
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.deletes < 3 {
		t.Fatalf("abort left exports: %d", source.deletes)
	}
}

func TestMoveRejectsClickHouseChecksumMismatch(t *testing.T) {
	fx := newMoveFixture(t)
	source, destination := fx.clickhouseSource()
	source.corrupt = true
	_, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	job := waitMove(t, fx.c, view.ID, moveStateFailed)
	if job.step(moveStepData).Status != "failed" || !strings.Contains(job.Error, "SHA-256") {
		t.Fatalf("job %s %+v", job.Error, job.step(moveStepData))
	}
	destination.mu.Lock()
	defer destination.mu.Unlock()
	if len(destination.restores) != 0 {
		t.Fatal("a mismatching archive was restored")
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if !slices.Equal(source.formats, []string{"backup"}) {
		t.Fatalf("a checksum mismatch is not retried in another format: %v", source.formats)
	}
}

func TestMoveClickHouseVerifiesRowCounts(t *testing.T) {
	fx := newMoveFixture(t)
	source, destination := fx.clickhouseSource()
	destination.short = true
	_, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	job := waitMove(t, fx.c, view.ID, moveStateFailed)
	if !strings.Contains(job.Error, "row-count verification failed") || !strings.Contains(job.Error, "events has 1199 rows") {
		t.Fatalf("job error %q", job.Error)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if !slices.Equal(source.formats, []string{"backup"}) {
		t.Fatalf("formats %v", source.formats)
	}
}

func TestMoveClickHouseDoesNotFallBackWhenTheDestinationRefusesTheBackup(t *testing.T) {
	fx := newMoveFixture(t)
	source, destination := fx.clickhouseSource()
	destination.rejectBackup = true
	_, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	job := waitMove(t, fx.c, view.ID, moveStateFailed)
	if !strings.Contains(job.Error, "copying ClickHouse") {
		t.Fatalf("job error %q", job.Error)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	// A per-table export is several times larger on the source's disk; a
	// destination error is fixed and retried instead.
	if !slices.Equal(source.formats, []string{"backup"}) {
		t.Fatalf("formats %v", source.formats)
	}
}

func TestMoveNativeApplicationWithClickHouse(t *testing.T) {
	fx := newMoveFixture(t)
	native := agent.NativeApplication{ID: "shop", Name: "shop", Runtime: "go", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", Services: []string{"app-shop-web.service"}}
	fx.c.data.Apps["shop"] = Application{ID: "shop", ServerID: "alpha", Native: &native, Runtime: "go", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", Generation: "g-source"}
	spec := agent.NativeCreateRequest{ID: "shop", Runtime: "go", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", HealthPath: "/healthz",
		Processes: []agent.NativeProcessSpec{{Name: "web", Command: "bin/shop", Web: true}}, Data: agent.NativeDataRequest{ClickHouse: true}}
	fx.a.source = &agent.MoveSource{AppID: "shop", Runtime: "go", Native: true, Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", Commit: moveTestCommit,
		EnvironmentKeys: append(slices.Clone(agent.ClickHouseEnvironmentKeys), "PORT", "SIGNING_KEY"), Databases: []agent.MoveSourceDatabase{}, Volumes: []string{}, Tasks: []string{}, Blockers: []string{}, Notes: []string{},
		ClickHouse: &agent.MoveSourceClickHouse{Mode: "host", Database: "app_shop", Version: "26.8.15.10", Tables: 2, Rows: 1200, SizeBytes: 1 << 20, EnvironmentKeys: agent.ClickHouseEnvironmentKeys, Copyable: true},
		CreateSpec: &spec, SpecRecorded: true}
	fx.a.env = agent.MoveEnvironment{AppID: "shop", Native: true, Values: map[string]string{"CLICKHOUSE_URL": "http://127.0.0.1:8123", "CLICKHOUSE_DATABASE": "app_shop", "CLICKHOUSE_USER": "app_shop", "CLICKHOUSE_PASSWORD": moveTestClickHousePassword, "PORT": "3001", "SIGNING_KEY": moveTestSecret},
		Kinds: map[string]string{"CLICKHOUSE_PASSWORD": "credential", "SIGNING_KEY": "credential"}}
	fx.a.nativeSource = &agent.NativeStatus{Application: native}
	fx.b.host.ClickHouse = true
	withClickHouse(fx.a, "app_shop", "26.8.15.10")
	destination := withClickHouse(fx.b, "app_shop", "26.8.36.3")
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
	if status != 202 {
		t.Fatalf("start %d %+v", status, view)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.mu.Lock()
	created := fx.b.created
	fx.b.mu.Unlock()
	if created == nil || !created.Data.ClickHouse || created.Data.ClickHouseVersion != "26.8" || created.Data.Postgres {
		t.Fatalf("destination request %+v", created)
	}
	if job.ClickHouse == nil || job.ClickHouse.TargetDatabase != "app_shop" || job.ClickHouse.Copied == "" {
		t.Fatalf("job %+v", job.ClickHouse)
	}
	destination.mu.Lock()
	defer destination.mu.Unlock()
	if len(destination.restores) != 1 {
		t.Fatalf("restores %d", len(destination.restores))
	}
}

func TestMoveClickHouseFormatNegotiation(t *testing.T) {
	current := agent.MoveHost{ClickHouseFormats: []string{"directory", "backup", "native"}, Zstd: true}
	older := agent.MoveHost{}
	plain := agent.MoveHost{ClickHouseFormats: []string{"directory", "backup", "native"}}
	for _, c := range []struct {
		name                         string
		source, target               agent.MoveHost
		sourceVersion, targetVersion string
		format, compression          string
	}{
		{"both current", current, current, "26.8.15.10", "26.8.36.3", "directory", "zstd"},
		{"destination without zstd", current, plain, "26.8.15.10", "26.8.36.3", "directory", ""},
		{"older destination agent", current, older, "26.8.15.10", "26.8.36.3", "backup", ""},
		{"older source agent", older, current, "26.8.15.10", "26.8.36.3", "backup", ""},
		{"older destination series", current, current, "26.8.15.10", "25.8.1.1", "native", "zstd"},
	} {
		format, compression := moveClickHouseFormats(c.sourceVersion, c.targetVersion, c.source, c.target)
		if format != c.format || compression != c.compression {
			t.Errorf("%s: %s %q, want %s %q", c.name, format, compression, c.format, c.compression)
		}
	}
}

func TestMoveClickHouseUsesDirectoryBackupsBetweenCurrentAgents(t *testing.T) {
	fx := newMoveFixture(t)
	source, destination := fx.clickhouseSource()
	formats := []string{"directory", "backup", "native"}
	fx.a.host.ClickHouseFormats, fx.a.host.Zstd = formats, true
	fx.b.host.ClickHouseFormats, fx.b.host.Zstd = formats, true
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatalf("start: %d", status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	source.mu.Lock()
	defer source.mu.Unlock()
	destination.mu.Lock()
	defer destination.mu.Unlock()
	if !slices.Equal(source.formats, []string{"directory"}) || !slices.Equal(source.compress, []string{"zstd"}) || !slices.Equal(destination.restored, []string{"directory"}) {
		t.Fatalf("formats %v compress %v restored %v", source.formats, source.compress, destination.restored)
	}
	if job.ClickHouse.Format != "directory" || !strings.Contains(moveStepLog(job, moveStepData), "ClickHouse BACKUP to a directory, zstd") {
		t.Fatalf("job %+v\n%s", job.ClickHouse, moveStepLog(job, moveStepData))
	}
}

func TestMoveResumeSkipsTheVerifiedClickHouseRestore(t *testing.T) {
	fx := newMoveFixture(t)
	_, destination := fx.clickhouseSource()
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatalf("start: %d %+v", status, view)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	key := moveCheckpointKey(movePhaseBulk, moveEngineClickHouse, "telemetry")
	var point moveClickHouseCheckpoint
	if err := json.Unmarshal(job.Checkpoints[key].Meta, &point); err != nil || point.Restored == "" || point.Restored != point.SHA256 || point.Restored != job.ClickHouse.Copied {
		t.Fatalf("checkpoint after the copy: %v %+v", err, point)
	}
	// The control panel restarted after the restore verified: the
	// destination is asked for its row counts and the archive is not
	// streamed or restored again.
	moveSimulateRestartDuringData(t, fx.c, view.ID, key, nil)
	restarted, err := New(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	fx.wire(restarted)
	restarted.moveTick(context.Background())
	job = waitMove(t, restarted, view.ID, moveStateCutover)
	destination.mu.Lock()
	if len(destination.restores) != 1 || destination.counted != 1 {
		t.Fatalf("resume restored again: %d restores, %d count requests", len(destination.restores), destination.counted)
	}
	destination.mu.Unlock()
	if !moveLogged(job, moveStepData, "was restored into telemetry on beta before this attempt and the row counts of 1 tables still match") {
		t.Fatalf("data log:\n%s", moveStepLog(job, moveStepData))
	}
	// A destination whose counts no longer match is restored again.
	destination.mu.Lock()
	destination.drift = true
	destination.mu.Unlock()
	moveSimulateRestartDuringData(t, restarted, view.ID, key, nil)
	again, err := New(fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	fx.wire(again)
	again.moveTick(context.Background())
	job = waitMove(t, again, view.ID, moveStateCutover)
	destination.mu.Lock()
	defer destination.mu.Unlock()
	if len(destination.restores) != 2 {
		t.Fatalf("a drifted destination must be restored again: %d restores", len(destination.restores))
	}
	if !moveLogged(job, moveStepData, "no longer matches the earlier restore") {
		t.Fatalf("data log:\n%s", moveStepLog(job, moveStepData))
	}
}
