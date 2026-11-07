package agent

// Redis side of resource tuning: a minimal RESP client for the host's
// Redis servers, the key-pattern probe that classifies an application's
// use, and the migration of an application from a database of the shared
// host Redis to its own lidza-redis@<id> instance.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// redisConn sends one command and returns its reply: string (simple or
// bulk), int64, []any, nil (null) or an error for a Redis error reply.
type redisConn interface {
	do(ctx context.Context, args ...string) (any, error)
	close() error
}

// dialTuningRedis connects and authenticates; tests replace it.
var dialTuningRedis = func(ctx context.Context, network, address, password string) (redisConn, error) {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, errors.New("Redis is unavailable at " + address)
	}
	c := &respConn{conn: conn, reader: bufio.NewReaderSize(conn, 64<<10)}
	if password != "" {
		if _, err := c.do(ctx, "AUTH", password); err != nil {
			c.close()
			return nil, errors.New("Redis authentication failed")
		}
	}
	return c, nil
}

type respConn struct {
	conn   net.Conn
	reader *bufio.Reader
}

func (c *respConn) close() error { return c.conn.Close() }

func (c *respConn) do(ctx context.Context, args ...string) (any, error) {
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.conn.SetDeadline(deadline)
	if _, err := io.WriteString(c.conn, nativeRESP(args...)); err != nil {
		return nil, err
	}
	reply, err := readRESP(c.reader, 0)
	if err != nil {
		return nil, err
	}
	if e, ok := reply.(redisError); ok {
		return nil, e
	}
	return reply, nil
}

type redisError string

func (e redisError) Error() string { return "Redis: " + string(e) }

// readRESP reads one RESP2 reply with bounded sizes.
func readRESP(r *bufio.Reader, depth int) (any, error) {
	if depth > 4 {
		return nil, errors.New("Redis reply is nested too deeply")
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return nil, errors.New("empty Redis reply")
	}
	body := line[1:]
	switch line[0] {
	case '+':
		return body, nil
	case '-':
		return redisError(body), nil
	case ':':
		return strconv.ParseInt(body, 10, 64)
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil || n > 512<<20 {
			return nil, errors.New("invalid Redis bulk reply")
		}
		if n < 0 {
			return nil, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(body)
		if err != nil || n > 1<<20 {
			return nil, errors.New("invalid Redis array reply")
		}
		if n < 0 {
			return nil, nil
		}
		out := make([]any, 0, n)
		for i := 0; i < n; i++ {
			item, err := readRESP(r, depth+1)
			if err != nil {
				return nil, err
			}
			if e, ok := item.(redisError); ok {
				item = error(e)
			}
			out = append(out, item)
		}
		return out, nil
	}
	return nil, errors.New("unknown Redis reply type")
}

func redisString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return ""
}

func redisInt(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}

// redisInfoField reads one field of an INFO reply.
func redisInfoField(info, field string) string {
	for _, line := range strings.Split(info, "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && key == field {
			return value
		}
	}
	return ""
}

// redisKeyspace reads INFO keyspace: keys and expiring keys per database.
func redisKeyspace(info string) map[int][2]int64 {
	out := map[int][2]int64{}
	for _, line := range strings.Split(info, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || !strings.HasPrefix(key, "db") {
			continue
		}
		db, err := strconv.Atoi(key[2:])
		if err != nil {
			continue
		}
		var keys, expires int64
		for _, part := range strings.Split(value, ",") {
			k, v, _ := strings.Cut(part, "=")
			n, _ := strconv.ParseInt(v, 10, 64)
			switch k {
			case "keys":
				keys = n
			case "expires":
				expires = n
			}
		}
		out[db] = [2]int64{keys, expires}
	}
	return out
}

// ---- shared host Redis -----------------------------------------------------

const sharedRedisConfig = "/etc/redis/redis.conf"

// sharedRedisServer is the distribution's redis-server.service as its
// configuration describes it.
type sharedRedisServer struct {
	Port     int
	Socket   string
	Password string
}

func (s sharedRedisServer) dial(ctx context.Context) (redisConn, error) {
	if s.Port > 0 {
		return dialTuningRedis(ctx, "tcp", "127.0.0.1:"+strconv.Itoa(s.Port), s.Password)
	}
	return dialTuningRedis(ctx, "unix", s.Socket, s.Password)
}

