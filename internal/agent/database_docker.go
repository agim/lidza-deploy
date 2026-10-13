package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

const databaseImage = "postgres:17-alpine"

func dockerStream(ctx context.Context, input io.Reader, output io.Writer, extra []string, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = input
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 5 * time.Second
	for _, name := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT"} {
		if v := os.Getenv(name); v != "" {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
	}
	cmd.Env = append(cmd.Env, extra...)
	if err := cmd.Run(); err != nil {
		return errors.New("PostgreSQL Docker operation failed; check connection, permissions and server version (client: PostgreSQL 17)")
	}
	return nil
}
func (m *Manager) provisionDatabase(ctx context.Context, d Database) error {
	if d.Mode == "local" {
		for _, kind := range []string{"network", "volume"} {
			name := d.Network
			if kind == "volume" {
				name += "-data"
			}
			found, err := command(ctx, "", nil, "docker", kind, "ls", "--filter", "name=^"+name+"$", "--format", "{{.Name}}")
			if err != nil {
				return err
			}
			if found == "" {
				if _, err = command(ctx, "", nil, "docker", kind, "create", "--label", "io.lidza.database="+d.AppID, name); err != nil {
					return err
				}
			} else {
				label, err := command(ctx, "", nil, "docker", kind, "inspect", "--format", `{{index .Labels "io.lidza.database"}}`, name)
				if err != nil || label != d.AppID {
					return errors.New("database resource name is already occupied")
				}
			}
		}
		found, err := command(ctx, "", nil, "docker", "ps", "-a", "--filter", "name=^/"+d.Network+"$", "--format", "{{.ID}}")
		if err != nil {
			return err
		}
		if found == "" {
			err = dockerStream(ctx, nil, io.Discard, []string{"POSTGRES_PASSWORD=" + d.AdminPassword}, "run", "-d", "--name", d.Network, "--label", "io.lidza.database="+d.AppID, "--network", d.Network, "--restart", "unless-stopped", "--memory", "1g", "--pids-limit", "256", "--log-opt", "max-size=10m", "--log-opt", "max-file=3", "--mount", "type=volume,src="+d.Network+"-data,dst=/var/lib/postgresql/data", "--env", "POSTGRES_PASSWORD", databaseImage)
			if err != nil {
				return errors.New("could not start PostgreSQL container")
			}
		} else {
			label, err := command(ctx, "", nil, "docker", "inspect", "--format", `{{index .Config.Labels "io.lidza.database"}}`, d.Network)
			if err != nil || label != d.AppID {
				return errors.New("database container name is already occupied")
			}
			if _, err = command(ctx, "", nil, "docker", "start", d.Network); err != nil {
				return err
			}
		}
		ready := false
		for i := 0; i < 60; i++ {
			if _, err = command(ctx, "", nil, "docker", "exec", d.Network, "pg_isready", "-h", "127.0.0.1", "-U", "postgres"); err == nil {
				ready = true
				break
			}
			select {
			case <-ctx.Done():
				return errors.New("database startup timed out")
			case <-time.After(time.Second):
			}
		}
		if !ready {
			return errors.New("database did not become ready")
		}
		u, _ := url.Parse(d.URL)
		password, _ := u.User.Password()
		// The password is generated hexadecimal; no caller-controlled SQL is interpolated.
		sql := fmt.Sprintf("DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='app') THEN CREATE ROLE app LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE; END IF; END $$;\nSELECT 'CREATE DATABASE app OWNER app' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='app')\\gexec\n", password)
		if err = dockerStream(ctx, strings.NewReader(sql), io.Discard, nil, "exec", "-i", d.Network, "psql", "-X", "-U", "postgres", "-d", "postgres", "-v", "ON_ERROR_STOP=1"); err != nil {
			return errors.New("could not create the application database and role")
		}
	}
	container := "lidza-db-check-" + newID()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = command(cleanup, "", nil, "docker", "rm", "-f", container)
	}()
	args := []string{"run", "--rm", "--name", container, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "256m"}
	if d.Network != "" {
		args = append(args, "--network", d.Network)
	}
	environment, flags := postgresEnvironment(d.URL)
	args = append(args, flags...)
	args = append(args, databaseImage, "psql", "-X", "-v", "ON_ERROR_STOP=1", "-tAc", "SELECT 1")
	return dockerStream(ctx, nil, io.Discard, environment, args...)
}

// libpq does not expand a connection URI supplied through the PGDATABASE
// environment variable. Pass its components as inherited environment values;
// passwords never enter the host command line or a temporary credentials file.
func postgresEnvironment(raw string) ([]string, []string) {
	u, _ := url.Parse(raw)
	password, _ := u.User.Password()
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	values := map[string]string{"PGHOST": u.Hostname(), "PGPORT": port, "PGUSER": u.User.Username(), "PGPASSWORD": password, "PGDATABASE": strings.TrimPrefix(u.Path, "/"), "PGCONNECT_TIMEOUT": "10"}
	for key, value := range u.Query() {
		if name := postgresOptions[key]; name != "" {
			values[name] = value[0]
		}
	}
	var env, flags []string
	for key, value := range values {
		env = append(env, key+"="+value)
		flags = append(flags, "--env", key)
	}
	return env, flags
}
