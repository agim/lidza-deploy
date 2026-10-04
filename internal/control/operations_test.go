package control

import (
	"context"
	"encoding/json"
	"github.com/agim/lidza"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/auth"
	"github.com/agim/lidza/packs/db"
	"github.com/agim/lidza/packs/jobs"
	"github.com/agim/lidza/packs/mail"
	"github.com/agim/lidza/pkg/credentials"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestScheduledBackupsAndDurableAlerts(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL")
	}
	t.Chdir(t.TempDir())
	for _, key := range []string{"MAIL_PROVIDER", "MAIL_FROM", "MAIL_SMTP_HOST", "MAIL_SMTP_PORT", "MAIL_SMTP_USERNAME", "MAIL_SMTP_PASSWORD", "MAIL_SMTP_SECURITY", "MAIL_SMTP_URL"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("JOBS_WORKERS", "0")
	t.Setenv("AUTH_SECRET", strings.Repeat("s", 64))
	t.Setenv("LIDZA_MASTER_KEY", strings.Repeat("ab", 32))
	t.Setenv("APP_URL", "http://127.0.0.1:3000")
	if _, err := credentials.Generate("."); err != nil {
		t.Fatal(err)
	}
	failed := true
	dispatched := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/databases":
			failure := ""
			if failed {
				failure = "fixture backup failed"
			}
			json.NewEncoder(w).Encode([]agent.DatabaseView{{AppID: "ops-fixture", Mode: "local", Ready: true, Error: failure, Backup: agent.BackupPolicy{Hours: 24, Keep: 7}, NextBackup: time.Now().Add(-time.Minute)}})
		case "/v1/apps/ops-fixture/database/backup":
			dispatched++
			io.WriteString(w, `{"status":"queued"}`)
		case "/v1/app-health":
			state := "healthy"
			if failed {
				state = "unhealthy"
			}
			json.NewEncoder(w).Encode([]agent.AppHealth{{AppID: "ops-fixture", State: state}})
		case "/v1/deployments":
			io.WriteString(w, `[]`)
		default:
			t.Error("unexpected agent path", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()
	cfg := Config{PublicURL: "http://127.0.0.1:3000", User: "operator@example.com", Password: "a-long-local-password-123", Key: []byte(strings.Repeat("k", 32)), DataDir: t.TempDir(), Servers: []Server{{ID: "ops-host", Name: "Ops host", URL: remote.URL, Token: strings.Repeat("a", 32)}}}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.data.Apps["ops-fixture"] = Application{ID: "ops-fixture", ServerID: "ops-host"}
	boot, err := lidza.Boot(context.Background(), lidza.App{Name: "ops-test", Packs: []lidza.Pack{db.Pack(), auth.Pack(), jobs.Pack(), mail.Pack()}, OnStart: c.Start})
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close(context.Background())
	ctx := lidza.WithServices(context.Background(), boot.Services)
	pool := db.From(ctx)
	// Isolate mail assertions by a unique recipient; no workers or external sends.
	c.cfg.User = "ops-" + random()[:12] + "@example.com"
	defer func() {
		pool.Exec(context.Background(), "DELETE FROM job WHERE kind=$1 AND payload->>'id' IN (SELECT id::text FROM mail_message WHERE recipient=$2)", mail.JobKind, c.cfg.User)
		pool.Exec(context.Background(), "DELETE FROM mail_message WHERE recipient=$1", c.cfg.User)
	}()
	call := func(fn http.HandlerFunc, method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/", strings.NewReader(body)).WithContext(ctx)
		w := httptest.NewRecorder()
		fn(w, r)
		return w
	}
	if w := call(c.configureMail, "PUT", `{"host":"smtp.example.invalid","port":587,"security":"starttls","from":"alerts@example.com","username":"fixture","password":"fixture-smtp-secret"}`); w.Code != 200 {
		t.Fatal("configure mail", w.Code, w.Body)
	}
	if w := call(c.testMail, "POST", `{}`); w.Code != 202 {
		t.Fatal("test email not queued", w.Code, w.Body)
	}
	count := func() int {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM mail_message WHERE recipient=$1", c.cfg.User).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 1 {
		t.Fatal("test email missing")
	}
	for i := 0; i < 3; i++ {
		if err = c.operationsTick(ctx, nil); err != nil {
			t.Fatal(err)
		}
	}
	if dispatched != 3 || count() != 3 {
		t.Fatal("expected scheduled dispatches, one database alert, one repeated-readiness alert", dispatched, count())
	}
	c2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c2.cfg.User = c.cfg.User
	if err = c2.operationsTick(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if count() != 3 {
		t.Fatal("restart duplicated active alerts")
	}
	failed = false
	if err = c2.operationsTick(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if count() != 5 {
		t.Fatal("recovery emails missing", count())
	}
	if w := call(c.infrastructure, "GET", ""); w.Code != 200 || strings.Contains(w.Body.String(), "fixture-smtp-secret") {
		t.Fatal("infrastructure exposes credentials")
	}
}
