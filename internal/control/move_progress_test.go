package control

import (
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
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
)

// newMoveProgressControl is a control panel whose servers are source and
// destination, with one move whose data step runs.
func newMoveProgressControl(t *testing.T, source, destination http.Handler) (*Control, MoveJob) {
	t.Helper()
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+moveTestToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	a, b := httptest.NewServer(auth(source)), httptest.NewServer(auth(destination))
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)
	c, err := New(Config{PublicURL: "https://deploy.example.com", User: "operator@example.com", Key: []byte(strings.Repeat("k", 32)), DataDir: t.TempDir(),
		Servers: []Server{{ID: "alpha", Name: "Alpha", URL: a.URL, Token: moveTestToken}, {ID: "beta", Name: "Beta", URL: b.URL, Token: moveTestToken}}})
	if err != nil {
		t.Fatal(err)
	}
	c.movePollInterval = time.Millisecond
	started := time.Now().UTC().Add(-time.Minute)
	job := MoveJob{ID: "move-progress", AppID: "shop", SourceServer: "alpha", TargetServer: "beta", State: moveStateRunning,
		Steps:   []MoveStep{{Key: moveStepData, Title: "Copy data", Status: "running", Started: &started, Log: []MoveLogLine{{Time: started, Message: "Taking a fresh backup of shop (3 tables) on alpha."}}}},
		Created: started, Updated: started}
	c.data.Moves = map[string]MoveJob{job.ID: job}
	return c, job
}

// readMoveView reads a move through GET /api/control/moves/{id}.
func readMoveView(t *testing.T, c *Control, id string) moveView {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/control/moves/"+id, nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	c.moveRoute(w, req)
	var view moveView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	return view
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestMoveStreamProgressIsCountedAndServedFromMemory(t *testing.T) {
	dump := []byte(strings.Repeat("PGDMP rows ", 4096))
	sum := sha256.Sum256(dump)
	record := agent.BackupRecord{ID: "b1", Size: int64(len(dump)), SHA256: hex.EncodeToString(sum[:])}
	half := len(dump) / 2
	resume, restore := make(chan struct{}), make(chan struct{})
	source := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(agent.MoveSHA256Header, record.SHA256)
		w.Header().Set("Content-Length", strconv.Itoa(len(dump)))
		w.WriteHeader(200)
		w.Write(dump[:half])
		w.(http.Flusher).Flush()
		<-resume
		w.Write(dump[half:])
	})
	received := make(chan []byte, 1)
	destination := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		<-restore
		agent.JSON(w, 200, map[string]string{"status": "restored"})
	})
	c, job := newMoveProgressControl(t, source, destination)
	c.moveHeartbeat = time.Hour
	end := c.beginMoveLive(context.Background(), job.ID, moveStepData)
	result := make(chan error, 1)
	go func() {
		result <- c.moveTransferDump(context.Background(), job, MoveDatabase{SourceID: "shop", TargetID: "shop-db0001"}, record, "")
	}()

	// While the dump streams, the GET endpoint shows the bytes forwarded.
	var p *agent.Progress
	waitFor(t, "streaming progress", func() bool {
		p = readMoveView(t, c, job.ID).Steps[0].Progress
		return p != nil && p.DoneBytes >= int64(half)
	})
	if p.Phase != "streaming the dump of shop to beta" || p.TotalBytes != record.Size || p.DoneBytes >= record.Size || p.StartedAt.IsZero() || p.UpdatedAt.Before(p.StartedAt) {
		t.Fatalf("progress %+v", p)
	}
	// Live progress stays in memory: the stream saved nothing.
	persisted, _ := c.moveJob(job.ID)
	if persisted.Steps[0].Progress != nil || !persisted.Updated.Equal(job.Updated) || len(persisted.Steps[0].Log) != 1 {
		t.Fatalf("streaming wrote the state file: %+v", persisted)
	}

	// Once every byte was sent, the destination restores.
	close(resume)
	if body := <-received; string(body) != string(dump) {
		t.Fatal("the destination received a different dump")
	}
	waitFor(t, "restore phase", func() bool {
		p = readMoveView(t, c, job.ID).Steps[0].Progress
		return p != nil && p.Phase == "restoring shop into shop-db0001 on beta"
	})
	close(restore)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	final := end()
	if final == nil || final.Phase != "restoring shop into shop-db0001 on beta" {
		t.Fatalf("final progress %+v", final)
	}
	if p := readMoveView(t, c, job.ID).Steps[0].Progress; p != nil {
		t.Fatal("ended progress is still served", p)
	}
}

