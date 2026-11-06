package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/storage"
)

const moveTestArchive = "gzip tar of the storage volume"

// withFiles gives the container source a storage volume with files and an
// empty stack volume, and the destination file support.
func (fx *moveFixture) withFiles(offsite bool) {
	fx.a.source.Files = &agent.MoveFiles{Storage: offsite, Warnings: []string{}, Sets: []agent.MoveFileSet{
		{ID: "storage", Path: "storage", Kind: agent.MoveFileKindVolume, Source: "volume lidza-storage-shop", Files: 2, Bytes: 90},
		{ID: "stack-media", Path: "stack/media", Kind: agent.MoveFileKindVolume, Source: "stack volume lidza-stack-shop-media", Empty: true, PreserveOwners: true},
	}}
	fx.a.source.Volumes = []string{"persistent storage volume lidza-storage-shop", "stack volume lidza-stack-shop-media"}
	fx.a.fileData["storage"] = []byte(moveTestArchive)
	fx.a.fileData["stack-media"] = []byte{}
	fx.b.host.Files = true
	if offsite {
		shared := &moveStorageFake{objects: map[string][]byte{}}
		fx.a.storage, fx.b.storage = shared, shared
		fx.c.data.BackupStorage = &storage.Config{Provider: "s3", Endpoint: "https://storage.example.com", Bucket: "workspace", Region: "us-east-1", AccessKey: "fixture", SecretKey: "fixture-secret"}
	}
}

func moveStepLog(job MoveJob, key string) string {
	lines := []string{}
	for _, line := range job.step(key).Log {
		lines = append(lines, line.Message)
	}
	return strings.Join(lines, "\n")
}

func callIndex(f *moveFake, call string, last bool) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := -1
	for i, c := range f.calls {
		if c == call {
			index = i
			if !last {
				break
			}
		}
	}
	return index
}

func TestMoveCopiesFilesBeforeDeploying(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.withFiles(false)
	// The storage volume holds service sockets: counted, never a blocker.
	fx.a.source.Files.Sets[0].Skipped = 3
	fx.a.fileSkipped = map[string]int64{"storage": 3}
	req := httptest.NewRequest("GET", "/", nil)
	plan, _, err := fx.c.computeMovePlan(req, "shop", moveStartInput{TargetServer: "beta"})
	if err != nil || len(plan.Blockers) > 0 {
		t.Fatalf("plan: %v %v", err, plan.Blockers)
	}
	for _, warning := range plan.Warnings {
		if strings.Contains(warning, "not moved automatically") {
			t.Fatalf("transferable files are still a warning: %v", plan.Warnings)
		}
	}
	if len(plan.Files) != 2 || plan.Files[0].Action != "copy" || plan.Files[0].Method != moveFileMethodDirect || plan.Files[0].Skipped != 3 || plan.Files[1].Action != "skip_empty" {
		t.Fatalf("plan files %+v", plan.Files)
	}
	if !strings.Contains(strings.Join(plan.Notes, "\n"), "runtime state that the services recreate") {
		t.Fatalf("skipped runtime files are explained: %v", plan.Notes)
	}
	// Nothing to acknowledge: the files are moved.
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatalf("start: %d", status)
	}
	keys := []string{}
	for _, step := range view.Steps {
		keys = append(keys, step.Key)
	}
	if !slices.Equal(keys, []string{"plan", "prepare", "environment", "data", "files", "deploy", "cutover", "final_sync", "finish"}) {
		t.Fatalf("steps %v", keys)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	if job.step(moveStepFiles).Status != "done" {
		t.Fatalf("files step %+v", job.step(moveStepFiles))
	}
	fx.b.with(func() {
		if got := fx.b.fileRestores["storage"]; len(got) != 1 || string(got[0]) != moveTestArchive {
			t.Fatalf("restored %q", got)
		}
		if _, restored := fx.b.fileRestores["stack-media"]; restored {
			t.Fatal("an empty set was transferred")
		}
	})
	if files := job.Files[0]; files.Used != moveFileMethodDirect || files.Copied == "" || files.ArchiveBytes != int64(len(moveTestArchive)) {
		t.Fatalf("file record %+v", files)
	}
	if job.Files[1].Skipped != "no files" || !strings.Contains(moveStepLog(job, moveStepFiles), "stack volume lidza-stack-shop-media: no files") {
		t.Fatalf("empty set: %+v\n%s", job.Files[1], moveStepLog(job, moveStepFiles))
	}
	if log := moveStepLog(job, moveStepFiles); !strings.Contains(log, "skipped: 3 runtime sockets/pipes (recreated by the service)") {
		t.Fatalf("skipped runtime files are logged:\n%s", log)
	}
	// Files arrive before the destination deploys, and the archives are
	// removed from both agents afterwards.
	if restore, deploy := callIndex(fx.b, "POST /v1/move/apps/shop/files/storage/restore", false), callIndex(fx.b, "POST /v1/apps/shop/deploy", false); restore < 0 || deploy < restore {
		t.Fatalf("restore at %d, deploy at %d", restore, deploy)
	}
	for _, f := range []*moveFake{fx.a, fx.b} {
		f.with(func() {
			if !slices.Equal(f.cleanups, []string{view.ID}) {
				t.Fatalf("cleanups %v", f.cleanups)
			}
		})
	}
	if !moveCanFinalSync(job) || !slices.Contains(moveActions(job), "final-sync") {
		t.Fatal("files can be synchronized again")
	}
}

