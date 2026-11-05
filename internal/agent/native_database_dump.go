package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/agim/lidza/packs/storage"
)

type nativePGDumpInput struct {
	URL          string `json:"url"`
	PeerDatabase string `json:"peer_database,omitempty"`
	Major        int    `json:"major"`
	ToolSHA256   string `json:"tool_sha256"`
	Lock         string `json:"lock"`
}

// RunNativePGDump is the agent's short-lived trusted-tool mode. It opens
// no manager, key, listener or application. Credentials arrive through stdin,
// never a process argument, app file or environment inherited by systemd-run.
func RunNativePGDump(ctx context.Context, input io.Reader, output io.Writer) error {
	decoder := json.NewDecoder(io.LimitReader(input, 16<<10))
	decoder.DisallowUnknownFields()
	var spec nativePGDumpInput
	if decoder.Decode(&spec) != nil {
		return errors.New("native dump input is invalid")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("native dump input has trailing data")
	}
	tool, err := openNativeDumpTool(spec.Major, spec.ToolSHA256)
	if err != nil {
		return err
	}
	defer tool.Close()
	if len(spec.URL) > 8192 || !filepath.IsAbs(spec.Lock) || filepath.Clean(spec.Lock) != spec.Lock || filepath.Base(filepath.Dir(spec.Lock)) != ".lidza-native-pg-locks" || !idPattern.MatchString(strings.TrimSuffix(filepath.Base(spec.Lock), ".lock")) || !strings.HasSuffix(spec.Lock, ".lock") {
		return errors.New("native dump requires a private bound operation lock")
	}
	parent := filepath.Dir(spec.Lock)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("native dump operation lock directory is not private")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("native dump lock directory has a different owner")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return errors.New("native dump operation lock is unavailable")
	}
	defer root.Close()
	lock, err := root.OpenFile(filepath.Base(spec.Lock), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return errors.New("native dump operation lock is unavailable")
	}
	defer lock.Close()
	lockInfo, err := lock.Stat()
	if err != nil || !lockInfo.Mode().IsRegular() {
		return errors.New("native dump operation lock is invalid")
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("native dump is already running; no concurrent copy started")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	clean := []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8", "PGOPTIONS=-c default_transaction_read_only=on -c statement_timeout=900000 -c lock_timeout=3000", "PGAPPNAME=lidza-native-backup", "PGPASSFILE=/nonexistent"}
	var environment []string
	var credential *syscall.Credential
	if spec.PeerDatabase != "" {
		if spec.URL != "" || !strings.HasPrefix(filepath.Base(spec.Lock), "native-archive-") || !nativePostgresDatabasePath.MatchString("/"+spec.PeerDatabase) {
			return errors.New("standalone native dump peer scope is invalid")
		}
		credential, err = nativeArchivePeerCredential()
		if err != nil {
			return err
		}
		environment = []string{"PGHOST=" + nativeArchiveSocket, "PGPORT=5432", "PGUSER=postgres", "PGDATABASE=" + spec.PeerDatabase, "PGCONNECT_TIMEOUT=5"}
	} else {
		// Parse again to keep libpq host, service and passfile overrides out of
		// the trusted helper's arguments and environment.
		u, err := urlForNativeDump(spec.URL)
		if err != nil {
			return err
		}
		environment, _ = postgresEnvironment(u)
	}
	for _, entry := range environment {
		if !strings.HasPrefix(entry, "PGOPTIONS=") && !strings.HasPrefix(entry, "PGAPPNAME=") {
			clean = append(clean, entry)
		}
	}
	// FD4 names the verified inode even if a package update replaces its pathname.
	command := exec.CommandContext(ctx, "/proc/self/fd/4", "--no-password", "--format=custom", "--compress=zstd:1", "--no-owner", "--no-acl", "--lock-wait-timeout=3s")
	command.Env = clean
	if credential != nil {
		command.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	}
	command.Stdout = output
	command.ExtraFiles = []*os.File{lock, tool}
	command.WaitDelay = 5 * time.Second
	var stderr limitedBuffer
	command.Stderr = &stderr
	if err = command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if strings.Contains(stderr.String(), "LOCK TABLE") && strings.Contains(stderr.String(), "statement timeout") {
			return errors.New("native PostgreSQL backup lock wait exceeded three seconds; source retained")
		}
		return errors.New("native PostgreSQL dump failed; source database and application retained")
	}
	return nil
}

func urlForNativeDump(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return "", errors.New("native PostgreSQL dump URL is invalid")
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	if err := validateNativeDatabaseURL(raw, NativeDatabasePolicy{Host: u.Hostname(), Port: port}); err != nil {
		return "", err
	}
	return raw, nil
}