// parseSharedRedisConfig reads port, unixsocket and requirepass.
func parseSharedRedisConfig(raw string) (sharedRedisServer, bool) {
	s := sharedRedisServer{Port: 6379}
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		value := strings.Trim(fields[1], `"`)
		switch strings.ToLower(fields[0]) {
		case "port":
			s.Port, _ = strconv.Atoi(value)
		case "unixsocket":
			s.Socket = value
		case "requirepass":
			s.Password = value
		}
	}
	return s, s.Port > 0 || s.Socket != ""
}

func readSharedRedis() (sharedRedisServer, bool) {
	raw, err := os.ReadFile(hostPath(sharedRedisConfig))
	if err != nil {
		return sharedRedisServer{}, false
	}
	return parseSharedRedisConfig(string(raw))
}

// redisEnvUse is one environment variable that points at the shared Redis.
type redisEnvUse struct {
	Key string
	DB  int
}

var redisSocketPath = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)

// sharedRedisUses finds the variables of env that point at the shared
// server, with their database numbers.
func sharedRedisUses(env map[string]string, server sharedRedisServer) []redisEnvUse {
	out := []redisEnvUse{}
	for key, raw := range env {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || (u.Scheme != "redis" && u.Scheme != "rediss" && u.Scheme != "unix") {
			continue
		}
		db := 0
		switch u.Scheme {
		case "unix":
			if server.Socket == "" || u.Path != server.Socket {
				continue
			}
			if v := u.Query().Get("db"); v != "" {
				db, _ = strconv.Atoi(v)
			}
		default:
			host := u.Hostname()
			port := u.Port()
			if port == "" {
				port = "6379"
			}
			if (host != "127.0.0.1" && host != "localhost" && host != "::1") || port != strconv.Itoa(server.Port) {
				continue
			}
			if path := strings.Trim(u.Path, "/"); path != "" {
				db, _ = strconv.Atoi(path)
			}
		}
		if db < 0 || db > 15 {
			continue
		}
		out = append(out, redisEnvUse{Key: key, DB: db})
	}
	return out
}

// probeRedisUse samples up to limit keys of db and classifies them.
func probeRedisUse(ctx context.Context, c redisConn, db, limit int) (string, int, error) {
	if _, err := c.do(ctx, "SELECT", strconv.Itoa(db)); err != nil {
		return "", 0, err
	}
	keys := []string{}
	cursor := "0"
	for i := 0; i < 20 && len(keys) < limit; i++ {
		reply, err := c.do(ctx, "SCAN", cursor, "COUNT", "200")
		if err != nil {
			return "", 0, err
		}
		parts, _ := reply.([]any)
		if len(parts) != 2 {
			return "", 0, errors.New("unexpected SCAN reply")
		}
		cursor = redisString(parts[0])
		batch, _ := parts[1].([]any)
		for _, k := range batch {
			keys = append(keys, redisString(k))
		}
		if cursor == "0" {
			break
		}
	}
	if len(keys) > limit {
		keys = keys[:limit]
	}
	expiring := 0
	for _, key := range keys[:min(len(keys), 200)] {
		ttl, err := c.do(ctx, "PTTL", key)
		if err == nil && redisInt(ttl) > 0 {
			expiring++
		}
	}
	sampled := min(len(keys), 200)
	// Scale the expiring count to the whole sample.
	if sampled > 0 && len(keys) > sampled {
		expiring = expiring * len(keys) / sampled
	}
	return classifyRedisKeys(keys, expiring), len(keys), nil
}

// ---- migration -------------------------------------------------------------