func TestMoveRejectsFileArchiveChecksumMismatch(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.withFiles(false)
	fx.a.corruptFiles = true
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatal(status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateFailed)
	if job.step(moveStepFiles).Status != "failed" || !strings.Contains(job.Error, "SHA-256") || job.step(moveStepData).Status != "done" {
		t.Fatalf("mismatch: %s %+v", job.Error, job.step(moveStepFiles))
	}
	fx.b.with(func() {
		if len(fx.b.fileRestores["storage"]) != 0 {
			t.Fatal("a mismatching archive reached the destination")
		}
	})
	if fx.b.called("POST /v1/apps/shop/deploy") {
		t.Fatal("the move deployed without the files")
	}
	// The retry resumes at the files step; the data is not copied again.
	fx.a.with(func() { fx.a.corruptFiles = false })
	if code := moveAction(t, fx.c, view.ID, "continue", nil); code != 202 {
		t.Fatalf("continue: %d", code)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.b.with(func() {
		if len(fx.b.fileRestores["storage"]) != 1 || len(fx.b.restores["shop"]) != 1 {
			t.Fatalf("retry: %d file restores, %d database restores", len(fx.b.fileRestores["storage"]), len(fx.b.restores["shop"]))
		}
	})
}

func TestMoveCopiesFilesThroughWorkspaceStorage(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.withFiles(true)
	req := httptest.NewRequest("GET", "/", nil)
	plan, _, _ := fx.c.computeMovePlan(req, "shop", moveStartInput{TargetServer: "beta"})
	if plan.Files[0].Method != moveFileMethodS3 {
		t.Fatalf("plan method %+v", plan.Files[0])
	}
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatal(status)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	key := "moves/" + view.ID + "/files/storage.tar.gz"
	fx.b.with(func() {
		if !slices.Equal(fx.b.fileFetches, []string{key}) || len(fx.b.fileRestores["storage"]) != 1 {
			t.Fatalf("fetches %v restores %d", fx.b.fileFetches, len(fx.b.fileRestores["storage"]))
		}
	})
	if !fx.b.called("PUT /v1/backup-storage") || fx.a.called("PUT /v1/backup-storage") {
		t.Fatal("workspace storage goes to the destination only; the source uses its own")
	}
	if fx.a.called("GET /v1/move/apps/shop/files/storage/archive") {
		t.Fatal("the archive streamed through the control panel although storage worked")
	}
	if job.Files[0].Used != moveFileMethodS3 {
		t.Fatalf("used %+v", job.Files[0])
	}
	if keys := fx.a.storage.keys(); len(keys) != 0 {
		t.Fatalf("move objects remain after the copy: %v", keys)
	}
	// When the destination cannot download, the archive streams directly.
	fx.b.with(func() { fx.b.failFetch = true })
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	job = waitMove(t, fx.c, view.ID, moveStateCutover)
	if job.Files[0].Used != moveFileMethodDirect || !strings.Contains(moveStepLog(job, moveStepFinalSync), "streaming the archive directly instead") {
		t.Fatalf("fallback: %+v\n%s", job.Files[0], moveStepLog(job, moveStepFinalSync))
	}
	fx.b.with(func() {
		if len(fx.b.fileRestores["storage"]) != 2 {
			t.Fatalf("fallback restores %d", len(fx.b.fileRestores["storage"]))
		}
	})
	if keys := fx.a.storage.keys(); len(keys) != 0 {
		t.Fatalf("move objects remain after the fallback: %v", keys)
	}
}

func TestMoveFinalSyncCopiesFilesWithTheSourceFrozen(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.withFiles(false)
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
	if status != 202 {
		t.Fatal(status)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	fx.a.with(func() { fx.a.fileData["storage"] = []byte("archive with the newest uploads") })
	if code := moveAction(t, fx.c, view.ID, "final-sync", nil); code != 202 {
		t.Fatal(code)
	}
	job := waitMove(t, fx.c, view.ID, moveStateCutover)
	if !job.FinalSynced || !job.SourceFrozen {
		t.Fatalf("final sync %+v", job)
	}
	fx.b.with(func() {
		got := fx.b.fileRestores["storage"]
		if len(got) != 2 || string(got[1]) != "archive with the newest uploads" {
			t.Fatalf("final files %q", got)
		}
	})
	freeze, archive := callIndex(fx.a, "PUT /v1/apps/shop/maintenance", false), callIndex(fx.a, "POST /v1/move/apps/shop/files/storage/archive", true)
	if freeze < 0 || archive < freeze {
		t.Fatalf("the final archive must be taken with the source frozen: freeze at %d, archive at %d", freeze, archive)
	}
	// Aborting removes any archive the move left behind.
	if code := moveAction(t, fx.c, view.ID, "abort", nil); code != 202 {
		t.Fatal(code)
	}
	waitMove(t, fx.c, view.ID, moveStateAborted)
	fx.a.with(func() {
		if len(fx.a.cleanups) != 3 {
			t.Fatalf("cleanups after the copy, the final sync and the abort: %v", fx.a.cleanups)
		}
	})
}

func TestMovePlanReportsFilesThatCannotMove(t *testing.T) {
	fx := newMoveFixture(t)
	fx.containerSource()
	fx.withFiles(false)
	fx.a.source.Files.Sets[0].Problems = []string{"etc (link pointing outside the set)"}
	fx.a.source.Files.Warnings = []string{"shared/public/system is a link to /mnt/volume outside the application directory; its files are not moved. Copy them to the destination manually."}
	req := httptest.NewRequest("GET", "/", nil)
	plan, _, _ := fx.c.computeMovePlan(req, "shop", moveStartInput{TargetServer: "beta"})
	if blockers := strings.Join(plan.Blockers, "\n"); plan.Files[0].Action != "blocked" || !strings.Contains(blockers, "etc (link pointing outside the set)") || !strings.Contains(blockers, "Links must stay inside the directory") {
		t.Fatalf("escaping links must block: %+v %v", plan.Files[0], plan.Blockers)
	}
	if !slices.Contains(plan.Warnings, fx.a.source.Files.Warnings[0]) {
		t.Fatalf("untransferable paths are named: %v", plan.Warnings)
	}
	// Sockets and pipes are runtime files the copy skips: counted in the
	// plan, never a blocker.
	fx.a.source.Files.Sets[0].Problems = nil
	fx.a.source.Files.Sets[0].Skipped = 21
	plan, _, _ = fx.c.computeMovePlan(req, "shop", moveStartInput{TargetServer: "beta"})
	if len(plan.Blockers) != 0 || plan.Files[0].Action != "copy" || plan.Files[0].Skipped != 21 {
		t.Fatalf("runtime files must not block: %+v %v", plan.Files[0], plan.Blockers)
	}
	if !strings.Contains(strings.Join(plan.Notes, "\n"), "they are not copied") {
		t.Fatalf("runtime files are explained: %v", plan.Notes)
	}
	// A source agent that still lists them as problems needs an upgrade.
	fx.a.source.Files.Sets[0].Problems = []string{"rspamd.sock (socket)"}
	plan, _, _ = fx.c.computeMovePlan(req, "shop", moveStartInput{TargetServer: "beta"})
	if blockers := strings.Join(plan.Blockers, "\n"); plan.Files[0].Action != "blocked" || !strings.Contains(blockers, "rspamd.sock (socket)") || !strings.Contains(blockers, "upgrade the agent on the source") {
		t.Fatalf("older source agents: %+v %v", plan.Files[0], plan.Blockers)
	}
	// Stack volumes have no counterpart in the native runtime.
	native := MovePlan{TargetRuntime: moveRuntimeNative}
	files := agent.MoveFiles{Warnings: []string{}}
	files.Sets = []agent.MoveFileSet{{ID: "stack-media", Path: "stack/media", Source: "stack volume lidza-stack-shop-media", Files: 3, Bytes: 10}}
	fx.c.planMoveFiles(&native, agent.MoveSource{Files: &files}, agent.MoveHost{Files: true})
	if native.Files[0].Action != "manual" || len(native.Warnings) != 1 || !strings.Contains(native.Warnings[0], "native runtime has no service volumes") {
		t.Fatalf("stack to native: %+v %v", native.Files, native.Warnings)
	}
	// Destinations that cannot receive files, and sources that cannot send
	// them, keep the manual warning and say why.
	old := MovePlan{TargetRuntime: moveRuntimeContainer}
	fx.c.planMoveFiles(&old, agent.MoveSource{Files: &files}, agent.MoveHost{})
	if !strings.Contains(strings.Join(old.Warnings, "\n"), "destination agent cannot receive files") {
		t.Fatalf("old destination: %v", old.Warnings)
	}
	legacy := MovePlan{}
	fx.c.planMoveFiles(&legacy, agent.MoveSource{Volumes: []string{"persistent storage volume lidza-storage-shop"}}, agent.MoveHost{Files: true})
	if len(legacy.Warnings) != 1 || !strings.Contains(legacy.Warnings[0], "source agent cannot transfer files") {
		t.Fatalf("old source: %v", legacy.Warnings)
	}
}

func TestMoveWarnsWhenDNSPointsToTheDestinationBeforeDeploying(t *testing.T) {
	fx := newMoveFixture(t)
	job := MoveJob{ID: "m1", AppID: "shop", SourceServer: "alpha", TargetServer: "beta", State: moveStateRunning, Hostnames: []string{"shop.example.com", "www.shop.example.com"}, Addresses: []string{moveTestTargetIP}}
	for _, s := range moveStepTitles {
		job.Steps = append(job.Steps, MoveStep{Key: s.key, Title: s.title, Status: "pending"})
	}
	job.step(moveStepPlan).Status, job.step(moveStepPrepare).Status = "done", "done"
	fx.c.data.Moves = map[string]MoveJob{"m1": job}
	fx.c.checkMoveEarlyDNS(context.Background(), "m1")
	if current, _ := fx.c.moveJob("m1"); current.DNSAhead || len(current.DNS) != 2 {
		t.Fatalf("DNS at the source: %+v", current)
	}
	fx.resolver.set("www.shop.example.com", moveTestTargetIP)
	for range 2 {
		fx.c.checkMoveEarlyDNS(context.Background(), "m1")
	}
	current, _ := fx.c.moveJob("m1")
	log := moveStepLog(current, moveStepEnvironment)
	if !current.DNSAhead || strings.Count(log, "already points to the destination") != 1 || !strings.Contains(log, "Switch DNS") {
		t.Fatalf("early DNS: ahead %t\n%s", current.DNSAhead, log)
	}
	// Once deployed, DNS at the destination is what the cutover waits for.
	fx.c.mu.Lock()
	deployed := fx.c.data.Moves["m1"]
	deployed.step(moveStepDeploy).Status = "done"
	deployed.DNSAhead = false
	fx.c.data.Moves["m1"] = deployed
	fx.c.mu.Unlock()
	fx.c.checkMoveEarlyDNS(context.Background(), "m1")
	if current, _ := fx.c.moveJob("m1"); current.DNSAhead {
		t.Fatal("after the deploy the early warning no longer applies")
	}
}

func TestMoveFileRelayHoldsBackMismatchingArchives(t *testing.T) {
	content := []byte(moveTestArchive)
	sum := sha256.Sum256(content)
	reader := &moveVerifyingReader{r: strings.NewReader(moveTestArchive + "!"), hash: sha256.New(), expected: hex.EncodeToString(sum[:]), size: int64(len(content)), mismatch: errMoveArchiveSum}
	buffer := make([]byte, 1024)
	total := 0
	var err error
	for err == nil {
		var n int
		n, err = reader.Read(buffer)
		total += n
	}
	if err != errMoveArchiveSum || total >= len(content) {
		t.Fatalf("err %v after %d bytes", err, total)
	}
}

// File archives use Zstandard only when both agents have the tool.
func TestMoveFileArchivesNegotiateZstd(t *testing.T) {
	for _, both := range []bool{false, true} {
		fx := newMoveFixture(t)
		fx.containerSource()
		fx.withFiles(false)
		fx.a.host.Zstd, fx.b.host.Zstd = true, both
		status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta"})
		if status != 202 {
			t.Fatalf("start: %d", status)
		}
		waitMove(t, fx.c, view.ID, moveStateCutover)
		want := ""
		if both {
			want = "zstd"
		}
		fx.a.with(func() {
			if len(fx.a.archives) == 0 || fx.a.archives[0].Compression != want {
				t.Fatalf("both=%t: archive requests %+v", both, fx.a.archives)
			}
		})
	}
}
