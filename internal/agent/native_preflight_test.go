package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func preflightCheck(t *testing.T, report NativePreflight, id string) HostReadinessCheck {
	t.Helper()
	for _, c := range report.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no %s check in %+v", id, report.Checks)
	return HostReadinessCheck{}
}

// The preflight rehearses the creation of a native application with its data
// and changes nothing: every command it runs only reads.
func TestNativePreflightRehearsesEveryStepWithoutChangingTheHost(t *testing.T) {
	m, f := newHostDataFixture(t)
	m.cfg.Native = &NativeHostConfig{Root: t.TempDir(), User: "deploy", AppsRoot: filepath.Join(f.root, "srv/apps")}
	savedEUID, savedDisk, savedForbidden, savedListen := hostNativeEUID, preflightDisk, nativeForbiddenAppsRoots, nativePortListenable
	t.Cleanup(func() {
		hostNativeEUID, preflightDisk, nativeForbiddenAppsRoots, nativePortListenable = savedEUID, savedDisk, savedForbidden, savedListen
	})
	nativeForbiddenAppsRoots = nil // the fixture's root is a temporary directory
	nativePortListenable = func(int) bool { return true }
	hostNativeEUID = func() int { return 0 }
	free := map[string]uint64{}
	preflightDisk = func(path string) (DiskHealth, error) {
		rel := "/" + strings.TrimPrefix(strings.TrimPrefix(path, f.root), "/")
		if rel == "/broken" {
			return DiskHealth{}, errors.New("no such device")
		}
		return DiskHealth{Path: rel, Available: free[rel]}, nil
	}
	for _, dir := range []string{"var/lib/postgresql/16/main", "var/lib/clickhouse", "usr/bin"} {
		os.MkdirAll(filepath.Join(f.root, dir), 0o755)
	}
	os.WriteFile(filepath.Join(f.root, "usr/bin/apt-get"), []byte("#!/bin/sh\n"), 0o755)
	request := NativeCreateRequest{ID: "shop", Runtime: "go", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", HealthPath: "/readyz",
		Processes: []NativeProcessSpec{{Name: "web", Command: "./shop", Web: true}}, Data: NativeDataRequest{Postgres: true, Redis: true, ClickHouse: true, ClickHouseVersion: "26.8"}}

	// The database's own disk is what is measured, not the root file system.
	free["/var/lib/postgresql/16/main"], free["/var/lib/clickhouse"], free["/"] = 10<<30, 500<<30, 900<<30
	report := m.nativePreflight(context.Background(), NativePreflightRequest{Request: request, PostgresBytes: 40 << 30, ClickHouseBytes: 5 << 30})
	if report.OK {
		t.Fatalf("a database that does not fit on its disk passed: %+v", report.Checks)
	}
	disk := preflightCheck(t, report, "postgres-disk")
	if disk.OK || !strings.Contains(disk.Detail, "/var/lib/postgresql/16/main has 10.0 GiB free; the data needs about 53.0 GiB there (40.0 GiB on the source") {
		t.Fatalf("postgres disk: %+v", disk)
	}
	if c := preflightCheck(t, report, "postgres"); !c.OK || !strings.Contains(c.Detail, `PostgreSQL 16 cluster "main" (port 5432) is online`) {
		t.Fatalf("postgres: %+v", c)
	}
	// Services that are not installed yet are installed on creation: not a problem.
	if c := preflightCheck(t, report, "redis"); !c.OK || !strings.Contains(c.Detail, "installs it") {
		t.Fatalf("redis: %+v", c)
	}
	if c := preflightCheck(t, report, "clickhouse"); !c.OK || !strings.Contains(c.Detail, "installs 26.8 series") {
		t.Fatalf("clickhouse: %+v", c)
	}
	if c := preflightCheck(t, report, "clickhouse-disk"); !c.OK {
		t.Fatalf("clickhouse disk: %+v", c)
	}
	free["/var/lib/postgresql/16/main"] = 200 << 30
	if report = m.nativePreflight(context.Background(), NativePreflightRequest{Request: request, PostgresBytes: 40 << 30, ClickHouseBytes: 5 << 30}); !report.OK {
		t.Fatalf("a ready server failed its preflight: %+v", report.Checks)
	}
	// Nothing was changed: no package, service or permission command ran.
	f.mu.Lock()
	for _, command := range f.commands {
		for _, changing := range []string{"apt-get", "systemctl start", "systemctl enable", "systemctl restart", "useradd", "setfacl", "chmod", "pg_createcluster"} {
			if strings.Contains(command, changing) {
				t.Fatalf("the preflight changed the host: %s", command)
			}
		}
	}
	f.mu.Unlock()
	// A request the agent would refuse is reported as the first and only step.
	bad := request
	bad.ID = "Not An ID"
	if report = m.nativePreflight(context.Background(), NativePreflightRequest{Request: bad}); report.OK || len(report.Checks) != 1 || !strings.Contains(report.Checks[0].Detail, "cannot be created here") {
		t.Fatalf("invalid request: %+v", report.Checks)
	}
	// An agent that is not root cannot create the application's account.
	hostNativeEUID = func() int { return 1000 }
	if report = m.nativePreflight(context.Background(), NativePreflightRequest{Request: request}); report.OK || preflightCheck(t, report, "account").OK {
		t.Fatalf("non-root agent passed: %+v", report.Checks)
	}
}
