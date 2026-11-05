package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// One Redis server per application, never a shared instance. Each runs from
// the systemd template lidza-redis@<id>.service as the application's own
// account, so its data directory (<root>/shared/redis, mode 0700) and its
// socket (mode 0700) are private to that account; other applications cannot
// connect at all, independently of the password. It listens on no TCP port.
//
// Persistence is the append-only file in the data directory (plus Redis'
// default RDB snapshots). There is no separate Redis backup: Redis holds
// queues and caches, and the files live in the application's shared
// directory with the rest of its persistent state.
//
// REDIS_URL uses the unix:// form with the password as URL userinfo and the
// database in the query: unix://:<password>@<socket>?db=0. redis-client (the
// Rails 7.1+/Sidekiq 7 driver behind redis-rb 5, ActionCable and
// RedisCacheStore) and go-redis parse this form. ioredis and node-redis do
// not read unix:// URLs; Node applications connect with REDIS_SOCKET and
// REDIS_PASSWORD, for example new Redis({path, password}).

const (
	nativeRedisTemplateUnit = "/etc/systemd/system/lidza-redis@.service"
	nativeRedisConfigDir    = "/etc/lidza/redis"
	nativeRedisDefaultMiB   = 128
	nativeRedisServer       = "/usr/bin/redis-server"
)

func nativeRedisUnit(id string) string { return "lidza-redis@" + id + ".service" }

func nativeRedisDir(root string) string { return filepath.Join(root, "shared", "redis") }

func nativeRedisSocket(root string) string { return filepath.Join(nativeRedisDir(root), "redis.sock") }

func nativeRedisURL(socket, password string) string {
	return "unix://:" + password + "@" + socket + "?db=0"
}

// renderNativeRedisTemplate is the shared, hardened template unit. Account,
// writable data directory and memory limit come from the instance drop-in.
func renderNativeRedisTemplate() string {
	return `# Managed by Līdza Deploy: one private Redis server per application.
[Unit]
Description=Redis for application %i
After=network.target
ConditionPathExists=/etc/lidza/redis/%i.conf

[Service]
Type=notify
ExecStart=/usr/bin/redis-server /etc/lidza/redis/%i.conf --supervised systemd --daemonize no
Restart=on-failure
RestartSec=2s
TimeoutStopSec=60s
UMask=0077
LimitNOFILE=10032
TasksMax=64
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictAddressFamilies=AF_UNIX
IPAddressDeny=any
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
`
}

// renderNativeRedisDropIn binds one instance to its account and data
// directory. MemoryMax leaves room for the copy-on-write fork of an AOF
// rewrite above Redis' own maxmemory.
func renderNativeRedisDropIn(account NativeAppAccount, dir string, maxMiB int) string {
	return fmt.Sprintf(`# Managed by Līdza Deploy.
[Service]
User=%s
Group=%d
ReadWritePaths=%s
MemoryMax=%dM
MemorySwapMax=0
`, account.User, account.GID, dir, 2*maxMiB+64)
}

// renderNativeRedisConfig is the instance configuration. It uses only
// directives Redis 6.0 and later understand. noeviction is the default:
// job queues (Sidekiq and similar) must never lose keys silently.
func renderNativeRedisConfig(id, dir, password string, maxMiB int) string {
	return fmt.Sprintf(`# Managed by Līdza Deploy for application %s. Changes are overwritten.
port 0
unixsocket %s
unixsocketperm 700
protected-mode yes
daemonize no
loglevel notice
logfile ""
databases 16
dir %s
dbfilename dump.rdb
appendonly yes
appendfilename "appendonly.aof"
appendfsync everysec
maxmemory %dmb
maxmemory-policy noeviction
requirepass %s
`, id, filepath.Join(dir, "redis.sock"), dir, maxMiB, password)
}

// validNativeRedisPath keeps paths usable unquoted in Redis and systemd
// configuration and within the Unix socket path limit.
func validNativeRedisPath(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" || strings.ContainsAny(root, " \t\r\n\"'\\%$;#") {
		return errors.New("application root is not usable for Redis")
	}
	if len(nativeRedisSocket(root)) > 100 {
		return errors.New("application root is too long for a Redis socket path")
	}
	return nil
}

// ensureRedisPackage installs redis-server when absent. Only when this call
// installed it does it disable the distribution's default instance, which
// nothing can be using yet; an existing redis-server.service is never touched.
func (h nativeHostData) ensureRedisPackage(ctx context.Context) error {
	if h.fileExists(nativeRedisServer) {
		return nil
	}
	if h.coexist {
		return errors.New("this agent shares its host; install redis-server there before creating applications with Redis")
	}
	if out, err := h.aptInstall(ctx, "redis-server"); err != nil {
		if _, updateErr := h.run(ctx, "", "/usr/bin/apt-get", "-o", "DPkg::Lock::Timeout=300", "update", "-q"); updateErr != nil {
			return fmt.Errorf("redis-server installation failed: %s", nativeTrimOutput(out))
		}
		if out, err = h.aptInstall(ctx, "redis-server"); err != nil {
			return fmt.Errorf("redis-server installation failed: %s", nativeTrimOutput(out))
		}
	}
	if out, err := h.run(ctx, "", "/usr/bin/systemctl", "disable", "--now", "redis-server.service"); err != nil {
		return fmt.Errorf("the newly installed default Redis could not be disabled: %s", nativeTrimOutput(out))
	}
	return nil
}