// RedisMigration is the record of moving one application off the shared
// Redis; it stays in agent state so the panel shows the outcome.
type RedisMigration struct {
	AppID    string    `json:"app_id"`
	DBs      []int     `json:"dbs"`
	Use      string    `json:"use"`
	Status   string    `json:"status"` // running, succeeded, failed
	Step     string    `json:"step,omitempty"`
	Copied   int64     `json:"copied,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	Error    string    `json:"error,omitempty"`
}

// redisMigrationSteps are the host actions a migration needs; Manager
// supplies the real ones and tests fakes.
type redisMigrationSteps struct {
	Source func(ctx context.Context) (redisConn, error)
	Target func(ctx context.Context) (redisConn, error)
	// Provision creates or repairs the application's own instance and
	// returns its REDIS_URL for a database number.
	Provision func(ctx context.Context) (func(db int) string, error)
	Stop      func(ctx context.Context) error
	Start     func(ctx context.Context) error // starts or restarts the application's units
	SwitchEnv func(ctx context.Context, changes map[string]string) (restore func(context.Context) error, err error)
	Healthy   func(ctx context.Context) error
	Log       func(format string, args ...any)
}

// runRedisMigration moves uses (environment variables and their database
// numbers) of one application from the shared server to its own instance.
//
//  1. provision lidza-redis@<id>;
//  2. for queue or persistent data, stop the application's units and copy
//     every key of each database with DUMP/RESTORE (keeping TTLs); a cache
//     starts empty and the application keeps running;
//  3. point the variables at the new instance (same database numbers) and
//     start or restart the application;
//  4. verify: the units are healthy, the new instance has the copied keys
//     and no client is left on the old databases;
//  5. only then FLUSHDB each old database.
//
// A failure before step 5 restores the old variables and restarts the
// application; the old databases are never touched.
func runRedisMigration(ctx context.Context, s redisMigrationSteps, uses []redisEnvUse, use string, record *RedisMigration) (err error) {
	logf := s.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	step := func(name string) { record.Step = name; logf("%s", name) }
	dbs := uniqueDBs(uses)
	copyKeys := use != tuningRedisUseCache
	step("Provisioning the application's own Redis instance")
	urlFor, err := s.Provision(ctx)
	if err != nil {
		return fmt.Errorf("provisioning failed: %w", err)
	}
	source, err := s.Source(ctx)
	if err != nil {
		return err
	}
	defer source.close()
	target, err := s.Target(ctx)
	if err != nil {
		return err
	}
	defer target.close()
	// The new instance is unused until the variables change; clear what an
	// earlier, failed attempt copied.
	for _, db := range dbs {
		if _, err := target.do(ctx, "SELECT", strconv.Itoa(db)); err != nil {
			return err
		}
		if _, err := target.do(ctx, "FLUSHDB"); err != nil {
			return err
		}
	}
	stopped := false
	var restore func(context.Context) error
	defer func() {
		if err == nil {
			return
		}
		rollback, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		if restore != nil {
			if rerr := restore(rollback); rerr != nil {
				err = fmt.Errorf("%w; restoring the old Redis settings also failed: %v", err, rerr)
				return
			}
		}
		if stopped || restore != nil {
			if serr := s.Start(rollback); serr != nil {
				err = fmt.Errorf("%w; restarting the application on the old Redis failed: %v", err, serr)
				return
			}
		}
		logf("Rolled back: the application uses the shared Redis again; its data there is unchanged")
	}()
	expected := map[int]int64{}
	if copyKeys {
		step("Stopping the application while its keys are copied")
		if err = s.Stop(ctx); err != nil {
			return fmt.Errorf("stopping the application failed: %w", err)
		}
		stopped = true
		for _, db := range dbs {
			step(fmt.Sprintf("Copying database %d", db))
			n, cerr := copyRedisDB(ctx, source, target, db)
			if cerr != nil {
				err = fmt.Errorf("copying database %d failed: %w", db, cerr)
				return err
			}
			expected[db] = n
			record.Copied += n
			logf("Copied %d keys of database %d", n, db)
		}
	}
	step("Pointing the application at its own Redis")
	changes := map[string]string{}
	for _, u := range uses {
		changes[u.Key] = urlFor(u.DB)
	}
	if restore, err = s.SwitchEnv(ctx, changes); err != nil {
		restore = nil
		return fmt.Errorf("updating the environment failed: %w", err)
	}
	step("Starting the application")
	if err = s.Start(ctx); err != nil {
		return fmt.Errorf("starting the application failed: %w", err)
	}
	step("Verifying")
	if err = s.Healthy(ctx); err != nil {
		return fmt.Errorf("the application is not healthy on its own Redis: %w", err)
	}
	for db, n := range expected {
		if _, err = target.do(ctx, "SELECT", strconv.Itoa(db)); err != nil {
			return err
		}
		size, derr := target.do(ctx, "DBSIZE")
		if derr != nil {
			err = derr
			return err
		}
		// Expiring keys may lapse, and the application may delete keys
		// once it runs; a large shortfall is a failed copy.
		if got := redisInt(size); got*10 < n*9 && n-got > 10 {
			err = fmt.Errorf("database %d has %d keys on the new instance, %d were copied", db, got, n)
			return err
		}
	}
	if err = waitNoClients(ctx, source, dbs); err != nil {
		return err
	}
	step("Clearing the old databases on the shared Redis")
	restore = nil // verified: from here on the new instance is authoritative
	for _, db := range dbs {
		if _, ferr := source.do(ctx, "SELECT", strconv.Itoa(db)); ferr != nil {
			return fmt.Errorf("the application runs on its own Redis; clearing old database %d failed: %v", db, ferr)
		}
		if _, ferr := source.do(ctx, "FLUSHDB", "ASYNC"); ferr != nil {
			return fmt.Errorf("the application runs on its own Redis; clearing old database %d failed: %v", db, ferr)
		}
		logf("Cleared database %d of the shared Redis", db)
	}
	return nil
}

func uniqueDBs(uses []redisEnvUse) []int {
	seen := map[int]bool{}
	out := []int{}
	for _, u := range uses {
		if !seen[u.DB] {
			seen[u.DB] = true
			out = append(out, u.DB)
		}
	}
	return out
}

// copyRedisDB copies every key of db with DUMP and RESTORE, keeping TTLs.
func copyRedisDB(ctx context.Context, source, target redisConn, db int) (int64, error) {
	for _, c := range []redisConn{source, target} {
		if _, err := c.do(ctx, "SELECT", strconv.Itoa(db)); err != nil {
			return 0, err
		}
	}
	var copied int64
	cursor := "0"
	for {
		reply, err := source.do(ctx, "SCAN", cursor, "COUNT", "500")
		if err != nil {
			return copied, err
		}
		parts, _ := reply.([]any)
		if len(parts) != 2 {
			return copied, errors.New("unexpected SCAN reply")
		}
		cursor = redisString(parts[0])
		batch, _ := parts[1].([]any)
		for _, item := range batch {
			key := redisString(item)
			ttl, err := source.do(ctx, "PTTL", key)
			if err != nil {
				return copied, err
			}
			ms := redisInt(ttl)
			if ms == -2 {
				continue // expired meanwhile
			}
			if ms < 0 {
				ms = 0
			}
			payload, err := source.do(ctx, "DUMP", key)
			if err != nil {
				return copied, err
			}
			if payload == nil {
				continue
			}
			if _, err := target.do(ctx, "RESTORE", key, strconv.FormatInt(ms, 10), redisString(payload), "REPLACE"); err != nil {
				return copied, err
			}
			copied++
		}
		if cursor == "0" {
			return copied, nil
		}
	}
}

// waitNoClients waits until no client other than this one has one of dbs
// selected on the shared server: every process of the application now uses
// its own instance, and nothing else shares the databases.
func waitNoClients(ctx context.Context, source redisConn, dbs []int) error {
	ownID := ""
	if id, err := source.do(ctx, "CLIENT", "ID"); err == nil {
		ownID = redisString(id)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		reply, err := source.do(ctx, "CLIENT", "LIST")
		if err != nil {
			return err
		}
		left := redisClientsOn(redisString(reply), ownID, dbs)
		if left == 0 {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("%d clients still use the old databases on the shared Redis; they are kept", left)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(redisClientPoll):
		}
	}
}

var redisClientPoll = 2 * time.Second

func redisClientsOn(list, ownID string, dbs []int) int {
	count := 0
	for _, line := range strings.Split(list, "\n") {
		fields := map[string]string{}
		for _, part := range strings.Fields(line) {
			if k, v, ok := strings.Cut(part, "="); ok {
				fields[k] = v
			}
		}
		if fields["id"] == "" || fields["id"] == ownID {
			continue
		}
		db, err := strconv.Atoi(fields["db"])
		if err != nil {
			continue
		}
		for _, d := range dbs {
			if d == db {
				count++
			}
		}
	}
	return count
}
