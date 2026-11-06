package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza/packs/storage"
)

func TestBeforeDeployPolicyValidation(t *testing.T) {
	valid := []BeforeDeployPolicy{{Mode: "always"}, {Mode: "never"}, {Mode: "reuse", MaxAgeHours: 1}, {Mode: "reuse", MaxAgeHours: 720}}
	for _, p := range valid {
		if err := p.Validate(); err != nil {
			t.Fatal(p, err)
		}
	}
	invalid := []BeforeDeployPolicy{{}, {Mode: "sometimes"}, {Mode: "reuse"}, {Mode: "reuse", MaxAgeHours: 721}, {Mode: "reuse", MaxAgeHours: -1}, {Mode: "always", MaxAgeHours: 24}, {Mode: "never", MaxAgeHours: 1}}
	for _, p := range invalid {
		if err := p.Validate(); err == nil {
			t.Fatal("accepted", p)
		}
	}
	if (BeforeDeployPolicy{}).Normalized().Mode != "always" || (BeforeDeployPolicy{Mode: "bogus"}).Normalized().Mode != "always" {
		t.Fatal("zero and invalid policies must resolve to always")
	}
	for policy, want := range map[BeforeDeployPolicy]string{{}: "always", {Mode: "never"}: "never", {Mode: "reuse", MaxAgeHours: 24}: "reuse within 24h"} {
		if got := policy.String(); got != want {
			t.Fatal(got, want)
		}
	}
	for d, want := range map[time.Duration]string{30 * time.Minute: "30m", 2*time.Hour + 20*time.Minute: "2h", 47 * time.Hour: "47h", 49 * time.Hour: "2d"} {
		if got := ageText(d); got != want {
			t.Fatal(d, got, want)
		}
	}
}

