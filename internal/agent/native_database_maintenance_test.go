package agent

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNativeMaintenanceOnlyRewritesTheReviewedLoopbackPool(t *testing.T) {
	policy := NativeDatabasePolicy{
		AppID: "rails-shop", Enabled: true, EnvironmentKey: "DATABASE_URL",
		Host: "127.0.0.1", Port: "6432", MaintenancePort: "5432", PostgresMajor: 17,
		Roles: []string{"primary", "queue"}, ConfigSHA256: strings.Repeat("a", 64),
		DumpSHA256: strings.Repeat("b", 64), Backup: BackupPolicy{Keep: 1, Offsite: true},
	}
	const source = "postgresql://fixture:local%40fixture@127.0.0.1:6432/fixture?sslmode=prefer"
	if err := validateNativeDatabasePolicy("native-shop-fixture", policy); err != nil {
		t.Fatal(err)
	}
	direct, err := nativePostgresMaintenanceURL(source, policy)
	if err != nil || direct != strings.Replace(source, ":6432/", ":5432/", 1) {
		t.Fatal("maintenance changed more than the approved local port", err)
	}
	if _, err = nativePostgresMaintenanceURL(strings.Replace(source, "127.0.0.1", "127.0.0.2", 1), policy); err == nil {
		t.Fatal("maintenance accepted an unreviewed source host")
	}
	for name, change := range map[string]func(*NativeDatabasePolicy){
		"same port":    func(p *NativeDatabasePolicy) { p.MaintenancePort = p.Port },
		"remote pool":  func(p *NativeDatabasePolicy) { p.Host = "10.0.0.5" },
		"invalid port": func(p *NativeDatabasePolicy) { p.MaintenancePort = "05432" },
	} {
		bad := policy
		change(&bad)
		if err = validateNativeDatabasePolicy("native-shop-fixture", bad); err == nil {
			t.Fatal("maintenance port accepted:", name)
		}
	}
	secondary := policy
	secondary.EnvironmentKey, secondary.Roles = "ANALYTICS_DATABASE_URL", []string{"secondary"}
	if err = validateNativeDatabasePolicy("native-shop-analytics", secondary); err != nil {
		t.Fatal("a separately configured secondary database was refused", err)
	}
	secondary.EnvironmentKey = "DATABASE_URL"
	if err = validateNativeDatabasePolicy("native-shop-analytics", secondary); err == nil {
		t.Fatal("a secondary database shared the primary connection key")
	}
	bad := policy
	bad.MaintenancePort = ""
	if direct, err = nativePostgresMaintenanceURL(source, bad); err != nil || direct != source {
		t.Fatal("ordinary database bindings must retain the exact URL")
	}
	if sameNativeDatabasePolicy(policy, bad) {
		t.Fatal("the maintenance binding changed without triggering policy review")
	}
}

func TestNativeMaintenanceReadsBackendIdentityWithoutWrites(t *testing.T) {
	raw := os.Getenv("TEST_NATIVE_MAINTENANCE_URL")
	if raw == "" {
		t.Skip("set TEST_NATIVE_MAINTENANCE_URL for the disposable PostgreSQL fixture")
	}
	uri, err := url.Parse(raw)
	if err != nil || uri.Host != "127.0.0.1:15473" || uri.Path != "/lidza_maintenance_fixture" {
		t.Fatal("the exact loopback fixture is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	identity, err := readNativePostgresBackend(ctx, raw)
	if err != nil || identity.port != 5432 || identity.version/10000 != 17 || identity.database == 0 || identity.role == 0 {
		t.Fatal("failed to read the local PostgreSQL backend identity", err)
	}
}