func TestMoveHeartbeatReportsSilentStepsAndReplacesItself(t *testing.T) {
	c, job := newMoveProgressControl(t, http.NotFoundHandler(), http.NotFoundHandler())
	c.moveHeartbeat = time.Hour
	end := c.beginMoveLive(context.Background(), job.ID, moveStepData)
	defer func() { end() }()
	tracker := c.moveTracker(job.ID)

	// A step without a phase gets no heartbeat.
	c.moveBeat(job.ID, moveStepData, tracker, time.Second)
	if got, _ := c.moveJob(job.ID); len(got.Steps[0].Log) != 1 {
		t.Fatal("heartbeat without a phase", got.Steps[0].Log)
	}
	c.moveMirror(job.ID, "shop", "alpha", &agent.Progress{Phase: "Dumping PostgreSQL to local disk (database lock wait limited to 30 seconds)", DoneBytes: 1288490189, StartedAt: time.Now().Add(-4 * time.Minute), UpdatedAt: time.Now()})
	c.moveBeat(job.ID, moveStepData, tracker, time.Second)
	got, _ := c.moveJob(job.ID)
	log := got.Steps[0].Log
	if len(log) != 2 || !log[1].Heartbeat || !strings.HasPrefix(log[1].Message, "Still dumping shop on alpha: 1.20 GiB written (4m0s elapsed") {
		t.Fatalf("heartbeat %+v", log)
	}
	// The next heartbeat replaces the previous one.
	c.moveMirror(job.ID, "shop", "alpha", &agent.Progress{Phase: "Uploading required S3 copy (1200.0 MiB total)", DoneBytes: 600 << 20, TotalBytes: 1200 << 20})
	c.moveBeat(job.ID, moveStepData, tracker, time.Second)
	got, _ = c.moveJob(job.ID)
	log = got.Steps[0].Log
	if len(log) != 2 || !strings.HasPrefix(log[1].Message, "Still uploading shop to off-site storage from alpha: 600.0 MiB of 1.17 GiB") {
		t.Fatalf("heartbeat %+v", log)
	}
	// A step that logged recently is not silent.
	c.moveLog(job.ID, moveStepData, "Backup b1 is ready.")
	c.moveBeat(job.ID, moveStepData, tracker, time.Minute)
	if got, _ := c.moveJob(job.ID); len(got.Steps[0].Log) != 3 || got.Steps[0].Log[2].Heartbeat {
		t.Fatal("heartbeat after recent output", got.Steps[0].Log)
	}
	// The heartbeat runs on its own while the step is silent.
	end()
	c.moveHeartbeat = 5 * time.Millisecond
	end = c.beginMoveLive(context.Background(), job.ID, moveStepData)
	c.moveStage(job.ID, "restoring shop into shop-db0001 on beta", 0, nil)
	waitFor(t, "a heartbeat", func() bool {
		got, _ := c.moveJob(job.ID)
		last := got.Steps[0].Log[len(got.Steps[0].Log)-1]
		return last.Heartbeat && strings.HasPrefix(last.Message, "Still restoring shop into shop-db0001 on beta")
	})
	for _, line := range got.Steps[0].Log {
		if strings.Contains(line.Message, moveTestToken) {
			t.Fatal("heartbeat leaked a credential")
		}
	}
}

