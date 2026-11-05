package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func nativeFixture(t *testing.T) (*Manager, string) {
	t.Helper()
	m := testManager(t, &fakeRuntime{})
	root := t.TempDir()
	app := filepath.Join(root, "MyRails")
	release := filepath.Join(app, "releases", "one")
	if err := os.MkdirAll(release, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "Gemfile"), []byte("source 'https://rubygems.org'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(release, filepath.Join(app, "current")); err != nil {
		t.Fatal(err)
	}
	units := filepath.Join(root, ".config", "systemd", "user")
	if err := os.MkdirAll(units, 0700); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []string{"myrails-server.service", "myrails-worker.service"} {
		if err := os.WriteFile(filepath.Join(units, unit), []byte("[Service]\nWorkingDirectory="+filepath.Join(app, "current")+"\nEnvironmentFile=-"+filepath.Join(app, ".asdf-vars")+"\nExecStart=/app/private-command\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(units, "unrelated.service"), []byte("WorkingDirectory=/other/app\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m.cfg.Native = &NativeHostConfig{Root: root, User: "agim"}
	return m, app
}

func TestNativeRegistrationIsMetadataOnlyAndSeparateFromContainers(t *testing.T) {
	m, app := nativeFixture(t)
	calls := 0
	m.nativeCommand = func(context.Context, []string) (string, error) { calls++; return "", nil }
	before, _ := os.Readlink(filepath.Join(app, "current"))
	a, err := m.nativeRegistration(NativeApplication{ID: "my-rails", Name: "MyRails", Domain: "rails.example.com", Repository: "acme/rails"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || len(a.Services) != 2 || len(m.Apps()) != 0 || len(m.nativeViews()) != 1 {
		t.Fatalf("registration acquired runtime ownership: %+v", a)
	}
	if after, _ := os.Readlink(filepath.Join(app, "current")); after != before {
		t.Fatal("current release changed")
	}
	if _, found := m.data.StorageVolumes[a.ID]; found {
		t.Fatal("native app provisioned a Docker volume")
	}
	if err := m.Upsert(App{ID: a.ID, Repository: "acme/rails", Branch: "main", Domain: "other.example.com"}); err == nil {
		t.Fatal("container app reused native identity")
	}
	m.mu.Lock()
	err = m.load()
	m.mu.Unlock()
	if err != nil || len(m.nativeViews()) != 1 {
		t.Fatal("native binding did not survive encrypted-state reload", err)
	}
}

func TestNativeServiceActionsRequireApprovalAndExactBinding(t *testing.T) {
	m, _ := nativeFixture(t)
	_, err := m.nativeRegistration(NativeApplication{ID: "my-rails", Name: "MyRails", Domain: "rails.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	calls := [][]string{}
	m.nativeCommand = func(_ context.Context, args []string) (string, error) {
		calls = append(calls, slices.Clone(args))
		return "", nil
	}
	if err := m.nativeServiceAction(context.Background(), "my-rails", "myrails-server.service", "restart", "MyRails"); err == nil || len(calls) != 0 {
		t.Fatal("service changed before host configuration approval")
	}
	m.cfg.Native.ServiceControl = true
	for _, test := range []struct{ unit, action, confirm string }{{"unrelated.service", "restart", "MyRails"}, {"myrails-server.service", "enable", "MyRails"}, {"myrails-server.service", "restart", "other"}} {
		if err := m.nativeServiceAction(context.Background(), "my-rails", test.unit, test.action, test.confirm); err == nil {
			t.Fatal("unapproved action accepted")
		}
	}
	if len(calls) != 0 {
		t.Fatal("invalid request executed a command")
	}
	if err := m.nativeServiceAction(context.Background(), "my-rails", "myrails-server.service", "start", "MyRails"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !slices.Equal(calls[0], []string{"systemctl", "--user", "--no-block", "start", "myrails-server.service"}) {
		t.Fatal(calls)
	}
}

func TestNativeLogsMaskExistingEnvironmentAndRejectChangedUnits(t *testing.T) {
	m, app := nativeFixture(t)
	_, err := m.nativeRegistration(NativeApplication{ID: "my-rails", Name: "MyRails", Domain: "rails.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, ".asdf-vars"), []byte("PORT=9180\nRAILS_MAX_THREADS=1\nRAILS_LOG_TO_STDOUT=true\nRAILS_ENV=production\nexport SECRET_KEY_BASE='original-key-value'\nDATABASE_URL=postgres://user:database-password@db/app\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m.nativeCommand = func(_ context.Context, args []string) (string, error) {
		if args[0] != "journalctl" {
			t.Fatal("unexpected command", args)
		}
		return "[ActiveJob] [ExternalDomainCheckJob] 12345678-1234-1234-1234-123456789abc branch=main port=9180 original-key-value database-password", nil
	}
	log, err := m.nativeLogs(context.Background(), "my-rails", "myrails-server.service")
	if err != nil || strings.Contains(log, "original-key-value") || strings.Contains(log, "database-password") {
		t.Fatal(log, err)
	}
	if !strings.Contains(log, "ExternalDomainCheckJob") || !strings.Contains(log, "branch=main port=9180") || !strings.Contains(log, "12345678-1234-1234-1234-123456789abc") {
		t.Fatal("public release metadata corrupted job names", log)
	}
	data, _ := json.Marshal(m.nativeViews())
	if strings.Contains(string(data), "SECRET_KEY_BASE") || strings.Contains(string(data), "private-command") {
		t.Fatal("native metadata exposed env or command content")
	}
	file := filepath.Join(m.cfg.Native.Root, ".config", "systemd", "user", "myrails-server.service")
	if err := os.WriteFile(file, []byte("WorkingDirectory=/unrelated/current\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.nativeLogs(context.Background(), "my-rails", "myrails-server.service"); err == nil {
		t.Fatal("changed service binding accepted")
	}
}

func TestNativeLogsMaskShortCredentialValues(t *testing.T) {
	m, app := nativeFixture(t)
	if err := os.WriteFile(filepath.Join(app, ".asdf-vars"), []byte("SERVICE_PASSWORD=main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if output := scrubOutput("password=main", m.nativeSecrets(NativeApplication{Name: "MyRails"})); strings.Contains(output, "main") {
		t.Fatal("a short configured credential was not masked")
	}
}

func TestNativeDiscoveryRejectsEscapingPathsAndRootUser(t *testing.T) {
	m, app := nativeFixture(t)
	if _, err := m.nativeBinding("../other"); err == nil {
		t.Fatal("path traversal accepted")
	}
	if err := os.Remove(filepath.Join(app, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(app, "current")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.nativeBinding("MyRails"); err == nil {
		t.Fatal("escaping current release accepted")
	}
	if err := (NativeHostConfig{Root: m.cfg.Native.Root, User: "root"}).validate(); err == nil {
		t.Fatal("root app execution accepted")
	}
}