// ensureRedis installs and starts the application's instance. It restarts a
// running instance only when its configuration changed.
func (h nativeHostData) ensureRedis(ctx context.Context, id string, account NativeAppAccount, password string, maxMiB int) error {
	if err := validNativeRedisPath(account.Root); err != nil {
		return err
	}
	if err := h.ensureRedisPackage(ctx); err != nil {
		return err
	}
	dir := nativeRedisDir(account.Root)
	if err := h.privateDir(dir, account); err != nil {
		return err
	}
	if _, err := h.writeIfChanged(nativeRedisTemplateUnit, []byte(renderNativeRedisTemplate()), 0644, 0, 0); err != nil {
		return err
	}
	if err := os.MkdirAll(h.path(nativeRedisConfigDir), 0755); err != nil {
		return err
	}
	unit := nativeRedisUnit(id)
	dropIn, err := h.writeIfChanged(filepath.Join("/etc/systemd/system", unit+".d", "lidza.conf"), []byte(renderNativeRedisDropIn(account, dir, maxMiB)), 0644, 0, 0)
	if err != nil {
		return err
	}
	// The password is readable by root and the application's group only.
	config, err := h.writeIfChanged(filepath.Join(nativeRedisConfigDir, id+".conf"), []byte(renderNativeRedisConfig(id, dir, password, maxMiB)), 0640, 0, account.GID)
	if err != nil {
		return err
	}
	if out, err := h.run(ctx, "", "/usr/bin/systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemd reload failed: %s", nativeTrimOutput(out))
	}
	action := "start"
	if dropIn || config {
		action = "restart"
	}
	if out, err := h.run(ctx, "", "/usr/bin/systemctl", "enable", unit); err != nil {
		return fmt.Errorf("enabling %s failed: %s", unit, nativeTrimOutput(out))
	}
	if out, err := h.run(ctx, "", "/usr/bin/systemctl", action, unit); err != nil {
		return fmt.Errorf("starting %s failed: %s", unit, nativeTrimOutput(out))
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err = h.redisMemory(ctx, h.path(nativeRedisSocket(account.Root)), password)
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// removeRedis stops and removes one instance's unit configuration. Its data
// directory lives in the application root, which the caller purges.
func (h nativeHostData) removeRedis(ctx context.Context, id string) error {
	unit := nativeRedisUnit(id)
	if out, err := h.run(ctx, "", "/usr/bin/systemctl", "disable", "--now", unit); err != nil && !strings.Contains(out, "not loaded") && !strings.Contains(out, "does not exist") {
		return fmt.Errorf("stopping %s failed: %s", unit, nativeTrimOutput(out))
	}
	for _, path := range []string{filepath.Join(nativeRedisConfigDir, id+".conf"), filepath.Join("/etc/systemd/system", unit+".d", "lidza.conf")} {
		if err := os.Remove(h.path(path)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	_ = os.Remove(h.path(filepath.Join("/etc/systemd/system", unit+".d")))
	if out, err := h.run(ctx, "", "/usr/bin/systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemd reload failed: %s", nativeTrimOutput(out))
	}
	return nil
}

// privateDir creates dir (0700, owned by the account) without following a
// symlink an application could have placed in its shared directory.
func (h nativeHostData) privateDir(dir string, account NativeAppAccount) error {
	path := h.path(dir)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err = os.Mkdir(path, 0700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil || !info.IsDir() {
		return errors.New("Redis data directory is not a directory")
	}
	if h.chown {
		if err = os.Lchown(path, account.UID, account.GID); err != nil {
			return err
		}
	}
	return os.Chmod(path, 0700)
}

// writeIfChanged atomically writes a root-managed file and reports whether
// its content changed.
func (h nativeHostData) writeIfChanged(name string, data []byte, mode os.FileMode, uid, gid int) (bool, error) {
	path := h.path(name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false, err
	}
	if old, err := os.ReadFile(path); err == nil && string(old) == string(data) {
		info, statErr := os.Lstat(path)
		if statErr == nil && info.Mode().Perm() == mode {
			return false, nil
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lidza-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(mode); err == nil && h.chown {
		err = tmp.Chown(uid, gid)
	}
	if err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), path)
}

// nativeRedisMemory is the subset of INFO memory the panel shows.
type nativeRedisMemory struct {
	Used int64
	Max  int64
}

// nativeRedisInfo authenticates over the socket and reads INFO memory with a
// minimal RESP exchange; the password is sent only on the socket.
func nativeRedisInfo(ctx context.Context, socket, password string) (nativeRedisMemory, error) {
	var memory nativeRedisMemory
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return memory, errors.New("Redis socket is unavailable")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	if _, err = conn.Write([]byte(nativeRESP("AUTH", password) + nativeRESP("INFO", "memory"))); err != nil {
		return memory, err
	}
	line, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "+OK" {
		return memory, errors.New("Redis authentication failed")
	}
	header, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(header, "$") {
		return memory, errors.New("Redis INFO failed")
	}
	size, err := strconv.Atoi(strings.TrimSpace(header[1:]))
	if err != nil || size < 0 || size > 1<<20 {
		return memory, errors.New("Redis INFO reply is invalid")
	}
	body := make([]byte, size+2)
	if _, err = io.ReadFull(reader, body); err != nil {
		return memory, err
	}
	return parseNativeRedisMemory(string(body)), nil
}

func nativeRESP(args ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, arg := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(arg), arg)
	}
	return b.String()
}

func parseNativeRedisMemory(info string) nativeRedisMemory {
	var memory nativeRedisMemory
	for _, line := range strings.Split(info, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		number, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "used_memory":
			memory.Used = number
		case "maxmemory":
			memory.Max = number
		}
	}
	return memory
}
