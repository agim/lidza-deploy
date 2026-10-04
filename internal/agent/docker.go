package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Docker struct {
	Root     string
	Client   *http.Client
	checkout func(context.Context, App, string, string) error
}
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 65536 {
		remaining := 65536 - b.Len()
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

// Never return raw command output on errors: build tools may echo secrets.
func command(ctx context.Context, dir string, extra []string, name string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v := os.Getenv(key); v != "" {
			c.Env = append(c.Env, key+"="+v)
		}
	}
	c.Env = append(c.Env, extra...)
	c.WaitDelay = 5 * time.Second
	var b limitedBuffer
	c.Stdout = &b
	c.Stderr = &b
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("%s failed: %w", name, err)
	}
	return strings.TrimSpace(b.String()), nil
}
func (d *Docker) Deploy(ctx context.Context, a App, id, token string) (release *Release, err error) {
	if err = a.Validate(); err != nil {
		return nil, err
	}
	if err = os.MkdirAll(d.Root, 0700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(d.Root, "build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	source := filepath.Join(dir, "source")
	checkout := d.checkout
	if checkout == nil {
		checkout = clone
	}
	if err = checkout(ctx, a, source, token); err != nil {
		return nil, err
	}
	commit, err := command(ctx, source, nil, "git", "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if err = prepareBuild(source); err != nil {
		return nil, err
	}
	if err = os.RemoveAll(filepath.Join(source, ".git")); err != nil {
		return nil, err
	}
	image := "lidza/" + a.ID + ":" + id
	if _, err = command(ctx, source, nil, "docker", "build", "--tag", image, "."); err != nil {
		return nil, fmt.Errorf("image build: %w", err)
	}
	name := "lidza-" + a.ID + "-" + id
	release = &Release{ID: id, Commit: commit, Container: name, Image: image, Created: time.Now().UTC()}
	candidate := release
	success := false
	defer func() {
		if !success {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = d.Remove(cleanup, candidate)
		}
	}()
	var content strings.Builder
	for k, v := range a.Env {
		fmt.Fprintf(&content, "%s=%s\n", k, v)
	}
	envfile := filepath.Join(dir, "runtime.env")
	if err = os.WriteFile(envfile, []byte(content.String()), 0600); err != nil {
		return nil, err
	}
	runArgs := []string{"run", "--detach", "--name", name, "--label", "io.lidza.managed=true", "--restart", "unless-stopped", "--read-only", "--user", "65532:65532", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "256", "--memory", "512m", "--cpus", "1", "--log-opt", "max-size=10m", "--log-opt", "max-file=3", "--tmpfs", "/tmp:rw,noexec,nosuid,size=64m", "--publish", "127.0.0.1::3000", "--env-file", envfile, "--env", "LIDZA_ADDR=0.0.0.0:3000", "--env", "LIDZA_MODE=production"}
	if a.Network != "" {
		runArgs = append(runArgs, "--network", a.Network)
	}
	runArgs = append(runArgs, image)
	_, err = command(ctx, dir, nil, "docker", runArgs...)
	if err != nil {
		return nil, fmt.Errorf("start container: %w", err)
	}
	port, err := command(ctx, dir, nil, "docker", "port", name, "3000/tcp")
	if err != nil {
		return nil, err
	}
	if !regexp.MustCompile(`^127\.0\.0\.1:[0-9]+$`).MatchString(port) {
		return nil, errors.New("unexpected container port mapping")
	}
	release.Port = strings.TrimPrefix(port, "127.0.0.1:")
	health, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err = d.Ready(health, release); err == nil {
			success = true
			return release, nil
		}
		select {
		case <-health.Done():
			return nil, errors.New("candidate failed /readyz; previous release retained")
		case <-ticker.C:
		}
	}
}
func (d *Docker) Ready(ctx context.Context, r *Release) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:"+r.Port+"/readyz", nil)
	if err != nil {
		return err
	}
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode != 200 {
		return fmt.Errorf("readiness returned %d", res.StatusCode)
	}
	return nil
}
func (d *Docker) Remove(ctx context.Context, r *Release) error {
	if r == nil {
		return nil
	}
	// Listing first makes retries safe when a previous cleanup removed only part
	// of the release. Daemon failures remain errors rather than "already removed".
	containers, err := command(ctx, "", nil, "docker", "container", "ls", "--all", "--filter", "name=^/"+r.Container+"$", "--format", "{{.ID}}")
	if err != nil {
		return err
	}
	if containers != "" {
		if _, err = command(ctx, "", nil, "docker", "rm", "--force", r.Container); err != nil {
			return err
		}
	}
	if r.Image != "" {
		images, e := command(ctx, "", nil, "docker", "image", "ls", "--quiet", "--filter", "reference="+r.Image)
		if e != nil {
			return e
		}
		if images != "" {
			_, err = command(ctx, "", nil, "docker", "image", "rm", r.Image)
		}
	}
	return err
}
func (d *Docker) Logs(ctx context.Context, r *Release) (string, error) {
	if r == nil {
		return "", errors.New("no running release")
	}
	return command(ctx, "", nil, "docker", "logs", "--tail", "200", r.Container)
}

// clone gives Git a one-shot askpass token; neither URL nor repository config contains it.
func clone(ctx context.Context, a App, source, token string) error {
	dir := filepath.Dir(source)
	ask := filepath.Join(dir, "askpass")
	if err := os.WriteFile(ask, []byte("#!/bin/sh\ncase \"$1\" in *Username*) printf '%s\\n' x-access-token ;; *) printf '%s\\n' \"$LIDZA_CLONE_TOKEN\" ;; esac\n"), 0700); err != nil {
		return err
	}
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=" + ask, "LIDZA_CLONE_TOKEN=" + token, "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=", "GIT_CONFIG_KEY_1=http.followRedirects", "GIT_CONFIG_VALUE_1=false"}
	_, err := command(ctx, dir, env, "git", "clone", "--depth=1", "--single-branch", "--branch", a.Branch, "--", "https://github.com/"+a.Repository+".git", source)
	if err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	return nil
}
