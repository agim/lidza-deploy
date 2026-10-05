package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza/packs/analytics"
	"github.com/agim/lidza/pkg/report"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAppErrorsRequireAgentKeyAndGiveSetupState(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	if err := m.Upsert(testApp("errors")); err != nil {
		t.Fatal(err)
	}
	request := func(key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/v1/apps/errors/errors", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		Handler(m).ServeHTTP(w, r)
		return w
	}
	if w := request("wrong"); w.Code != http.StatusUnauthorized {
		t.Fatal("unauthenticated errors exposed")
	}
	w := request(m.cfg.APIKey)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "needs_database") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing database not explained", w.Code, w.Body)
	}
}

func TestDockerFrameworkErrorsReadRedactAndIsolate(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1")
	}
	m := testManager(t, &fakeRuntime{})
	id := "errors-" + newID()[:10]
	network := "lidza-db-" + id
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	t.Cleanup(func() {
		m.Close()
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, args := range [][]string{{"rm", "-f", network}, {"volume", "rm", network + "-data"}, {"network", "rm", network}} {
			_, _ = command(cleanup, "", nil, "docker", args...)
		}
	})
	app := testApp(id)
	app.Env["APP_KIND"] = "server"
	if err := m.Upsert(app); err != nil {
		t.Fatal(err)
	}
	if err := m.Upsert(testApp("other-app")); err != nil {
		t.Fatal(err)
	}
	if err := m.configureDatabase(id, DatabaseRequest{Mode: "local", Backup: BackupPolicy{Keep: 1}}); err != nil {
		t.Fatal(err)
	}
	if d := waitDatabase(t, m, id); !d.Ready {
		t.Fatal("fixture database unavailable", d.Error)
	}
	if out, err := m.appErrors(ctx, id); err != nil || out.Status != "needs_analytics" {
		t.Fatal("missing analytics schema not explained", out.Status, err)
	}
	m.mu.Lock()
	d := m.data.Databases[id]
	m.mu.Unlock()
	env, flags := postgresEnvironment(d.URL)
	args := append([]string{"run", "--rm", "-i", "--network", network}, flags...)
	args = append(args, databaseImage, "psql", "-X", "-v", "ON_ERROR_STOP=1")
	if err := dockerStream(ctx, strings.NewReader(analytics.Tables), io.Discard, env, args...); err != nil {
		t.Fatal(err)
	}
	address, err := command(ctx, "", nil, "docker", "inspect", "--format", `{{with index .NetworkSettings.Networks "`+network+`"}}{{.IPAddress}}{{end}}`, network)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(d.URL)
	if err != nil {
		t.Fatal("invalid fixture URL")
	}
	cfg.ConnConfig.Host = address
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	reporter := analytics.New(analytics.Config{Queue: 16}, pool)
	reporter.Run()
	capture := report.WithLookup(ctx, func() report.Reporter { return reporter })
	for i := 0; i < 3; i++ {
		report.Capture(capture, report.Error{Source: "server", Message: "checkout failed runtime-secret", Stack: "handlers.Checkout runtime-secret", Route: "POST /checkout", Method: "POST", RequestID: "request-123", UserID: "private-user", URL: "/checkout?token=untracked-secret"})
	}
	if err := reporter.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	out, err := m.appErrors(ctx, id)
	if err != nil || out.Status != "ready" || len(out.Errors) != 3 {
		t.Fatal("framework errors not read", err, len(out.Errors))
	}
	data, _ := json.Marshal(out)
	for _, forbidden := range []string{"runtime-secret", "private-user", "untracked-secret"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatal("error dashboard exposed private data", forbidden)
		}
	}
	if !strings.Contains(string(data), "[redacted]") || out.Errors[0].Source != "server" || out.Errors[0].Fingerprint != out.Errors[1].Fingerprint {
		t.Fatal("redaction or framework grouping lost")
	}
	if other, err := m.appErrors(ctx, "other-app"); err != nil || len(other.Errors) != 0 || other.Status != "needs_database" {
		t.Fatal("errors crossed application assignments")
	}
	shared := testApp("shared-errors")
	shared.Env["DATABASE_URL"] = d.URL
	if err := m.Upsert(shared); err != nil {
		t.Fatal(err)
	}
	if sharedOut, err := m.appErrors(ctx, id); err != nil || !sharedOut.Shared {
		t.Fatal("shared database attribution was hidden", err)
	}
	// The framework API's query limit bounds the dashboard sample.
	if _, err := pool.Exec(ctx, `INSERT INTO app_error(source,message,fingerprint) SELECT 'client','browser failure','browser-group' FROM generate_series(1,505)`); err != nil {
		t.Fatal(err)
	}
	out, err = m.appErrors(ctx, id)
	if err != nil || len(out.Errors) != appErrorLimit {
		t.Fatal("sample limit not respected", err, len(out.Errors))
	}
	// JSON escaping can expand text; bound the encoded response too.
	if _, err := pool.Exec(ctx, "UPDATE app_error SET message=repeat('<',3000),stack=repeat('<',5000)"); err != nil {
		t.Fatal(err)
	}
	out, err = m.appErrors(ctx, id)
	payload, _ := json.Marshal(out)
	if err != nil || !out.Truncated || len(payload) > (2<<20)+2048 {
		t.Fatal("encoded response budget not enforced", err, len(payload))
	}
	// A schema permission error is an outage, not a false healthy/empty result.
	if _, err := pool.Exec(ctx, "ALTER TABLE app_error RENAME TO saved_error; CREATE VIEW app_error AS SELECT 1 AS id"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.appErrors(ctx, id); err == nil {
		t.Fatal("incompatible error schema silently ignored")
	}
}
