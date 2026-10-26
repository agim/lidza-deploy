package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/agim/lidza/packs/storage"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitDatabase(t *testing.T, m *Manager, id string) DatabaseView {
	t.Helper()
	deadline := time.Now().Add(100 * time.Second)
	for time.Now().Before(deadline) {
		for _, d := range m.databaseViews() {
			if d.AppID == id && d.Operation == "" {
				return d
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("database operation timed out")
	return DatabaseView{}
}
func TestDockerDatabaseBackupRestore(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1")
	}
	m := testManager(t, &fakeRuntime{})
	id := "dbtest-" + newID()[:10]
	app := testApp(id)
	name := "lidza-db-" + id
	t.Cleanup(func() {
		m.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, args := range [][]string{{"rm", "-f", name}, {"volume", "rm", name + "-data"}, {"network", "rm", name}} {
			_, _ = command(ctx, "", nil, "docker", args...)
		}
	})
	if err := m.Upsert(app); err != nil {
		t.Fatal(err)
	}
	if err := m.configureDatabase(id, DatabaseRequest{Mode: "local", Backup: BackupPolicy{Hours: 24, Keep: 1}}); err != nil {
		t.Fatal(err)
	}
	d := waitDatabase(t, m, id)
	if !d.Ready || d.Error != "" {
		t.Fatalf("provision: %+v", d)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	m.mu.Lock()
	secret := m.data.Databases[id].URL
	runtimeURL := m.data.Apps[id].Env["DATABASE_URL"]
	m.mu.Unlock()
	if secret == "" || runtimeURL != secret {
		t.Fatal("database URL not attached")
	}
	var result bytes.Buffer
	env, flags := postgresEnvironment(secret)
	args := append([]string{"run", "--rm", "--network", name}, flags...)
	args = append(args, databaseImage, "psql", "-X", "-v", "ON_ERROR_STOP=1", "-tAc", "CREATE TABLE proof (value text); INSERT INTO proof VALUES ('restore-me'); SELECT rolsuper FROM pg_roles WHERE rolname=current_user")
	if err := dockerStream(ctx, nil, &result, env, args...); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(result.String()), "f") {
		t.Fatal("app role is a superuser")
	}
	ports, err := command(ctx, "", nil, "docker", "port", name)
	if err != nil || ports != "" {
		t.Fatal("database exposed a host port")
	}
	if err = m.PatchSettings(id, SettingsPatch{EnvChanges: map[string]*string{"DATABASE_URL": nil}}); err == nil {
		t.Fatal("managed URL could be removed")
	}
	for i := 0; i < 2; i++ {
		if err = m.databaseAction(id, "backup"); err != nil {
			t.Fatal(err)
		}
		d = waitDatabase(t, m, id)
		if d.Error != "" || len(d.Backups) != 1 {
			t.Fatalf("backup: %+v", d)
		}
	}
	record := d.Backups[0]
	path := filepath.Join(m.backupDir(id), record.ID+".dump")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != record.SHA256 || int64(len(raw)) != record.Size {
		t.Fatal("backup integrity metadata mismatch")
	}
	var archiveList bytes.Buffer
	if err := dockerStream(ctx, bytes.NewReader(raw), &archiveList, nil, "exec", "-i", name, "pg_restore", "--list"); err != nil {
		t.Fatal("backup archive is unreadable", err)
	}
	if !strings.Contains(archiveList.String(), "Compression: zstd") || !strings.Contains(archiveList.String(), "TABLE DATA public proof") {
		t.Fatal("full Zstandard archive required", archiveList.String())
	}
	files, _ := filepath.Glob(filepath.Join(m.backupDir(id), "*.dump"))
	if len(files) != 1 {
		t.Fatal("retention did not remove old file")
	}
	if _, err = command(ctx, "", nil, "docker", "exec", name, "createdb", "-U", "postgres", "-O", "app", "restored"); err != nil {
		t.Fatal(err)
	}
	if err = dockerStream(ctx, bytes.NewReader(raw), io.Discard, nil, "exec", "-i", name, "pg_restore", "-U", "postgres", "--role=app", "--no-owner", "--no-acl", "--exit-on-error", "-d", "restored"); err != nil {
		t.Fatal("restore failed", err)
	}
	proof, err := command(ctx, "", nil, "docker", "exec", name, "psql", "-U", "postgres", "-d", "restored", "-tAc", "SELECT value FROM proof")
	if err != nil || proof != "restore-me" {
		t.Fatal("restored data mismatch")
	}
	state, _ := os.ReadFile(filepath.Join(m.cfg.DataDir, "state.json"))
	views, _ := json.Marshal(m.databaseViews())
	if bytes.Contains(state, []byte(secret)) || bytes.Contains(views, []byte(secret)) {
		t.Fatal("database secret leaked")
	}
	// Backup download must authenticate, and an S3 outage must preserve a local copy.
	req := httptest.NewRequest("GET", "/v1/apps/"+id+"/backups/"+record.ID, nil)
	w := httptest.NewRecorder()
	Handler(m).ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal("unauthenticated backup download", w.Code)
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	w = httptest.NewRecorder()
	Handler(m).ServeHTTP(w, req)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), raw) {
		t.Fatal("backup download differs")
	}
	initial, err := m.Enqueue(id, DeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if waitDeployment(t, m, initial.ID).Status != "live" {
		t.Fatal("initial deployment failed")
	}
	originalRelease := m.Current(id).ID
	retainedBeforeLock := m.databaseViews()[0].Backups[0]
	retainedPath := filepath.Join(m.backupDir(id), retainedBeforeLock.ID+".dump")
	retainedBytes, err := os.ReadFile(retainedPath)
	if err != nil {
		t.Fatal(err)
	}
	// A real exclusive table lock must fail pg_dump promptly, not leave the
	// deployment labeled building for the two-hour database operation window.
	lockCtx, stopLock := context.WithCancel(ctx)
	defer stopLock()
	lockDone := make(chan error, 1)
	go func() {
		_, e := command(lockCtx, "", nil, "docker", "exec", "-e", "PGAPPNAME=lidza-backup-lock-test", name, "psql", "-X", "-U", "postgres", "-d", "app", "-c", "BEGIN; LOCK TABLE proof IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(60)")
		lockDone <- e
	}()
	locked := false
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
		out, e := command(ctx, "", nil, "docker", "exec", name, "psql", "-X", "-U", "postgres", "-d", "app", "-tAc", "SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a USING (pid) WHERE a.application_name='lidza-backup-lock-test' AND l.mode='AccessExclusiveLock' AND l.granted")
		if e == nil && strings.TrimSpace(out) == "1" {
			locked = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !locked {
		t.Fatal("exclusive lock fixture did not acquire its lock")
	}
	blockedByLock, err := m.Enqueue(id, DeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var lockDeployment Deployment
	for end := time.Now().Add(40 * time.Second); time.Now().Before(end); {
		for _, item := range m.Deployments() {
			if item.ID == blockedByLock.ID {
				lockDeployment = item
			}
		}
		if lockDeployment.Status == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	_, _ = command(ctx, "", nil, "docker", "exec", name, "psql", "-X", "-U", "postgres", "-d", "app", "-tAc", "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name='lidza-backup-lock-test'")
	stopLock()
	<-lockDone
	if lockDeployment.Status != "failed" || !strings.Contains(lockDeployment.Error, "lock wait exceeded") || !strings.Contains(lockDeployment.Log, "Dumping PostgreSQL") || m.Current(id).ID != originalRelease {
		t.Fatal("locked backup was not bounded with useful failure details", lockDeployment)
	}
	if current := m.databaseViews()[0]; current.Operation != "" || len(current.Backups) != 1 || current.Backups[0].ID != retainedBeforeLock.ID {
		t.Fatal("locked dump changed retained backup", current)
	}
	if currentBytes, e := os.ReadFile(retainedPath); e != nil || !bytes.Equal(currentBytes, retainedBytes) {
		t.Fatal("locked dump changed retained backup bytes", e)
	}
	// A controlled endpoint with an untrusted certificate fails promptly. An
	// arbitrary unused port may be silently dropped by a host firewall instead
	// of refusing the connection, making this preservation test nondeterministic.
	unavailableS3 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unavailableS3.Close()
	store := storage.Config{Provider: "s3", Endpoint: unavailableS3.URL, Bucket: "test", Region: "us-east-1", AccessKey: "test", SecretKey: "test"}
	if err = m.setBackupStorage(&store); err != nil {
		t.Fatal(err)
	}
	if err = m.setBackupPolicy(id, BackupPolicy{Hours: 24, Keep: 1, Offsite: true}); err != nil {
		t.Fatal(err)
	}
	if err = m.databaseAction(id, "backup"); err != nil {
		t.Fatal(err)
	}
	d = waitDatabase(t, m, id)
	if !strings.Contains(d.Error, "local backup saved") || len(d.Backups) != 1 || d.Backups[0].Offsite {
		t.Fatal("failed off-site backup lost the local copy")
	}
	blocked, err := m.Enqueue(id, DeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if waitDeployment(t, m, blocked.ID).Status != "failed" || m.Current(id).ID != originalRelease {
		t.Fatal("failed pre-deployment backup activated a release")
	}
	d = waitDatabase(t, m, id)
	path = filepath.Join(m.backupDir(id), d.Backups[0].ID+".dump")
	if err = m.Retire(ctx, id); err != nil {
		t.Fatal(err)
	}
	if !m.databaseViews()[0].Retained {
		t.Fatal("retained data hidden")
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("retirement deleted backup")
	}
	if err = m.Upsert(app); err != nil {
		t.Fatal("application ID unavailable after removal", err)
	}
	if m.data.Apps[id].resourceID() == id || len(m.data.Apps[id].Bindings) != 0 || len(m.data.Databases[id].Backups) == 0 {
		t.Fatal("recreated application inherited or lost retained database data")
	}
}
func TestExternalDatabaseValidation(t *testing.T) {
	for _, url := range []string{"postgres://user:password@db.example.com/app?sslmode=disable", "postgres://user:password@db.example.com/app?sslmode=require&sslmode=disable", "mysql://user:password@db.example.com/app", "postgres://user:password@db.example.com/", "not a URL"} {
		if (DatabaseRequest{Mode: "external", URL: url, Backup: BackupPolicy{Keep: 7}}).Validate() == nil {
			t.Fatal("unsafe external URL accepted")
		}
	}
	if err := (DatabaseRequest{Mode: "external", URL: "postgres://user:password@db.example.com/app?sslmode=require", Backup: BackupPolicy{Hours: 24, Keep: 7}}).Validate(); err != nil {
		t.Fatal(err)
	}
}
func TestBackupStreamingS3(t *testing.T) {
	body := bytes.Repeat([]byte("backup-fixture"), 10000)
	file := filepath.Join(t.TempDir(), "backup.dump")
	if err := os.WriteFile(file, body, 0600); err != nil {
		t.Fatal(err)
	}
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/private/prefix/databases/app/backup.dump" {
			t.Error("wrong object path")
		}
		switch r.Method {
		case "PUT":
			if r.URL.Query().Get("X-Amz-Signature") == "" {
				t.Error("unsigned upload")
			}
			received, _ = io.ReadAll(r.Body)
			w.WriteHeader(200)
		case "HEAD":
			w.Header().Set("Content-Length", "140000")
			w.WriteHeader(200)
		default:
			t.Error("unexpected storage operation")
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	cfg := storage.Config{Provider: "s3", Endpoint: server.URL, Bucket: "private", Region: "us-east-1", Prefix: "prefix", AccessKey: "fixture-access", SecretKey: "fixture-secret"}
	if err := uploadBackup(context.Background(), cfg, file, "databases/app/backup.dump", int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, received) {
		t.Fatal("upload bytes changed")
	}
}
