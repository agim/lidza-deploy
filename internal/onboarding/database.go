package onboarding

import (
	"context"
	"errors"
	"fmt"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/control"
	"github.com/agim/lidza/pkg/credentials"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const managedDatabaseImage = "postgres:17-alpine"

// managedDatabasePorts is the loopback port range for the control panel's own
// PostgreSQL container. The port is fixed per installation and saved as
// SETUP_DB_PORT, so DATABASE_URL stays valid when the container restarts
// (after a Docker upgrade, a reboot or a crash). 5432 is left to a host
// PostgreSQL. Tests narrow the range.
var managedDatabasePorts = [2]int{5433, 5499}

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

// managedDatabaseName derives the installation id and the container (and
// volume) name from the data directory; a control panel move restores into
// the container of the same name on its destination.
func managedDatabaseName(dir string) (id, name string) {
	return agent.ControlDatabaseContainer(dir)
}

// RecoverManagedDatabase starts and verifies the control panel's own database
// before the application framework tries to connect to it. External databases
// are never touched: SETUP_DB_PASSWORD exists only for installer-managed
// PostgreSQL. managedDatabase also verifies the installation label before it
// starts an existing container and retains its named volume when recreating it.
func RecoverManagedDatabase(ctx context.Context, dir string) error {
	saved, err := credentials.Read(dir)
	if err != nil {
		return errors.New("could not read managed database state")
	}
	if _, managed := saved["SETUP_DB_PASSWORD"]; !managed {
		return nil
	}
	s := &Setup{opts: Options{Dir: dir}}
	dsn, err := s.managedDatabase(ctx)
	if err != nil {
		return fmt.Errorf("managed database recovery failed: %w", err)
	}
	if saved["DATABASE_URL"] != dsn {
		if err = credentials.Set(dir, map[string]string{"DATABASE_URL": dsn}); err != nil {
			return errors.New("could not save the recovered managed database address")
		}
		log.Printf("managed database address saved: %s", redactedAddress(dsn))
	}
	return nil
}

func (s *Setup) managedDatabase(ctx context.Context) (string, error) {
	id, name := managedDatabaseName(s.opts.Dir)
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
		if managedDatabaseEphemeralPort(ctx, name) {
			// Containers from earlier versions published PostgreSQL on a
			// random loopback port that changed whenever the container
			// restarted, leaving the panel pointing at the old one. Recreate
			// it once on a fixed port; the named volume keeps the data and
			// the panel is not serving yet.
			log.Printf("managed database %s publishes a random port; recreating it on a fixed loopback port (volume %s is kept)", name, name)
			if _, err = docker(ctx, "stop", "--time", "60", name); err != nil {
				return "", err
			}
			if _, err = docker(ctx, "rm", name); err != nil {
				return "", err
			}
			found = ""
		} else if _, err = docker(ctx, "start", name); err != nil {
			return "", err
		}
	}
	if found == "" {
		if password == "" {
			password = random()
			if err = credentials.Set(s.opts.Dir, map[string]string{"SETUP_DB_PASSWORD": password}); err != nil {
				return "", errors.New("could not save database credentials")
			}
			missingPassword = false
		}
		port, err := s.managedDatabasePort(saved["SETUP_DB_PORT"])
		if err != nil {
			return "", err
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
		_, err = docker(ctx, "run", "--detach", "--name", name, "--label", "io.lidza.control.database="+id, "--restart", "unless-stopped", "--memory", "1g", "--log-opt", "max-size=10m", "--log-opt", "max-file=3", "--publish", "127.0.0.1:"+port+":5432", "--mount", "type=volume,src="+name+",dst=/var/lib/postgresql/data", "--env-file", f.Name(), managedDatabaseImage)
		if err != nil {
			return "", err
		}
	}
	host, port, err := managedDatabaseBinding(ctx, name)
	if err != nil {
		return "", err
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

// managedDatabaseEphemeralPort reports whether the container was created
// with `--publish 127.0.0.1::5432`, so Docker picks a new port every time it
// starts. Anything that cannot be inspected is left as it is.
func managedDatabaseEphemeralPort(ctx context.Context, name string) bool {
	binding, err := docker(ctx, "inspect", "--format", `{{with index .HostConfig.PortBindings "5432/tcp"}}{{(index . 0).HostPort}}{{else}}none{{end}}`, name)
	return err == nil && binding == ""
}

// managedDatabasePort returns the saved loopback port when it is still free,
// else the first free port of the range, and saves its choice.
func (s *Setup) managedDatabasePort(saved string) (string, error) {
	var candidates []int
	if n, err := strconv.Atoi(saved); err == nil && n > 0 && n < 65536 {
		candidates = append(candidates, n)
	}
	for p := managedDatabasePorts[0]; p <= managedDatabasePorts[1]; p++ {
		candidates = append(candidates, p)
	}
	for _, p := range candidates {
		if !loopbackPortFree(p) {
			continue
		}
		port := strconv.Itoa(p)
		if port != saved {
			if err := credentials.Set(s.opts.Dir, map[string]string{"SETUP_DB_PORT": port}); err != nil {
				return "", errors.New("could not save the managed database port")
			}
		}
		return port, nil
	}
	return "", fmt.Errorf("no free loopback port between %d and %d for the managed database", managedDatabasePorts[0], managedDatabasePorts[1])
}

func loopbackPortFree(port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// managedDatabaseBinding asks Docker where the container publishes
// PostgreSQL right now.
func managedDatabaseBinding(ctx context.Context, name string) (host, port string, err error) {
	binding, err := docker(ctx, "port", name, "5432/tcp")
	if err != nil {
		return "", "", err
	}
	first, _, _ := strings.Cut(binding, "\n")
	host, port, err = net.SplitHostPort(strings.TrimSpace(first))
	if err != nil || host != "127.0.0.1" {
		return "", "", errors.New("unexpected managed database port binding")
	}
	return host, port, nil
}

// managedDatabaseAddress resolves the labelled container's current address
// without starting or changing anything.
func managedDatabaseAddress(ctx context.Context, dir string) (string, error) {
	id, name := managedDatabaseName(dir)
	found, err := docker(ctx, "ps", "--all", "--filter", "name=^/"+name+"$", "--format", "{{.ID}}")
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", errors.New("the managed database container is missing")
	}
	owner, err := docker(ctx, "inspect", "--format", `{{index .Config.Labels "io.lidza.control.database"}}`, name)
	if err != nil {
		return "", err
	}
	if owner != id {
		return "", errors.New("the database container belongs to another installation")
	}
	host, port, err := managedDatabaseBinding(ctx, name)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, port), nil
}

// WatchManagedDatabase keeps a running control panel attached to its own
// database container. When the saved address stops answering but the
// labelled container now publishes PostgreSQL somewhere else (containers
// created before fixed ports got a new random port on every restart), the
// saved DATABASE_URL is corrected and restart is called so the panel
// reconnects: restart should end the process cleanly, the service manager
// starts it again, and boot recovery verifies the database first. External
// databases are never touched.
func WatchManagedDatabase(ctx context.Context, dir string, restart func()) {
	w := databaseWatch{dir: dir, interval: 15 * time.Second, threshold: 2, probe: probeDatabase, restart: restart, logf: log.Printf}
	go w.run(ctx)
}

type databaseWatch struct {
	dir       string
	interval  time.Duration
	threshold int // consecutive failed probes before the container is consulted
	probe     func(context.Context, string) error
	restart   func()
	logf      func(string, ...any)
}

func probeDatabase(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	return conn.Ping(ctx)
}

func (w databaseWatch) run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	failures := 0
	logged := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		saved, err := credentials.Read(w.dir)
		if err != nil {
			continue
		}
		dsn := saved["DATABASE_URL"]
		if saved["SETUP_DB_PASSWORD"] == "" || dsn == "" {
			failures = 0
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = w.probe(probeCtx, dsn)
		cancel()
		if err == nil {
			failures, logged = 0, false
			continue
		}
		failures++
		if failures < w.threshold {
			continue
		}
		if w.reconcile(ctx, dsn, &logged) {
			return
		}
	}
}

// reconcile compares the unreachable address with the container's current
// one and returns true when the panel is being restarted onto the new one.
func (w databaseWatch) reconcile(ctx context.Context, dsn string, logged *bool) bool {
	u, err := url.Parse(dsn)
	if err != nil {
		return false
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	current, err := managedDatabaseAddress(bounded, w.dir)
	cancel()
	switch {
	case err != nil:
		if !*logged {
			w.logf("managed database at %s is unreachable and its container cannot be inspected: %v", u.Host, err)
			*logged = true
		}
		return false
	case current == u.Host:
		if !*logged {
			w.logf("managed database at %s is unreachable; the container still publishes that address, waiting for it", u.Host)
			*logged = true
		}
		return false
	}
	previous := u.Host
	u.Host = current
	if err := credentials.Set(w.dir, map[string]string{"DATABASE_URL": u.String()}); err != nil {
		w.logf("managed database moved from %s to %s but the new address could not be saved: %v", previous, current, err)
		return false
	}
	w.logf("managed database moved from %s to %s; saved the new address and restarting the control panel to reconnect", previous, current)
	w.restart()
	return true
}

// redactedAddress is a DSN's host for logs, never its credentials.
func redactedAddress(dsn string) string {
	if u, err := url.Parse(dsn); err == nil {
		return u.Host
	}
	return "(unparseable address)"
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