func TestMoveFreshBackupMirrorsTheSourcesDumpProgress(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.a.with(func() {
		fx.a.databases["shop"] = &agent.DatabaseView{Mode: "local", Ready: true, Operation: "backup",
			Progress: &agent.Progress{Phase: "Dumping PostgreSQL to local disk (database lock wait limited to 30 seconds)", DoneBytes: 3 << 20, StartedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now()}}
	})
	started := time.Now().UTC()
	fx.c.data.Moves = map[string]MoveJob{"m1": {ID: "m1", AppID: "shop", SourceServer: "alpha", TargetServer: "beta", State: moveStateRunning, Steps: []MoveStep{{Key: moveStepData, Status: "running", Started: &started}}}}
	end := fx.c.beginMoveLive(context.Background(), "m1", moveStepData)
	defer end()
	type outcome struct {
		record agent.BackupRecord
		err    error
	}
	result := make(chan outcome, 1)
	go func() {
		record, err := fx.c.moveFreshBackup(context.Background(), "alpha", "shop", func(p *agent.Progress) { fx.c.moveMirror("m1", "shop", "alpha", p) })
		result <- outcome{record, err}
	}()
	waitFor(t, "dump progress", func() bool {
		p := readMoveView(t, fx.c, "m1").Steps[0].Progress
		return p != nil && p.Phase == "dumping shop on alpha" && p.DoneBytes == 3<<20
	})
	sum := sha256.Sum256(fx.a.dumps["shop"])
	fx.a.with(func() {
		fx.a.databases["shop"].Operation, fx.a.databases["shop"].Progress = "", nil
		fx.a.backups["shop"] = []agent.BackupRecord{{ID: "b9", Created: time.Now().UTC(), Size: int64(len(fx.a.dumps["shop"])), SHA256: hex.EncodeToString(sum[:])}}
	})
	got := <-result
	if got.err != nil || got.record.ID != "b9" {
		t.Fatal(got)
	}
}

func TestMoveStepsPersistTheirLastProgressAndAcceptOldRecords(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.withFiles(false)
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatalf("start: %d", status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	if p := job.step(moveStepData).Progress; p == nil || p.Phase != "restoring shop into shop on beta" {
		t.Fatalf("data step progress %+v", p)
	}
	// The empty stack volume is archived last and has nothing to restore.
	if p := job.step(moveStepFiles).Progress; p == nil || p.Phase != "archiving stack volume lidza-stack-shop-media on alpha" {
		t.Fatalf("files step progress %+v", p)
	}
	if p := job.step(moveStepDeploy).Progress; p == nil || p.Phase != "checking the application's health on beta" {
		t.Fatalf("deploy step progress %+v", p)
	}
	// Agents without the file progress endpoint answer 404 once per
	// operation and are not polled again during it.
	polls := func(f *moveFake) int {
		n := 0
		f.with(func() {
			for _, call := range f.calls {
				if strings.HasSuffix(call, "/progress") {
					n++
				}
			}
		})
		return n
	}
	if a, b := polls(fx.a), polls(fx.b); a > 2 || b > 1 {
		t.Fatalf("progress polls: source %d, destination %d", a, b)
	}

	// Jobs stored before progress existed decode without it.
	var old MoveJob
	raw := `{"id":"old","app_id":"shop","state":"running","steps":[{"key":"data","title":"Copy data","status":"running","started":"2026-11-05T01:00:00Z","log":[{"time":"2026-11-05T01:00:01Z","message":"Taking a fresh backup."}]}]}`
	if err := json.Unmarshal([]byte(raw), &old); err != nil || old.Steps[0].Progress != nil || old.Steps[0].Log[0].Heartbeat {
		t.Fatal(err, old)
	}
	encoded, _ := json.Marshal(moveJobView(old))
	if strings.Contains(string(encoded), `"progress"`) || strings.Contains(string(encoded), `"heartbeat"`) {
		t.Fatal("optional fields are omitted", string(encoded))
	}
}

func TestMoveViewOffersResumeOnlyWithoutAnActiveRunner(t *testing.T) {
	job := MoveJob{ID: "m1", State: moveStateSyncing}
	if !slices.Contains(withoutActiveResume(moveJobView(job), false).Actions, "continue") {
		t.Fatal("a syncing move whose runner stopped must offer Resume")
	}
	if slices.Contains(withoutActiveResume(moveJobView(job), true).Actions, "continue") {
		t.Fatal("Resume was offered while the final sync is running")
	}
	job.State = moveStateFailed
	if !slices.Contains(withoutActiveResume(moveJobView(job), true).Actions, "continue") {
		t.Fatal("a failed move must keep Retry")
	}
}
