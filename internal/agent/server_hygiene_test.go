package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/updates"
)

// ---- upgrades wait for move transfers ----------------------------------------------

func TestUpgradeRefusedWhileMoveTransferRuns(t *testing.T) {
	m := channelHost(t)
	f := newBundleFixture(t, "v0.9.0-private.1")
	h := Handler(m)
	token := m.cfg.APIKey
	manifest, _ := json.Marshal(upgradeManifestRequest{Manifest: string(f.manifest), Signature: updates.Sign(f.key, f.manifest)})
	if w := call(t, h, token, "POST", "/v1/upgrade/manifest", "application/json", manifest); w.Code != 200 {
		t.Fatal("manifest refused", w.Code, w.Body)
	}
	if w := call(t, h, token, "PUT", "/v1/upgrade/bundle/"+f.version, "application/octet-stream", f.archive); w.Code != 200 {
		t.Fatal("archive refused", w.Code, w.Body)
	}
	// A transfer handler that blocks until released.
	entered, release := make(chan struct{}), make(chan struct{})
	transfer := m.moveTransfer(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(200)
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		transfer(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/move/apps/shop/clickhouse/exports/x", nil))
	}()
	<-entered
	w := call(t, h, token, "POST", "/v1/upgrade", "application/json", []byte(`{"version":"`+f.version+`"}`))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "move transfer") {
		t.Fatal("upgrade queued during a move transfer", w.Code, w.Body)
	}
	close(release)
	<-done
	if n := m.moveTransfers.Load(); n != 0 {
		t.Fatal("transfer counter not released", n)
	}
	if w := call(t, h, token, "POST", "/v1/upgrade", "application/json", []byte(`{"version":"`+f.version+`"}`)); w.Code != 202 {
		t.Fatal("upgrade refused after the transfer ended", w.Code, w.Body)
	}
}

// ---- disk reserve --------------------------------------------------------------------

func fakeDisk(t *testing.T, total, available uint64) {
	t.Helper()
	saved := diskStat
	diskStat = func(path string) (DiskHealth, error) {
		used := 100 * float64(total-available) / float64(total)
		return DiskHealth{Path: path, Total: total, Available: available, UsedPercent: used}, nil
	}
	t.Cleanup(func() { diskStat = saved })
}

func TestDiskReserveRefusesLargeOperations(t *testing.T) {
	const gib = 1 << 30
	if got := diskReserve(DiskHealth{Total: 10 * gib}); got != 2*gib {
		t.Fatal("small disks keep 2 GiB", got)
	}
	if got := diskReserve(DiskHealth{Total: 200 * gib}); got != 10*gib {
		t.Fatal("large disks keep 5%", got)
	}
	fakeDisk(t, 100*gib, 8*gib) // reserve 5 GiB
	if err := diskFits("/data", 2*gib, "the database dump"); err != nil {
		t.Fatal("fitting operation refused", err)
	}
	err := diskFits("/data", 4*gib, "the database dump")
	if err == nil || !strings.Contains(err.Error(), "the database dump") || !strings.Contains(err.Error(), "5.0 GiB reserve") {
		t.Fatal("operation below the reserve allowed or unclear", err)
	}
	if keepDiskReserve("/data") != nil {
		t.Fatal("disk above its reserve reported full")
	}
	fakeDisk(t, 100*gib, 5*gib)
	if !errors.Is(keepDiskReserve("/data"), errDiskReserve) {
		t.Fatal("disk at its reserve not stopped")
	}
	writer := &reserveWriter{dir: "/data", written: 64<<20 - 1}
	if _, err := writer.Write([]byte{1, 2}); !errors.Is(err, errDiskReserve) {
		t.Fatal("streaming write continued into the reserve", err)
	}
	if err := diskFits("/data", 0, "the database backup"); err == nil || !strings.Contains(err.Error(), "database backup") {
		t.Fatal("backup started inside the reserve", err)
	}
}

func TestBuildRefusedWhenDiskLow(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	fakeDisk(t, 20<<30, 2<<30) // reserve 2 GiB, a build needs 1 GiB more
	release, err := m.acquireBuildSlot(context.Background(), "shop", nil)
	if err == nil || !strings.Contains(err.Error(), "a build") {
		if release != nil {
			release()
		}
		t.Fatal("build started on a full disk", err)
	}
	// The slot was released: the next build with space proceeds.
	fakeDisk(t, 20<<30, 10<<30)
	release, err = m.acquireBuildSlot(context.Background(), "shop", nil)
	if err != nil {
		t.Fatal("build slot leaked after the refusal", err)
	}
	release()
}

