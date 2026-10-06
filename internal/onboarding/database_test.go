package onboarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza/pkg/credentials"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestDatabaseAuthenticationClassification(t *testing.T) {
	if !databaseAuthenticationError(&pgconn.PgError{Code: "28P01"}) {
		t.Fatal("password failure not classified")
	}
	if databaseAuthenticationError(&pgconn.PgError{Code: "08006"}) {
		t.Fatal("connection failure misclassified")
	}
}

func TestManagedDatabaseReinstallRecovery(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("requires Docker")
	}
	cleanSetupEnv(t)
	dir := t.TempDir()
	if _, err := credentials.Generate(dir); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(dir))
	name := "lidza-control-db-" + hex.EncodeToString(sum[:6])
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	t.Cleanup(func() {
		docker(context.Background(), "rm", "-f", name)
		docker(context.Background(), "volume", "rm", name)
	})
	s := &Setup{opts: Options{Dir: dir}}
	connect := func() *pgx.Conn {
		t.Helper()
		u, err := s.managedDatabase(ctx)
		if err != nil {
			t.Fatal(err)
		}
		c, err := pgx.Connect(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := connect()
	if _, err := c.Exec(ctx, "CREATE TABLE recovery_marker (value text); INSERT INTO recovery_marker VALUES ('preserved')"); err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)
	verify := func() {
		t.Helper()
		c := connect()
		defer c.Close(ctx)
		var value string
		if err := c.QueryRow(ctx, "SELECT value FROM recovery_marker").Scan(&value); err != nil || value != "preserved" {
			t.Fatal("data lost", err)
		}
		saved, err := credentials.Read(dir)
		if err != nil || saved["SETUP_DB_PASSWORD"] == "" {
			t.Fatal("recovered password not sealed", err)
		}
	}
	// Existing stopped container, lost setup password.
	if _, err := docker(ctx, "stop", name); err != nil {
		t.Fatal(err)
	}
	if err := credentials.Set(dir, map[string]string{"SETUP_DB_PASSWORD": ""}); err != nil {
		t.Fatal(err)
	}
	verify()
	// Container removed, old volume retained, newly sealed password rejected.
	if _, err := docker(ctx, "rm", "-f", name); err != nil {
		t.Fatal(err)
	}
	if err := credentials.Set(dir, map[string]string{"SETUP_DB_PASSWORD": "deliberately-wrong"}); err != nil {
		t.Fatal(err)
	}
	verify()
	// Matching name does not grant ownership.
	if _, err := docker(ctx, "rm", "-f", name); err != nil {
		t.Fatal(err)
	}
	if _, err := docker(ctx, "create", "--name", name, "postgres:17-alpine"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.managedDatabase(ctx); err == nil || !strings.Contains(err.Error(), "docker rename "+name) {
		t.Fatal("unmanaged container not refused", err)
	}
}

func TestManagedDatabaseDockerAccessFailure(t *testing.T) {
	cleanSetupEnv(t)
	dir := t.TempDir()
	if _, err := credentials.Generate(dir); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(bin+"/docker", []byte("#!/bin/sh\nif [ \"$1\" = ps ]; then echo fixture; exit 0; fi\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	s := &Setup{opts: Options{Dir: dir}}
	_, err := s.managedDatabase(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Docker access") || strings.Contains(err.Error(), "unmanaged") {
		t.Fatal("inspect access failure misreported", err)
	}
}
