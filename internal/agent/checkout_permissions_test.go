package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckoutPermissionsUnderPrivateServiceUmask(t *testing.T) {
	if os.Getenv("LIDZA_TEST_PRIVATE_UMASK") == "1" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		dir := t.TempDir()
		repo := filepath.Join(dir, "repository")
		if err := os.MkdirAll(filepath.Join(repo, "mail"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "mail", "auth_reset.txt.tmpl"), []byte("reset-template"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "tool"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"init", repo}, {"-C", repo, "add", "."}, {"-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-m", "fixture"}} {
			if _, err := command(ctx, "", nil, "git", args...); err != nil {
				t.Fatal(err)
			}
		}
		source := filepath.Join(dir, "source")
		if _, err := command(ctx, "", nil, "git", "clone", "--", repo, source); err != nil {
			t.Fatal(err)
		}
		for path, mode := range map[string]os.FileMode{"mail": 0755, "mail/auth_reset.txt.tmpl": 0644, "tool": 0755} {
			info, err := os.Stat(filepath.Join(source, path))
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("checkout mode %s: %v %v", path, info, err)
			}
		}
		private := filepath.Join(dir, "runtime.env")
		if err := os.WriteFile(private, []byte("fixture-secret"), 0666); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(private)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("parent private umask changed")
		}
		if os.Getenv("TEST_DOCKER") == "1" {
			if err := os.WriteFile(filepath.Join(source, "Dockerfile"), []byte("FROM caddy:2.10.2-alpine\nCOPY mail /app/mail\n"), 0644); err != nil {
				t.Fatal(err)
			}
			image := "lidza-checkout-permission-test:" + newID()
			if _, err := command(ctx, source, nil, "docker", "build", "-t", image, "."); err != nil {
				t.Fatal(err)
			}
			defer command(context.Background(), "", nil, "docker", "image", "rm", image)
			output, err := command(ctx, "", nil, "docker", "run", "--rm", "--read-only", "--user", "65532:65532", image, "cat", "/app/mail/auth_reset.txt.tmpl")
			if err != nil || output != "reset-template" {
				t.Fatalf("nonroot runtime cannot read copied template: %s %v", output, err)
			}
		}
		return
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, "sh", "-c", `umask 077; exec "$1" -test.run '^TestCheckoutPermissionsUnderPrivateServiceUmask$' -test.v`, "fixture", binary)
	child.Env = append(os.Environ(), "LIDZA_TEST_PRIVATE_UMASK=1")
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("private-umask subprocess: %v\n%s", err, output)
	}
}