func TestServerHealthWarnsAtEightyFivePercent(t *testing.T) {
	if !diskLow(DiskHealth{Total: 100, Available: 15, UsedPercent: 85}) || diskLow(DiskHealth{Total: 100 << 30, Available: 20 << 30, UsedPercent: 80}) {
		t.Fatal("low-disk threshold")
	}
	m := testManager(t, &fakeRuntime{})
	fakeDisk(t, 100<<30, 10<<30)
	health := m.serverHealth(context.Background())
	if !health.DiskLow || !slices.ContainsFunc(health.Warnings, func(w string) bool { return strings.Contains(w, "90% full") && strings.Contains(w, "refused") }) {
		t.Fatalf("no low-disk warning: %+v", health.Warnings)
	}
}

// ---- stale move transfer files -----------------------------------------------------

func writeAged(t *testing.T, path string, size int, modified time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func TestSweepRemovesStaleMoveTransferFiles(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	now := time.Now()
	saved := agentStarted
	agentStarted = now.Add(-time.Minute)
	t.Cleanup(func() { agentStarted = saved })
	before := now.Add(-2 * time.Hour)
	data := m.cfg.DataDir
	exportID, freshID := strings.Repeat("a", 64), strings.Repeat("b", 64)
	path := func(parts ...string) string { return filepath.Join(append([]string{data}, parts...)...) }
	// Interrupted export: spool directory of table files and the zero-byte archive.
	writeAged(t, path("move-clickhouse/exports/shop/.clickhouse-export-1/0001.native"), 4096, before)
	os.Chtimes(path("move-clickhouse/exports/shop/.clickhouse-export-1"), before, before)
	writeAged(t, path("move-clickhouse/exports/shop/.export-2"), 0, before)
	// Finished exports: one past its download window, one recent.
	writeAged(t, path("move-clickhouse/exports/shop", exportID+".tar"), 1024, now.Add(-72*time.Hour))
	writeAged(t, path("move-clickhouse/exports/shop", exportID+".json"), 10, now.Add(-72*time.Hour))
	writeAged(t, path("move-clickhouse/exports/shop", freshID+".tar"), 1024, now.Add(-time.Hour))
	writeAged(t, path("move-clickhouse/exports/shop", freshID+".json"), 10, now.Add(-time.Hour))
	// Interrupted upload and a file written by a transfer of this process.
	writeAged(t, path("move-clickhouse/restore/shop/.upload-3"), 2048, before)
	writeAged(t, path("move-clickhouse/restore/shop/.upload-live"), 2048, now)
	// An application whose export runs now keeps everything.
	writeAged(t, path("move-clickhouse/exports/busy/.export-4"), 512, before)
	lock := moveClickHouseLock("busy")
	lock.Lock()
	defer lock.Unlock()
	// File-set spool and database directories.
	writeAged(t, path("move-files/move-1/.archive-5"), 256, before)
	writeAged(t, path("move-files/move-1/storage.tar.gz"), 256, before)
	writeAged(t, path("backups/shop/.move-6"), 128, before)
	writeAged(t, path("backups/shop/kept.dump"), 128, before)
	writeAged(t, path("backups/restoring/.move-7"), 128, before)
	m.data.Databases["restoring"] = Database{AppID: "restoring", Operation: "restore"}

	preview := m.sweepMoveTemps(now, true)
	if len(preview.Removed) != 7 {
		t.Fatalf("dry run lists %v", preview.Removed)
	}
	if _, err := os.Stat(path("move-clickhouse/exports/shop/.export-2")); err != nil {
		t.Fatal("dry run removed a file")
	}
	result := m.sweepMoveTemps(now, false)
	want := []string{
		"move-clickhouse/exports/shop/.clickhouse-export-1", "move-clickhouse/exports/shop/.export-2",
		"move-clickhouse/exports/shop/" + exportID + ".tar", "move-clickhouse/exports/shop/" + exportID + ".json",
		"move-clickhouse/restore/shop/.upload-3", "move-files/move-1/.archive-5", "backups/shop/.move-6",
	}
	for _, rel := range want {
		if !slices.Contains(result.Removed, rel) {
			t.Fatalf("%s not removed: %v", rel, result.Removed)
		}
		if _, err := os.Stat(path(rel)); !os.IsNotExist(err) {
			t.Fatal("still present", rel)
		}
	}
	if result.Bytes != 4096+1024+10+2048+256+128 {
		t.Fatal("freed bytes", result.Bytes)
	}
	for _, kept := range []string{"move-clickhouse/exports/shop/" + freshID + ".tar", "move-clickhouse/restore/shop/.upload-live", "move-clickhouse/exports/busy/.export-4", "move-files/move-1/storage.tar.gz", "backups/shop/kept.dump", "backups/restoring/.move-7"} {
		if _, err := os.Stat(path(kept)); err != nil {
			t.Fatal("removed", kept)
		}
	}
	if message := result.message(); !strings.Contains(message, "7 stale move transfer file(s)") || !strings.Contains(message, "freeing") {
		t.Fatal("report", message)
	}
	// A running transfer of any kind defers the shared spools.
	writeAged(t, path("move-files/move-1/.incoming-x-1"), 64, before)
	m.moveTransfers.Add(1)
	if result := m.sweepMoveTemps(now, false); slices.Contains(result.Removed, "move-files/move-1/.incoming-x-1") {
		t.Fatal("spool swept during a transfer")
	}
	m.moveTransfers.Add(-1)
}

// ---- ClickHouse system logs -----------------------------------------------------------

type fakeSystemLogs struct {
	mu      sync.Mutex
	tables  map[string]string // name -> engine_full
	altered []string
}

func (f *fakeSystemLogs) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	sql := string(body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasPrefix(sql, "SELECT name, engine_full FROM system.tables"):
		for _, name := range slices.Sorted(func(yield func(string) bool) {
			for name := range f.tables {
				if !yield(name) {
					return
				}
			}
		}) {
			raw, _ := json.Marshal(map[string]string{"name": name, "engine_full": f.tables[name]})
			fmt.Fprintf(w, "%s\n", raw)
		}
	case strings.HasPrefix(sql, "ALTER TABLE system."):
		f.altered = append(f.altered, sql)
		name := strings.Fields(sql)[2]
		name = strings.Trim(strings.TrimPrefix(name, "system."), "`")
		days := strings.Fields(sql)[8]
		f.tables[name] = "MergeTree PARTITION BY toYYYYMM(event_date) ORDER BY event_time TTL event_date + toIntervalDay(" + days + ")"
	case strings.HasPrefix(sql, "SELECT toUInt64(sum(bytes_on_disk)) AS bytes FROM system.parts"):
		fmt.Fprintln(w, `{"bytes":3221225472}`)
	default:
		http.Error(w, "Code: 62. unexpected "+sql, 500)
	}
}

