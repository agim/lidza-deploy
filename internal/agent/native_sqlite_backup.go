package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const nativeSQLiteTool = "/usr/bin/sqlite3"
const nativeSQLiteMount = "/run/lidza-native-sqlite-source"

var nativeSQLiteFile = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,100}\.(?:db|sqlite|sqlite3)$`)

func validNativeSQLitePath(path string) bool {
	base := filepath.Base(path)
	return nativeSQLiteFile.MatchString(base) && (path == base || path == "storage/"+base)
}

func (m *Manager) nativeSQLiteSource(app NativeApplication, policy NativeDatabasePolicy) (string, error) {
	source := filepath.Join(m.cfg.Native.Root, app.Name, "shared", policy.SQLitePath)
	if !validNativeSQLitePath(policy.SQLitePath) || !filepath.IsAbs(source) || strings.ContainsAny(source, " \t\r\n:%") {
		return "", errors.New("native SQLite source path is invalid")
	}
	values, err := m.nativeBuildEnvironment(app.Name)
	if err != nil {
		return "", err
	}
	for _, role := range policy.Roles {
		selector := strings.ToUpper(role) + "_DATABASE_URL"
		if selector != policy.EnvironmentKey && values[selector] != "" ||
			role == "primary" && policy.EnvironmentKey != "DATABASE_URL" && values["DATABASE_URL"] != "" {
			return "", errors.New("native SQLite database has an unreviewed connection override")
		}
	}
	if policy.EnvironmentKey != "" {
		raw := values[policy.EnvironmentKey]
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "sqlite3" || u.Opaque != "" || u.Host != "" || u.User != nil ||
			u.Path != source || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return "", errors.New("native SQLite environment selector differs from its reviewed file")
		}
	}
	if _, err := nativeSQLiteIdentity(source); err != nil {
		return "", err
	}
	return source, nil
}

type nativeSQLiteFileIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func nativeSQLiteIdentity(path string) (nativeSQLiteFileIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxBackupSize {
		return nativeSQLiteFileIdentity{}, errors.New("native SQLite source is missing, unsafe or oversized")
	}
	resolved, err := filepath.EvalSymlinks(path)
	stat, ok := info.Sys().(*syscall.Stat_t)
	if err != nil || resolved != path || !ok || stat.Nlink != 1 {
		return nativeSQLiteFileIdentity{}, errors.New("native SQLite source must have no symbolic or hard links")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		sidecar, err := os.Lstat(path + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !sidecar.Mode().IsRegular() {
			return nativeSQLiteFileIdentity{}, errors.New("native SQLite sidecar is unsafe")
		}
		sidecarStat, ok := sidecar.Sys().(*syscall.Stat_t)
		if !ok || sidecarStat.Nlink != 1 || sidecarStat.Uid != stat.Uid {
			return nativeSQLiteFileIdentity{}, errors.New("native SQLite sidecar has an unreviewed owner or link")
		}
	}
	return nativeSQLiteFileIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}, nil
}

func checkNativeBackupSource(ctx context.Context, source string, policy NativeDatabasePolicy) error {
	if policy.SQLitePath != "" {
		tool, err := openNativeBackupTool(nativeSQLiteTool, policy.DumpSHA256)
		if err != nil {
			return err
		}
		return tool.Close()
	}
	if err := verifyNativeDumpTool(policy.PostgresMajor, policy.DumpSHA256); err != nil {
		return err
	}
	if policy.ArchiveDatabase != "" {
		if source != nativeArchiveMetadataURL(policy.ArchiveDatabase) {
			return errors.New("standalone native backup source does not match its approved identity")
		}
		return probeNativeArchiveDatabase(ctx, policy)
	}
	if policy.MaintenancePort != "" {
		return verifyNativePostgresMaintenance(ctx, source, policy)
	}
	return probeNativeDatabase(ctx, source, policy.PostgresMajor)
}

type nativeSQLiteSnapshotInput struct {
	Name       string                   `json:"name"`
	Identity   nativeSQLiteFileIdentity `json:"identity"`
	ToolSHA256 string                   `json:"tool_sha256"`
	Sidecars   bool                     `json:"sidecar_coordination,omitempty"`
}

// RunNativeSQLiteSnapshot runs inside the unprivileged transient unit. The
// bind exposes the DB, WAL and SHM together and keeps the source read-only.
func RunNativeSQLiteSnapshot(ctx context.Context, input io.Reader, output io.Writer) error {
	decoder := json.NewDecoder(io.LimitReader(input, 4096))
	decoder.DisallowUnknownFields()
	var spec nativeSQLiteSnapshotInput
	var extra any
	if decoder.Decode(&spec) != nil || decoder.Decode(&extra) != io.EOF || !nativeSQLiteFile.MatchString(spec.Name) {
		return errors.New("native SQLite snapshot input is invalid")
	}
	source := filepath.Join(nativeSQLiteMount, spec.Name)
	var fileMount, directoryMount syscall.Statfs_t
	if syscall.Statfs(source, &fileMount) != nil || syscall.Statfs(nativeSQLiteMount, &directoryMount) != nil ||
		fileMount.Flags&1 == 0 || (directoryMount.Flags&1 == 0) != spec.Sidecars || os.Geteuid() == 0 {
		return errors.New("native SQLite snapshot requires the exact unprivileged source mount policy")
	}
	return streamNativeSQLiteSnapshot(ctx, source, spec, output)
}

func streamNativeSQLiteSnapshot(ctx context.Context, source string, spec nativeSQLiteSnapshotInput, output io.Writer) error {
	identity, err := nativeSQLiteIdentity(source)
	if err != nil || identity != spec.Identity {
		return errors.New("native SQLite source inode changed")
	}
	tool, err := openNativeBackupTool(nativeSQLiteTool, spec.ToolSHA256)
	if err != nil {
		return err
	}
	defer tool.Close()
	temp, err := os.MkdirTemp("/tmp", "lidza-native-sqlite-")
	if err != nil {
		return errors.New("native SQLite snapshot spool is unavailable")
	}
	defer os.RemoveAll(temp)
	snapshot := filepath.Join(temp, "snapshot.db")
	run := func(database string, args ...string) (string, error) {
		options := []string{"-init", "/dev/null", "-batch", "-bail", "-readonly", database}
		command := exec.CommandContext(ctx, "/proc/self/fd/3", append(options, args...)...)
		command.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8"}
		command.ExtraFiles = []*os.File{tool}
		command.WaitDelay = 3 * time.Second
		var result limitedBuffer
		command.Stdout = &result
		if err := command.Run(); err != nil {
			return "", errors.New("native SQLite snapshot or isolated validation failed")
		}
		return strings.TrimSpace(result.String()), nil
	}
	if nativeSQLiteWALMode(source) {
		err = backupNativeSQLiteWAL(ctx, source, snapshot, tool)
	} else {
		_, err = run(source, ".timeout 300", ".backup "+snapshot)
	}
	if err != nil {
		return err
	}
	if after, err := nativeSQLiteIdentity(source); err != nil || after != identity {
		return errors.New("native SQLite source inode changed during backup")
	}
	for _, sidecar := range []string{"-wal", "-shm", "-journal"} {
		if _, err = os.Lstat(snapshot + sidecar); !os.IsNotExist(err) {
			return errors.New("native SQLite snapshot is not self-contained")
		}
	}
	if result, err := run(snapshot, "PRAGMA trusted_schema=OFF; PRAGMA integrity_check"); err != nil || result != "ok" {
		return errors.New("native SQLite snapshot integrity check failed")
	}
	if result, err := run(snapshot, "PRAGMA trusted_schema=OFF; PRAGMA foreign_key_check"); err != nil || result != "" {
		return errors.New("native SQLite snapshot foreign key check failed")
	}
	file, err := os.Open(snapshot)
	if err != nil {
		return errors.New("native SQLite snapshot could not be opened")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxBackupSize {
		return errors.New("native SQLite snapshot is invalid or oversized")
	}
	n, err := io.Copy(output, io.LimitReader(file, info.Size()+1))
	if err != nil || n != info.Size() {
		return errors.New("native SQLite snapshot stream failed")
	}
	return nil
}

func backupNativeSQLiteWAL(ctx context.Context, source, snapshot string, tool *os.File) error {
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3", "-init", "/dev/null", "-batch", "-bail", "-readonly", "-noheader", "-list", source)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8"}
	cmd.ExtraFiles = []*os.File{tool}
	session, err := startNativeSQLiteSession(cmd, cancel)
	if err != nil {
		return err
	}
	defer session.close()
	// The CLI copies a few pages at a time. Keep one WAL read snapshot open
	// so commits on other connections cannot make the copy restart. Confirm
	// WAL on this connection before copying to avoid blocking rollback-mode writers.
	rows, err := session.query(".timeout 300\nPRAGMA trusted_schema=OFF;\nBEGIN;\nSELECT count(*) FROM main.sqlite_schema;\nPRAGMA main.journal_mode;")
	if err != nil || len(rows) != 2 || rows[1] != "wal" {
		return errors.New("native SQLite WAL backup could not pin a read-only snapshot")
	}
	if _, err := session.query(".backup " + snapshot + "\nROLLBACK;"); err != nil {
		return errors.New("native SQLite WAL snapshot copy failed")
	}
	return nil
}

func nativeSQLiteWALMode(source string) bool {
	file, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	var header [20]byte
	_, err = io.ReadFull(file, header[:])
	return err == nil && string(header[:16]) == "SQLite format 3\x00" && header[18] == 2 && header[19] == 2
}

func nativeSQLiteNeedsSidecars(source string) bool {
	if !nativeSQLiteWALMode(source) {
		return false
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(source + suffix); os.IsNotExist(err) {
			return true
		}
	}
	return false
}

func (m *Manager) dumpNativeSQLite(ctx context.Context, database Database, p NativeDatabasePolicy, output io.Writer) error {
	source := database.URL
	identity, err := nativeSQLiteIdentity(source)
	if err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return errors.New("private SQLite snapshot helper is unavailable")
	}
	sidecars := m.cfg.Native.SQLiteSidecarBackups[database.AppID] && nativeSQLiteNeedsSidecars(source)
	raw, err := json.Marshal(nativeSQLiteSnapshotInput{Name: filepath.Base(source), Identity: identity, ToolSHA256: p.DumpSHA256, Sidecars: sidecars})
	if err != nil {
		return errors.New("native SQLite snapshot input could not be prepared")
	}
	unit := "lidza-native-sqlite-" + newID() + ".service"
	args := []string{"--quiet", "--wait", "--pipe", "--collect", "--expand-environment=no", "--unit=" + unit,
		"--property=User=" + m.nativeDatabaseAccount(p.AppID), "--property=CPUQuota=35%", "--property=MemoryMax=268435456",
		"--property=MemorySwapMax=0", "--property=TasksMax=16", "--property=RuntimeMaxSec=180",
		"--property=LimitFSIZE=5368709120", "--property=ProtectSystem=strict", "--property=ProtectHome=tmpfs",
		"--property=PrivateTmp=yes", "--property=PrivateNetwork=yes", "--property=SystemCallFilter=~@network-io",
		"--property=NoNewPrivileges=yes", "--property=CapabilityBoundingSet=", "--property=KillMode=control-group"}
	if sidecars {
		// SQLite may create lock/WAL sidecars in an otherwise idle directory.
		// Keep the database itself on a separate read-only bind.
		args = append(args, "--property=BindPaths="+filepath.Dir(source)+":"+nativeSQLiteMount,
			"--property=ReadWritePaths="+nativeSQLiteMount,
			"--property=BindReadOnlyPaths="+source+":"+filepath.Join(nativeSQLiteMount, filepath.Base(source)))
	} else {
		args = append(args, "--property=BindReadOnlyPaths="+filepath.Dir(source)+":"+nativeSQLiteMount)
	}
	args = append(args, "--", binary, "-native-sqlite-snapshot")
	command := exec.CommandContext(ctx, "/usr/bin/systemd-run", args...)
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8"}
	command.Stdin = strings.NewReader(string(raw))
	command.Stdout = output
	command.WaitDelay = 5 * time.Second
	err = command.Run()
	if ctx.Err() != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		exec.CommandContext(cleanup, "/usr/bin/systemctl", "stop", unit).Run()
		return ctx.Err()
	}
	if err != nil {
		return errors.New("isolated native SQLite snapshot failed; source retained")
	}
	if after, err := nativeSQLiteIdentity(source); err != nil || after != identity {
		return errors.New("native SQLite source inode changed during backup")
	}
	return nil
}

// nativeSQLiteSession drives one pinned sqlite3 CLI process over stdin/stdout,
// so a single connection can hold a read snapshot across several commands.
type nativeSQLiteSession struct {
	command *exec.Cmd
	in      io.WriteCloser
	out     *bufio.Scanner
	cancel  context.CancelFunc
	stderr  limitedBuffer
	waited  bool
}

func startNativeSQLiteSession(cmd *exec.Cmd, cancel context.CancelFunc) (*nativeSQLiteSession, error) {
	session := &nativeSQLiteSession{command: cmd, cancel: cancel}
	cmd.Stderr = &session.stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, errors.New("native SQLite tool cannot create a bounded session")
	}
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		_ = in.Close()
		cancel()
		return nil, errors.New("native SQLite session could not start the pinned tool")
	}
	session.in, session.out = in, bufio.NewScanner(out)
	return session, nil
}

func (session *nativeSQLiteSession) close() {
	_ = session.in.Close()
	session.cancel()
	if !session.waited {
		_ = session.command.Wait()
		session.waited = true
	}
}

func (session *nativeSQLiteSession) query(sql string) ([]string, error) {
	marker := "lidza_sqlite_" + newID()
	if _, err := io.WriteString(session.in, sql+"\n.print "+marker+"\n"); err != nil {
		return nil, errors.New("native SQLite command could not be delivered")
	}
	var rows []string
	for session.out.Scan() {
		line := session.out.Text()
		if line == marker {
			return rows, nil
		}
		if len(rows) >= 128 || len(line) > 4096 {
			return nil, errors.New("native SQLite response exceeds its budget")
		}
		rows = append(rows, line)
	}
	if !session.waited {
		_ = session.command.Wait()
		session.waited = true
	}
	return nil, errors.New("native SQLite response is incomplete, locked or oversized")
}

// nativeDatabaseAccount is the account that owns an application's database
// files: the account its release policy runs as, otherwise the native host's
// deployment user. Snapshots run as that account so they read exactly what
// the application can.
func (m *Manager) nativeDatabaseAccount(appID string) string {
	m.mu.Lock()
	created, isCreated := m.data.NativeCreated[appID]
	m.mu.Unlock()
	if isCreated && created.User != "" {
		return created.User
	}
	if m.cfg.Native != nil {
		if policy, ok := m.cfg.Native.Releases[appID]; ok && policy.User != "" {
			return policy.User
		}
	}
	return m.cfg.Native.User
}
