package onboarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/agim/lidza-deploy/internal/control"
	"github.com/agim/lidza/pkg/credentials"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func dockerAvailable() bool { _, err := exec.LookPath("docker"); return err == nil }
func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	for _, name := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT"} {
		if v := os.Getenv(name); v != "" {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
	}
	cmd.WaitDelay = 3 * time.Second
	data, err := cmd.Output()
	if err != nil {
		return "", errors.New("the Docker operation failed; check the service and control-panel user's Docker access")
	}
	return strings.TrimSpace(string(data)), nil
}
func (s *Setup) managedDatabase(ctx context.Context) (string, error) {
	sum := sha256.Sum256([]byte(s.opts.Dir))
	id := hex.EncodeToString(sum[:6])
	name := "lidza-control-db-" + id
	saved, err := credentials.Read(s.opts.Dir)
	if err != nil {
		return "", errors.New("could not read managed database state")
	}
	password := saved["SETUP_DB_PASSWORD"]
	missingPassword := password == ""
	found, err := docker(ctx, "ps", "--all", "--filter", "name=^/"+name+"$", "--format", "{{.ID}}")
	if err != nil {
		return "", err
	}
	if found != "" {
		owner, e := docker(ctx, "inspect", "--format", `{{index .Config.Labels "io.lidza.control.database"}}`, name)
		if e != nil {
			return "", e
		}
		if owner != id {
			return "", fmt.Errorf("database container %s belongs to an unmanaged installation; preserve it with docker rename %s %s-old, then retry setup (its volume is left untouched)", name, name, name)
		}
		if _, err = docker(ctx, "start", name); err != nil {
			return "", err
		}
	} else {
		if password == "" {
			password = random()
			if err = credentials.Set(s.opts.Dir, map[string]string{"SETUP_DB_PASSWORD": password}); err != nil {
				return "", errors.New("could not save database credentials")
			}
			missingPassword = false
		}
		f, e := os.CreateTemp(s.opts.Dir, ".postgres-env-*")
		if e != nil {
			return "", e
		}
		defer os.Remove(f.Name())
		_, e = fmt.Fprintf(f, "POSTGRES_USER=lidza\nPOSTGRES_DB=lidza_control\nPOSTGRES_PASSWORD=%s\n", password)
		f.Close()
		if e != nil {
			return "", e
		}
		_, err = docker(ctx, "run", "--detach", "--name", name, "--label", "io.lidza.control.database="+id, "--restart", "unless-stopped", "--memory", "1g", "--log-opt", "max-size=10m", "--log-opt", "max-file=3", "--publish", "127.0.0.1::5432", "--mount", "type=volume,src="+name+",dst=/var/lib/postgresql/data", "--env-file", f.Name(), "postgres:17-alpine")
		if err != nil {
			return "", err
		}
	}
	binding, err := docker(ctx, "port", name, "5432/tcp")
	if err != nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(binding)
	if err != nil || host != "127.0.0.1" {
		return "", errors.New("unexpected managed database port binding")
	}
	// Wait for PostgreSQL, not just the container process, to be ready.
	for i := 0; i < 40; i++ {
		if _, err = docker(ctx, "exec", name, "pg_isready", "-h", "127.0.0.1", "-U", "lidza", "-d", "lidza_control"); err == nil {
			if missingPassword && password == "" {
				password = random()
			}
			u := url.URL{Scheme: "postgres", User: url.UserPassword("lidza", password), Host: net.JoinHostPort(host, port), Path: "/lidza_control", RawQuery: "sslmode=disable"}
			conn, connectErr := pgx.Connect(ctx, u.String())
			if connectErr == nil {
				conn.Close(ctx)
				if !missingPassword {
					return u.String(), nil
				}
			}
			if !missingPassword && !databaseAuthenticationError(connectErr) {
				return "", databaseConnectionError(connectErr)
			}
			// Only our labelled container is eligible. Use the local socket;
			// SQL and the password go through stdin, never argv or logs.
			password = random()
			cmd := exec.CommandContext(ctx, "docker", "exec", "-i", name, "psql", "-U", "lidza", "-d", "lidza_control", "-v", "ON_ERROR_STOP=1")
			cmd.Stdin = strings.NewReader("ALTER ROLE lidza PASSWORD '" + strings.ReplaceAll(password, "'", "''") + "';\n")
			cmd.WaitDelay = 3 * time.Second
			if err := cmd.Run(); err != nil {
				return "", errors.New("could not recover the managed database password through its local socket; check Docker access and PostgreSQL local authentication")
			}
			if err := credentials.Set(s.opts.Dir, map[string]string{"SETUP_DB_PASSWORD": password}); err != nil {
				return "", errors.New("could not save recovered database credentials")
			}
			u.User = url.UserPassword("lidza", password)
			conn, connectErr = pgx.Connect(ctx, u.String())
			if connectErr != nil {
				return "", databaseConnectionError(connectErr)
			}
			conn.Close(ctx)
			return u.String(), nil
		}
		select {
		case <-ctx.Done():
			return "", errors.New("managed database did not become ready; retry setup")
		case <-time.After(time.Second):
		}
	}
	return "", errors.New("managed database did not become ready; retry setup")
}
func validateAgent(ctx context.Context, s control.Server) error {
	cfg := control.Config{PublicURL: "http://127.0.0.1", User: "setup@example.com", Key: make([]byte, 32), DataDir: filepath.Join(os.TempDir(), "lidza-validate-"+random()), Servers: []control.Server{s}}
	// New validates URLs/tokens without writing configuration.
	if _, err := control.New(cfg); err != nil {
		return errors.New("invalid agent ID, URL or token")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", s.URL+"/v1/apps", nil)
	if err != nil {
		return errors.New("invalid agent URL")
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	client := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return errors.New("agent could not be reached")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return errors.New("agent authentication failed")
	}
	return nil
}

func databaseAuthenticationError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "28P01" || pgErr.Code == "28000")
}
func databaseConnectionError(err error) error {
	if databaseAuthenticationError(err) {
		return errors.New("PostgreSQL authentication failed; check the database user and password")
	}
	return errors.New("could not connect to PostgreSQL; check its address, credentials and TLS configuration")
}