func (m *Manager) nativeDatabaseDumpPolicy(ctx context.Context, d Database) (*NativeDatabasePolicy, error) {
	m.refreshNativeHostDataTool(ctx, d.AppID) // follow verified pg_dump package updates
	m.mu.Lock()
	binding, native := m.data.NativeDatabases[d.AppID]
	if !native {
		m.mu.Unlock()
		return nil, nil
	}
	allowed := m.nativeDatabaseInUseLocked(d.AppID)
	if binding.Policy.ArchiveDatabase != "" && (!d.Backup.Offsite || m.data.BackupStorage == nil) {
		allowed = false
	}
	m.mu.Unlock()
	if !allowed || d.Mode != "external" {
		return nil, errors.New("native database binding/policy is unavailable; backup refused")
	}
	if len(m.cfg.BackupExcludeTableData[d.AppID]) > 0 {
		return nil, errors.New("native release backups require the complete database; exclusions refused")
	}
	raw, err := m.nativeDatabaseSource(binding.Policy)
	if err != nil || raw != d.URL {
		return nil, errors.New("native database source changed; existing backup retained")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err = checkNativeBackupSource(probeCtx, raw, binding.Policy); err != nil {
		return nil, err
	}
	p := binding.Policy
	return &p, nil
}

func (m *Manager) dumpNativeDatabase(ctx context.Context, d Database, p NativeDatabasePolicy, output io.Writer) error {
	if strings.ContainsAny(m.cfg.DataDir, " \t\r\n:") {
		return errors.New("native backup data directory requires review")
	}
	dir := filepath.Join(m.cfg.DataDir, ".lidza-native-pg-locks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("native backup lock directory is unavailable")
	}
	binary, err := os.Executable()
	if err != nil {
		return errors.New("private backup helper is unavailable")
	}
	if m.nativePGExecutable != "" {
		binary = m.nativePGExecutable
	}
	maintenanceURL, err := nativePostgresMaintenanceURL(d.URL, p)
	if err != nil {
		return err
	}
	spec := nativePGDumpInput{URL: maintenanceURL, Major: p.PostgresMajor, ToolSHA256: p.DumpSHA256, Lock: filepath.Join(dir, d.AppID+".lock")}
	capabilities := ""
	if p.ArchiveDatabase != "" {
		spec.URL, spec.PeerDatabase = "", p.ArchiveDatabase
		capabilities = "CAP_SETUID CAP_SETGID"
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return errors.New("native backup input could not be prepared")
	}
	unit := "lidza-native-pg-" + newID() + ".service"
	args := []string{"--quiet", "--wait", "--pipe", "--collect", "--expand-environment=no", "--unit=" + unit, "--property=User=root", "--property=CPUQuota=50%", "--property=MemoryMax=536870912", "--property=MemorySwapMax=0", "--property=TasksMax=64", "--property=RuntimeMaxSec=1800", "--property=ProtectSystem=strict", "--property=ProtectHome=tmpfs", "--property=PrivateTmp=yes", "--property=NoNewPrivileges=yes", "--property=CapabilityBoundingSet=" + capabilities, "--property=KillMode=control-group", "--property=BindPaths=" + dir, "--property=ReadWritePaths=" + dir, "--", binary, "-native-pg-dump"}
	command := exec.CommandContext(ctx, "/usr/bin/systemd-run", args...)
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8"}
	command.Stdin = strings.NewReader(string(raw))
	command.Stdout = output
	command.WaitDelay = 5 * time.Second
	var stderr limitedBuffer
	command.Stderr = &stderr
	err = command.Run()
	if ctx.Err() != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		exec.CommandContext(cleanup, "/usr/bin/systemctl", "stop", unit).Run()
		return ctx.Err()
	}
	if err != nil {
		return errors.New("isolated native PostgreSQL backup failed; source retained")
	}
	return nil
}

func nativeBackupPrivacy(ctx context.Context, cfg storage.Config, key string) error {
	store, err := storage.New(cfg)
	if err != nil {
		return errors.New("native off-site storage is invalid")
	}
	var report storage.PrivacyReport
	if _, err = store.Stat(ctx, key); errors.Is(err, storage.ErrNotFound) {
		report, err = store.PrivacyProbe(ctx, key)
		if err == nil {
			if _, absent := store.Stat(ctx, key); !errors.Is(absent, storage.ErrNotFound) {
				return errors.New("native backup privacy probe was not removed; no snapshot uploaded")
			}
		}
	} else if err == nil {
		report, err = store.Privacy(ctx, key)
	}
	if err != nil || !report.Private || report.Public {
		return errors.New("native off-site object/listing privacy was not verified; no snapshot uploaded")
	}
	return nil
}

func verifyNativeOffsiteBackup(ctx context.Context, cfg storage.Config, key string, record BackupRecord) error {
	store, err := storage.New(cfg)
	if err != nil {
		return errors.New("native off-site storage is invalid")
	}
	privacy, err := store.Privacy(ctx, key)
	if err != nil || !privacy.Private || privacy.Public {
		return errors.New("native off-site privacy changed; backup is not verified")
	}
	stream, object, err := store.Get(ctx, key)
	if err != nil {
		return errors.New("native off-site checksum readback failed")
	}
	defer stream.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(stream, record.Size+1))
	if err != nil || n != record.Size || object.Size != record.Size || hex.EncodeToString(hash.Sum(nil)) != record.SHA256 {
		return errors.New("native off-site readback checksum/size differs; current release retained")
	}
	return nil
}