// beforeDeployFixture is a container application with a primary database
// (DATABASE_URL) and a secondary one, each with one verified local backup
// taken age ago.
func beforeDeployFixture(t *testing.T, age time.Duration) (*Manager, App) {
	t.Helper()
	m := testManager(t, &fakeRuntime{})
	app := testApp("shop")
	app.Bindings = map[string]string{"DATABASE_URL": "shop", "EVENTS_DATABASE_URL": "shop-events"}
	m.mu.Lock()
	m.data.Apps[app.ID] = app
	for _, id := range []string{"shop", "shop-events"} {
		record := BackupRecord{ID: "b-" + id, Created: time.Now().UTC().Add(-age), Size: 2048, SHA256: strings.Repeat("c", 64), Seconds: 3}
		m.data.Databases[id] = Database{AppID: id, Ready: true, URL: "postgres://app:fixture-secret@db/" + id, Backups: []BackupRecord{record}}
		if err := os.MkdirAll(m.backupDir(id), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(m.backupDir(id), record.ID+".dump"), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m.mu.Unlock()
	return m, app
}

func TestBeforeDeployDecisionsFollowThePolicyAndTheBackupAge(t *testing.T) {
	m, _ := beforeDeployFixture(t, 23*time.Hour+59*time.Minute)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	reuse := BeforeDeployPolicy{Mode: "reuse", MaxAgeHours: 24}
	set := func(id string, p BeforeDeployPolicy, edit func(*Database)) {
		d := m.data.Databases[id]
		d.BeforeDeploy = p
		if edit != nil {
			edit(&d)
		}
		m.data.Databases[id] = d
	}
	// Default: always, a fresh backup, for both databases.
	if d := m.beforeDeployDecisionLocked("shop-events", false, now); d.Policy.Mode != "always" || d.Skip || d.Reuse.ID != "" || d.Reason != "" {
		t.Fatal("default decision", d)
	}
	if !m.databasePrimaryLocked("shop") || m.databasePrimaryLocked("shop-events") {
		t.Fatal("DATABASE_URL marks the primary database")
	}
	// Reuse: a verified backup newer than 24 hours counts.
	set("shop-events", reuse, nil)
	if d := m.beforeDeployDecisionLocked("shop-events", false, now); d.Reuse.ID != "b-shop-events" || d.Age < 23*time.Hour || d.Skip || d.Reason != "" {
		t.Fatal("recent backup not reused", d)
	}
	// Exactly 24 hours old is not newer than 24 hours.
	set("shop-events", reuse, func(d *Database) { d.Backups[0].Created = now.Add(-24 * time.Hour) })
	if d := m.beforeDeployDecisionLocked("shop-events", false, now); d.Reuse.ID != "" || d.Reason != "the newest verified backup is 24h old" {
		t.Fatal("boundary", d)
	}
	// A failed upload, a missing off-site copy when one is required, a
	// missing local file and a database error all mean a fresh backup.
	set("shop-events", reuse, func(d *Database) {
		d.Backups[0].Created = now.Add(-time.Hour)
		d.Backups[0].UploadError = "upload failed"
	})
	if d := m.beforeDeployDecisionLocked("shop-events", false, now); d.Reuse.ID != "" || d.Reason != "no verified backup yet" {
		t.Fatal("failed upload reused", d)
	}
	set("shop-events", reuse, func(d *Database) { d.Backups[0].Created = now.Add(-time.Hour); d.Backups[0].UploadError = "" })
	if d := m.beforeDeployDecisionLocked("shop-events", true, now); d.Reuse.ID != "" || d.Reason != "no verified backup with an off-site copy yet" {
		t.Fatal("local-only backup reused where off-site is required", d)
	}
	set("shop-events", reuse, func(d *Database) { d.Backups[0].Offsite = true })
	if d := m.beforeDeployDecisionLocked("shop-events", true, now); d.Reuse.ID != "b-shop-events" {
		t.Fatal("off-site backup not reused", d)
	}
	set("shop-events", reuse, func(d *Database) { d.Backups[0].Offsite = false })
	if err := os.Remove(filepath.Join(m.backupDir("shop-events"), "b-shop-events.dump")); err != nil {
		t.Fatal(err)
	}
	if d := m.beforeDeployDecisionLocked("shop-events", false, now); d.Reuse.ID != "" || !strings.Contains(d.Reason, "no longer on this server") {
		t.Fatal("pruned local backup reused", d)
	}
	set("shop-events", reuse, func(d *Database) { d.Backups[0].Offsite = true; d.Error = "Database operation failed: fixture" })
	if d := m.beforeDeployDecisionLocked("shop-events", false, now); d.Reuse.ID != "" || d.Reason != "its last database operation failed" {
		t.Fatal("database with an error reused", d)
	}
	// Never: a secondary database is skipped; a primary one stays always.
	set("shop-events", BeforeDeployPolicy{Mode: "never"}, nil)
	if d := m.beforeDeployDecisionLocked("shop-events", false, now); !d.Skip {
		t.Fatal("secondary never not skipped", d)
	}
	set("shop", BeforeDeployPolicy{Mode: "never"}, nil)
	if d := m.beforeDeployDecisionLocked("shop", false, now); d.Skip || d.Policy.Mode != "always" {
		t.Fatal("primary resolved to never", d)
	}
}

// The reviewed configuration is the initial default, legacy skip flag
// included; what the panel saved in state takes over.
func TestBeforeDeployPolicyPrecedence(t *testing.T) {
	m, _, p, _ := nativeDatabaseFixture(t, "postgresql://fixture:password@127.0.0.1:15999/native_fixture")
	p.Roles = []string{"primary"}
	scraper := p
	scraper.Roles, scraper.EnvironmentKey = []string{"queue"}, "QUEUE_DATABASE_URL"
	bound := scraper
	m.cfg.Native.Databases = map[string]NativeDatabasePolicy{"native-fixture": p, "native-scraper": scraper}
	m.data.NativeDatabases = map[string]NativeDatabaseBinding{"native-fixture": {Policy: p}, "native-scraper": {Policy: bound}}
	m.data.Databases = map[string]Database{"native-fixture": {AppID: "native-fixture", Mode: "external", Ready: true}, "native-scraper": {AppID: "native-scraper", Mode: "external", Ready: true}}
	m.mu.Lock()
	defer m.mu.Unlock()
	if got := m.beforeDeployPolicyLocked("native-scraper"); got.Mode != "always" {
		t.Fatal("default", got)
	}
	// Legacy flag in the configuration: never, even when the binding adopted
	// earlier does not carry it.
	scraper.SkipReleaseBackup = true
	m.cfg.Native.Databases["native-scraper"] = scraper
	if got := m.beforeDeployPolicyLocked("native-scraper"); got.Mode != "never" {
		t.Fatal("legacy flag", got)
	}
	// before_deploy in the configuration replaces the legacy flag.
	scraper.BeforeDeploy = &BeforeDeployPolicy{Mode: "reuse", MaxAgeHours: 12}
	m.cfg.Native.Databases["native-scraper"] = scraper
	if got := m.beforeDeployPolicyLocked("native-scraper"); got != (BeforeDeployPolicy{Mode: "reuse", MaxAgeHours: 12}) {
		t.Fatal("configured policy", got)
	}
	// The panel's saved choice wins over the configuration.
	d := m.data.Databases["native-scraper"]
	d.BeforeDeploy = BeforeDeployPolicy{Mode: "always"}
	m.data.Databases["native-scraper"] = d
	if got := m.beforeDeployPolicyLocked("native-scraper"); got.Mode != "always" {
		t.Fatal("state must win", got)
	}
	// A primary is always protected whatever state says.
	primary := m.data.Databases["native-fixture"]
	primary.BeforeDeploy = BeforeDeployPolicy{Mode: "never"}
	m.data.Databases["native-fixture"] = primary
	if got := m.beforeDeployPolicyLocked("native-fixture"); got.Mode != "always" || !m.databasePrimaryLocked("native-fixture") {
		t.Fatal("primary", got)
	}
	// A configuration that sets never on a primary database, or an invalid
	// policy, is refused; the legacy flag follows the same rule.
	for _, bad := range []NativeDatabasePolicy{
		func() NativeDatabasePolicy { q := p; q.BeforeDeploy = &BeforeDeployPolicy{Mode: "never"}; return q }(),
		func() NativeDatabasePolicy { q := p; q.SkipReleaseBackup = true; return q }(),
		func() NativeDatabasePolicy { q := p; q.BeforeDeploy = &BeforeDeployPolicy{Mode: "reuse"}; return q }(),
	} {
		if err := validateNativeDatabasePolicy("native-fixture", bad); err == nil {
			t.Fatal("primary database policy accepted", bad.BeforeDeploy, bad.SkipReleaseBackup)
		}
	}
	if err := validateNativeDatabasePolicy("native-scraper", scraper); err != nil {
		t.Fatal(err)
	}
	ok := p
	ok.BeforeDeploy = &BeforeDeployPolicy{Mode: "reuse", MaxAgeHours: 24}
	if err := validateNativeDatabasePolicy("native-fixture", ok); err != nil {
		t.Fatal("primary may reuse", err)
	}
}

// nativeGateFixture binds a primary and a queue database to one native
// application with off-site protection, ready for the release gate.
func nativeGateFixture(t *testing.T) (*Manager, NativeApplication, NativeDatabasePolicy, NativeDatabasePolicy) {
	t.Helper()
	raw := "postgresql://fixture:password@127.0.0.1:15999/native_fixture"
	m, a, p, _ := nativeDatabaseFixture(t, raw)
	p.Roles = []string{"primary"}
	scraper := p
	scraper.Roles, scraper.EnvironmentKey = []string{"queue"}, "QUEUE_DATABASE_URL"
	m.cfg.Native.Databases = map[string]NativeDatabasePolicy{"native-fixture": p, "native-scraper": scraper}
	m.data.NativeDatabases = map[string]NativeDatabaseBinding{"native-fixture": {Policy: p}, "native-scraper": {Policy: scraper}}
	m.data.Databases = map[string]Database{
		"native-fixture": {AppID: "native-fixture", Mode: "external", URL: raw, Ready: true, Backup: BackupPolicy{Keep: 1, Offsite: true}},
		"native-scraper": {AppID: "native-scraper", Mode: "external", URL: strings.Replace(raw, "native_fixture", "scraper", 1), Ready: true, Backup: BackupPolicy{Keep: 1, Offsite: true}},
	}
	m.data.BackupStorage = &storage.Config{}
	m.data.Deployments = []Deployment{{ID: "gate"}}
	m.nativeOperations = map[string]bool{a.ID: true}
	return m, a, p, scraper
}

func offsiteRecord(id string, age time.Duration) BackupRecord {
	return BackupRecord{ID: id, Created: time.Now().UTC().Add(-age), Size: 4096, SHA256: strings.Repeat("d", 64), Offsite: true, OffsiteRequested: true, Seconds: 2}
}

func TestNativeReleaseGateHonoursBeforeDeployPolicies(t *testing.T) {
	// Never: the secondary database is left out (even unready), the
	// primary is backed up fresh. The legacy flag in the configuration and
	// the panel's saved policy both mean never.
	for _, source := range []string{"state", "legacy flag"} {
		m, a, _, scraper := nativeGateFixture(t)
		d := m.data.Databases["native-scraper"]
		d.Ready = false
		if source == "state" {
			d.BeforeDeploy = BeforeDeployPolicy{Mode: "never"}
		} else {
			scraper.SkipReleaseBackup = true
			m.cfg.Native.Databases["native-scraper"] = scraper
		}
		m.data.Databases["native-scraper"] = d
		if !m.nativeDatabaseInUseLocked("native-scraper") {
			t.Fatal("the policy took the database out of use")
		}
		if targets := m.nativeReleaseBackupTargetsLocked(a.ID); len(targets) != 1 || targets[0] != "native-fixture" {
			t.Fatal("release backup targets", targets)
		}
		ctx := m.deploymentDiagnostics(context.Background(), job{app: App{ID: a.ID}, deployment: "gate"})
		_, err := m.backupBeforeNativeRailsRelease(ctx, a.ID, []string{"primary", "queue"})
		log := m.data.Deployments[0].Log
		// The primary's fresh backup fails here (no server), past the role check.
		if err == nil || strings.Contains(err.Error(), "unprotected") || strings.Contains(err.Error(), "ready resources") {
			t.Fatal(source, "excluded database still gated", err)
		}
		for _, want := range []string{"Skipping the pre-release backup of native-scraper (queue) by its database policy (scheduled backups only); its scheduled backups continue.", "Taking a fresh backup of native-fixture (policy: always)."} {
			if !strings.Contains(log, want) {
				t.Fatalf("%s: log lacks %q:\n%s", source, want, log)
			}
		}
		if d := m.data.Databases["native-scraper"]; len(d.Backups) != 0 || d.Operation != "" {
			t.Fatal("excluded database was backed up", d)
		}
	}

	// Reuse: recent verified off-site backups count for both databases; no
	// fresh backup runs and the proofs name the reused backups and ages.
	m, a, p, _ := nativeGateFixture(t)
	p.BeforeDeploy = &BeforeDeployPolicy{Mode: "reuse", MaxAgeHours: 24} // configuration as the initial default
	m.cfg.Native.Databases["native-fixture"] = p
	primary := m.data.Databases["native-fixture"]
	primary.Backups = []BackupRecord{offsiteRecord("p-old", 40*time.Hour), offsiteRecord("p-recent", 2*time.Hour)}
	m.data.Databases["native-fixture"] = primary
	secondary := m.data.Databases["native-scraper"]
	secondary.BeforeDeploy = BeforeDeployPolicy{Mode: "reuse", MaxAgeHours: 48} // saved from the panel
	secondary.Backups = []BackupRecord{offsiteRecord("s-recent", 30*time.Hour)}
	m.data.Databases["native-scraper"] = secondary
	ctx := m.deploymentDiagnostics(context.Background(), job{app: App{ID: a.ID}, deployment: "gate"})
	proofs, err := m.backupBeforeNativeRailsRelease(ctx, a.ID, []string{"primary", "queue"})
	if err != nil {
		t.Fatal(err)
	}
	byResource := map[string]NativeDatabaseBackupProof{}
	for _, proof := range proofs {
		byResource[proof.Resource] = proof
	}
	if len(proofs) != 2 || !byResource["native-fixture"].Reused || byResource["native-fixture"].Backup != "p-recent" || byResource["native-fixture"].Policy != "reuse within 24h" ||
		byResource["native-fixture"].AgeSeconds < 7200 || byResource["native-fixture"].AgeSeconds > 7260 || byResource["native-fixture"].SHA256 != strings.Repeat("d", 64) ||
		!byResource["native-scraper"].Reused || byResource["native-scraper"].Backup != "s-recent" || byResource["native-scraper"].Policy != "reuse within 48h" || byResource["native-scraper"].AgeSeconds < 108000 {
		t.Fatalf("proofs %+v", proofs)
	}
	log := m.data.Deployments[0].Log
	for _, want := range []string{"Reusing backup p-recent from 2h ago for native-fixture (policy: reuse within 24h).", "Reusing backup s-recent from 30h ago for native-scraper (policy: reuse within 48h)."} {
		if !strings.Contains(log, want) {
			t.Fatalf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "Creating the required pre-deployment backup") || strings.Contains(log, "Taking a fresh backup") {
		t.Fatal("a fresh backup ran under reuse:\n", log)
	}
	if a, b := m.data.Databases["native-fixture"], m.data.Databases["native-scraper"]; a.Operation != "" || len(a.Backups) != 2 || b.Operation != "" || len(b.Backups) != 1 {
		t.Fatal("databases changed under reuse", a, b)
	}

	// Reuse with nothing recent enough, or without the required off-site
	// copy, takes a fresh backup and says why.
	for _, c := range []struct {
		name   string
		record BackupRecord
		reason string
	}{
		{"too old", offsiteRecord("s-stale", 24*time.Hour+time.Minute), "the newest verified backup is 24h old"},
		{"no off-site copy", func() BackupRecord { r := offsiteRecord("s-local", time.Hour); r.Offsite = false; return r }(), "no verified backup with an off-site copy yet"},
	} {
		m, a, p, _ := nativeGateFixture(t)
		p.BeforeDeploy = &BeforeDeployPolicy{Mode: "reuse", MaxAgeHours: 24}
		m.cfg.Native.Databases["native-fixture"] = p
		primary := m.data.Databases["native-fixture"]
		primary.Backups = []BackupRecord{offsiteRecord("p-recent", time.Hour)}
		m.data.Databases["native-fixture"] = primary
		secondary := m.data.Databases["native-scraper"]
		secondary.BeforeDeploy = BeforeDeployPolicy{Mode: "reuse", MaxAgeHours: 24}
		secondary.Backups = []BackupRecord{c.record}
		m.data.Databases["native-scraper"] = secondary
		ctx := m.deploymentDiagnostics(context.Background(), job{app: App{ID: a.ID}, deployment: "gate"})
		if _, err := m.backupBeforeNativeRailsRelease(ctx, a.ID, []string{"primary", "queue"}); err == nil || strings.Contains(err.Error(), "unprotected") {
			t.Fatal(c.name, err)
		}
		if log := m.data.Deployments[0].Log; !strings.Contains(log, "Taking a fresh backup of native-scraper: "+c.reason+" (policy: reuse within 24h).") || !strings.Contains(log, "Reusing backup p-recent from 1h ago for native-fixture") {
			t.Fatalf("%s: log\n%s", c.name, log)
		}
	}
}

// The container deploy path honours the same policies: a secondary database
// set to never or with a recent verified backup is not dumped again; a
// primary database is dumped whatever its saved policy says.
func TestContainerPredeploymentBackupHonoursBeforeDeployPolicies(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\ncase \"$1\" in\nrun) printf 'fixture-dump';;\nesac\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	m, app := beforeDeployFixture(t, time.Hour)
	m.mu.Lock()
	events := m.data.Databases["shop-events"]
	events.BeforeDeploy = BeforeDeployPolicy{Mode: "reuse", MaxAgeHours: 24}
	m.data.Databases["shop-events"] = events
	primary := m.data.Databases["shop"]
	primary.BeforeDeploy = BeforeDeployPolicy{Mode: "never"} // ignored for a primary
	m.data.Databases["shop"] = primary
	m.data.Deployments = []Deployment{{ID: "deploy"}}
	m.mu.Unlock()
	ctx := m.deploymentDiagnostics(context.Background(), job{app: app, deployment: "deploy"})
	if err := m.backupBeforeRelease(ctx, []string{"shop", "shop-events"}); err != nil {
		t.Fatal(err)
	}
	log := m.Deployments()[0].Log
	for _, want := range []string{"Taking a fresh backup of shop (policy: always).", "Pre-deployment backup completed for shop.", "Reusing backup b-shop-events from 1h ago for shop-events (policy: reuse within 24h)."} {
		if !strings.Contains(log, want) {
			t.Fatalf("log lacks %q:\n%s", want, log)
		}
	}
	// Local retention keeps the newest usable dump only: the primary's
	// record is now a fresh pre-deployment backup, the reused one unchanged.
	newest := func(id string) BackupRecord {
		m.mu.Lock()
		defer m.mu.Unlock()
		var latest BackupRecord
		for _, record := range m.data.Databases[id].Backups {
			if record.Created.After(latest.Created) {
				latest = record
			}
		}
		return latest
	}
	if latest := newest("shop"); latest.ID == "b-shop" || latest.Kind != predeploymentBackup {
		t.Fatal("primary not backed up fresh", latest)
	}
	if latest := newest("shop-events"); latest.ID != "b-shop-events" {
		t.Fatal("reused database was dumped again", latest)
	}
	m.mu.Lock()
	events = m.data.Databases["shop-events"]
	events.BeforeDeploy = BeforeDeployPolicy{Mode: "never"}
	m.data.Databases["shop-events"] = events
	m.data.Deployments = []Deployment{{ID: "never"}}
	m.mu.Unlock()
	ctx = m.deploymentDiagnostics(context.Background(), job{app: app, deployment: "never"})
	if err := m.backupBeforeRelease(ctx, []string{"shop-events"}); err != nil {
		t.Fatal(err)
	}
	if log := m.Deployments()[0].Log; !strings.Contains(log, "Skipping the pre-release backup of shop-events by its database policy (scheduled backups only)") {
		t.Fatal(log)
	}
	// The final backup before a database is removed is always fresh.
	m.mu.Lock()
	m.data.Deployments = []Deployment{{ID: "final"}}
	m.mu.Unlock()
	ctx = m.deploymentDiagnostics(context.Background(), job{app: app, deployment: "final"})
	if err := m.freshBackupBeforeRelease(ctx, []string{"shop-events"}); err != nil {
		t.Fatal(err)
	}
	if latest := newest("shop-events"); latest.ID == "b-shop-events" || latest.Kind != predeploymentBackup {
		t.Fatal("final backup not taken", latest)
	}
}

func TestBeforeDeployAPIValidatesAndExposesThePolicy(t *testing.T) {
	m, _ := beforeDeployFixture(t, time.Hour)
	mux := http.NewServeMux()
	m.databaseRoutes(mux)
	views := func() map[string]DatabaseView {
		rec := serve(t, mux, "GET", "/v1/databases", nil)
		var list []DatabaseView
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
			t.Fatal(rec.Code, rec.Body.String())
		}
		out := map[string]DatabaseView{}
		for _, v := range list {
			out[v.AppID] = v
		}
		return out
	}
	before := views()
	if v := before["shop"]; !v.Primary || v.BeforeDeploy.Mode != "always" {
		t.Fatalf("primary view %+v", v)
	}
	if v := before["shop-events"]; v.Primary || v.BeforeDeploy.Mode != "always" {
		t.Fatalf("secondary view %+v", v)
	}
	for _, c := range []struct {
		id   string
		body any
		code int
	}{
		{"shop-events", map[string]any{"mode": "reuse", "max_age_hours": 48}, 200},
		{"shop-events", map[string]any{"mode": "never"}, 200},
		{"shop-events", map[string]any{"mode": "sometimes"}, 400},
		{"shop-events", map[string]any{"mode": "reuse"}, 400},
		{"shop-events", map[string]any{"mode": "reuse", "max_age_hours": 1000}, 400},
		{"shop-events", map[string]any{"mode": "always", "max_age_hours": 2}, 400},
		{"shop", map[string]any{"mode": "never"}, 409},
		{"shop", map[string]any{"mode": "reuse", "max_age_hours": 6}, 200},
		{"missing", map[string]any{"mode": "always"}, 409},
	} {
		if rec := serve(t, mux, "PATCH", "/v1/databases/"+c.id+"/backups/before-deploy", c.body); rec.Code != c.code {
			t.Fatalf("%s %v: %d %s", c.id, c.body, rec.Code, rec.Body.String())
		}
	}
	after := views()
	if v := after["shop-events"]; v.BeforeDeploy.Mode != "never" {
		t.Fatalf("saved policy %+v", v.BeforeDeploy)
	}
	if v := after["shop"]; v.BeforeDeploy != (BeforeDeployPolicy{Mode: "reuse", MaxAgeHours: 6}) {
		t.Fatalf("primary policy %+v", v.BeforeDeploy)
	}
	// The saved policy is state, read back by a manager that reopens it.
	reopened, err := NewManager(context.Background(), m.cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.mu.Lock()
	persisted := reopened.data.Databases["shop-events"].BeforeDeploy
	reopened.mu.Unlock()
	if persisted.Mode != "never" {
		t.Fatal("policy not persisted", persisted)
	}
}