func TestClickHouseSystemLogRetention(t *testing.T) {
	config := renderClickHouseLogsConfig()
	for _, want := range []string{"<text_log><level>warning</level><ttl>event_date + INTERVAL 14 DAY DELETE</ttl></text_log>", "<trace_log><ttl>event_date + INTERVAL 3 DAY DELETE</ttl></trace_log>", "<query_views_log>", "<processors_profile_log>"} {
		if !strings.Contains(config, want) {
			t.Fatalf("container log policy lacks %s:\n%s", want, config)
		}
	}
	fake := &fakeSystemLogs{tables: map[string]string{
		"query_log":   "MergeTree PARTITION BY toYYYYMM(event_date) ORDER BY event_time",
		"query_log_0": "MergeTree PARTITION BY toYYYYMM(event_date) ORDER BY event_time",
		"trace_log":   "MergeTree ORDER BY event_time TTL event_date + toIntervalDay(30)",
		"part_log":    "MergeTree ORDER BY event_time TTL event_date + toIntervalDay(14)",
		"tables_seen": "MergeTree ORDER BY id", // not a system log
	}}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	defer server.Close()
	client := clickhouseClient{base: server.URL, user: "lidza", password: "secret"}
	changed, err := applyClickHouseLogTTL(context.Background(), client)
	slices.Sort(changed)
	if err != nil || !slices.Equal(changed, []string{"query_log", "query_log_0", "trace_log"}) {
		t.Fatal("changed", changed, err)
	}
	if !slices.Contains(fake.altered, "ALTER TABLE system.`trace_log` MODIFY TTL event_date + INTERVAL 3 DAY DELETE") {
		t.Fatal("trace_log not shortened", fake.altered)
	}
	// Idempotent: a second run sends no ALTER.
	fake.altered = nil
	if changed, err := applyClickHouseLogTTL(context.Background(), client); err != nil || len(changed) != 0 || len(fake.altered) != 0 {
		t.Fatal("repeated reconciliation altered tables", changed, fake.altered, err)
	}
	if size, err := clickhouseLogBytes(context.Background(), client); err != nil || size != 3<<30 {
		t.Fatal("system log size", size, err)
	}

	// The hourly tick reconciles each ready container and keeps its size.
	m := testManager(t, &fakeRuntime{})
	m.data.Apps["shop"] = testApp("shop")
	m.data.ClickHouses["shop"] = ClickHouseResource{Network: "lidza-clickhouse-shop", Password: "secret", Ready: true}
	savedService, savedConfig := containerClickHouseService, clickhouseContainerLogsConfig
	t.Cleanup(func() { containerClickHouseService, clickhouseContainerLogsConfig = savedService, savedConfig })
	written := 0
	clickhouseContainerLogsConfig = func(context.Context, ClickHouseResource) (bool, error) { written++; return true, nil }
	containerClickHouseService = func(context.Context, ClickHouseResource) (clickhouseService, error) {
		return clickhouseService{mode: "container", database: "telemetry", client: client}, nil
	}
	now := time.Now()
	m.clickhouseLogsTick(now)
	m.clickhouseLogsTick(now.Add(time.Minute)) // not due again within the hour
	if written != 1 || m.clickhouseLogSizes()["shop"] != 3<<30 {
		t.Fatal("tick", written, m.clickhouseLogSizes())
	}
	warnings := clickhouseLogWarnings(m.clickhouseLogSizes())
	if len(warnings) != 1 || !strings.Contains(warnings[0], "shop") || !strings.Contains(warnings[0], "3.0 GiB") {
		t.Fatal("warning", warnings)
	}
}

// ---- Docker disk hygiene ----------------------------------------------------------------

type fakeDockerHost struct {
	mu    sync.Mutex
	calls [][]string
	out   map[string]string
}

func (f *fakeDockerHost) run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	command := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, append([]string{name}, args...))
	for prefix, out := range f.out {
		if strings.HasPrefix(command, prefix) {
			return out, nil
		}
	}
	return "", nil
}

func (f *fakeDockerHost) ran(command string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.calls, func(c []string) bool { return strings.Join(c, " ") == command })
}

func TestDockerHygienePlansAndRemovesOnlyUnusedData(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	m.data.HostUpdates = &HostUpdatesState{Docker: &HostDockerState{InstalledByAgent: true}}
	shop := testApp("shop")
	shop.Current = &Release{ID: "d3", Image: "lidza/shop:d3"}
	shop.Previous = &Release{ID: "d2", Image: "lidza/shop:d2"}
	m.data.Apps["shop"] = shop
	api := testApp("shop-api")
	api.Current = &Release{ID: "a1", Image: "lidza/shop-api:a1"}
	m.data.Apps["shop-api"] = api
	saved, savedPath := hygieneHostRoot, hostPath
	root := t.TempDir()
	hygieneHostRoot = func() bool { return true }
	hostPath = func(path string) string { return filepath.Join(root, path) }
	t.Cleanup(func() { hygieneHostRoot, hostPath = saved, savedPath })
	// An apt host with a package cache, and volume directories to measure.
	writeAged(t, filepath.Join(root, "etc/os-release"), 0, time.Now())
	os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID=ubuntu\nID_LIKE=debian\n"), 0o644)
	os.MkdirAll(filepath.Join(root, "run/systemd/system"), 0o755)
	writeAged(t, filepath.Join(root, aptArchiveDir, "tool.deb"), 3000, time.Now())
	writeAged(t, filepath.Join(root, "volumes/state/data"), 5000, time.Now())
	writeAged(t, filepath.Join(root, "volumes/anon/data"), 700, time.Now())
	old := time.Now().Add(-30 * 24 * time.Hour).UTC().Format("2006-01-02 15:04:05 -0700 MST")
	recent := time.Now().Add(-10 * time.Minute).UTC().Format("2006-01-02 15:04:05 -0700 MST")
	anon := strings.Repeat("c", 64)
	fake := &fakeDockerHost{out: map[string]string{
		"docker container ls --all --quiet --no-trunc":                                    "c1\nc2\n",
		"docker container inspect --format {{.Image}}":                                    "sha256:used\nsha256:d3\n",
		"docker image ls --no-trunc --filter reference=lidza/*":                           strings.Join([]string{"sha256:d3\tlidza/shop\td3\t" + old, "sha256:d2\tlidza/shop\td2\t" + old, "sha256:d1\tlidza/shop\td1\t" + old, "sha256:used\tlidza/shop\td0\t" + old, "sha256:new\tlidza/shop\td4\t" + recent, "sha256:a0\tlidza/shop-api\ta0\t" + old, "sha256:a1\tlidza/shop-api\ta1\t" + old, "sha256:x\tlidza/retired\tr1\t" + old, "sha256:n\tlidza/shop\t<none>\t" + old}, "\n"),
		"docker image inspect --format {{.Size}} lidza/shop:d1":                           "1000",
		"docker image inspect --format {{.Size}} lidza/shop-api":                          "2000",
		"docker container ls --all --filter name=buildx_buildkit_":                        "buildx_buildkit_old0\nbuildx_buildkit_current0\nbuildx_buildkit_recent0\n",
		"docker buildx inspect":                                                           "Name:   current\nDriver: docker-container\n\nNodes:\nName:      current0\nEndpoint:  unix:///var/run/docker.sock\n",
		"docker container inspect --format {{.State.FinishedAt}} buildx_buildkit_old0":    time.Now().Add(-20 * 24 * time.Hour).UTC().Format(time.RFC3339Nano),
		"docker container inspect --format {{.State.FinishedAt}} buildx_buildkit_recent0": time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano),
		"docker volume ls --quiet --filter name=^buildx_buildkit_old0_state$":             "buildx_buildkit_old0_state\n",
		"docker volume inspect --format {{.Mountpoint}} buildx_buildkit_old0_state":       "/volumes/state",
		"docker volume ls --quiet --filter dangling=true":                                 anon + "\nnamed-data\n",
		"docker volume inspect --format {{.Mountpoint}} " + anon:                          "/volumes/anon",
	}}
	items, _, failures := m.planDiskHygiene(context.Background(), fake.run, time.Now(), dockerAvailability{ok: true})
	if len(failures) != 0 {
		t.Fatal(failures)
	}
	var names []string
	var total int64
	for _, item := range items {
		names = append(names, item.Kind+" "+item.Name)
		total += item.Bytes
	}
	slices.Sort(names)
	want := []string{"builder buildx_buildkit_old0 and volume buildx_buildkit_old0_state", "image lidza/shop-api:a0", "image lidza/shop:d1", "package-cache " + aptArchiveDir, "volume " + anon}
	if !slices.Equal(names, want) {
		t.Fatalf("plan:\n%v\nwant:\n%v", names, want)
	}
	if total != 1000+2000+5000+700+3000 {
		t.Fatal("planned bytes", total)
	}
	messages, errs := applyCleanupItems(context.Background(), fake.run, items)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, command := range []string{"docker image rm lidza/shop:d1", "docker image rm lidza/shop-api:a0", "docker container rm buildx_buildkit_old0", "docker volume rm buildx_buildkit_old0_state", "docker volume rm " + anon, "apt-get clean"} {
		if !fake.ran(command) {
			t.Fatal("not run:", command)
		}
	}
	for _, kept := range []string{"docker image rm lidza/shop:d2", "docker image rm lidza/shop:d3", "docker image rm lidza/shop:d0", "docker image rm lidza/shop:d4", "docker image rm lidza/retired:r1", "docker volume rm named-data", "docker container rm buildx_buildkit_current0", "docker container rm buildx_buildkit_recent0"} {
		if fake.ran(kept) {
			t.Fatal("removed data that must stay:", kept)
		}
	}
	if !strings.Contains(strings.Join(messages, "\n"), "freeing about 12 KiB") {
		t.Fatal("report", messages)
	}

	// The cleanup page's dry run lists the same items without removing them.
	dockerPresent(t, dockerAvailability{ok: true})
	m.maintenanceRun = (&fakeDockerHost{out: fake.out}).run
	preview := m.cleanupPreview(context.Background())
	if len(preview.Items) != len(items) || preview.Bytes != total {
		t.Fatalf("preview %+v", preview)
	}
	request := httptest.NewRequest("GET", "/v1/maintenance?preview=1", nil)
	request.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	w := httptest.NewRecorder()
	Handler(m).ServeHTTP(w, request)
	var status MaintenanceState
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil || status.Preview == nil || status.Preview.Bytes != total {
		t.Fatal("preview route", w.Code, w.Body)
	}
}
