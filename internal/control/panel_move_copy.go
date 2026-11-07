package control

// What a control panel move copies: the data directory (encrypted state,
// framework credentials and master key, setup marker, boot seed), rewritten
// for the destination; the process environment the unit gave this panel;
// and the panel's own database as a pg_dump -Fc stream.

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/pkg/credentials"
)

type panelRewrite struct {
	Target    string
	TargetURL string
	Plan      PanelMovePlan
	Hostname  string
	At        time.Time
}

// panelRewriteState is the state the destination panel starts with: the
// destination is reached at its loopback agent address, servers follow the
// plan (a retired source leaves), and the move itself is replaced by an
// arrival note. Server IDs never change: applications, backups and moves
// refer to them.
func panelRewriteState(data saved, in panelRewrite) (saved, error) {
	if in.TargetURL == "" {
		return data, errors.New("the destination's agent address is unknown")
	}
	plan := map[string]PanelMoveServer{}
	for _, s := range in.Plan.Servers {
		plan[s.ID] = s
	}
	servers := []Server{}
	retired := []PanelMoveServer{}
	for _, s := range data.Servers {
		entry, planned := plan[s.ID]
		switch {
		case s.ID == in.Target:
			s.URL = in.TargetURL
		case planned && entry.Retire:
			for _, a := range data.Apps {
				if a.ServerID == s.ID {
					return data, fmt.Errorf("%s still holds application %s", s.ID, a.ID)
				}
			}
			retired = append(retired, PanelMoveServer{ID: s.ID, Name: entry.Name, Role: entry.Role, URL: s.URL, Retire: true, Detail: entry.Detail})
			continue
		case planned && entry.NewURL != "":
			s.URL = entry.NewURL
		}
		servers = append(servers, s)
	}
	if !slicesContainsServer(servers, in.Target) {
		return data, errors.New("the destination is not in the server list")
	}
	if err := validateServers(servers); err != nil {
		return data, fmt.Errorf("the new server list is invalid: %w", err)
	}
	data.Servers = servers
	targets := []UpdateTarget{}
	for _, t := range data.Updates.Targets {
		if slicesContainsServer(servers, t.ServerID) {
			t.Local = false
			targets = append(targets, t)
		}
	}
	data.Updates.Targets = targets
	data.PanelMove = nil
	data.PanelArrival = &PanelArrival{From: cmpOrString(in.Plan.SourceName, "the previous server"), At: in.At, Hostname: in.Hostname, Retired: retired}
	return data, nil
}

func slicesContainsServer(servers []Server, id string) bool {
	for _, s := range servers {
		if s.ID == id {
			return true
		}
	}
	return false
}

// panelSkip names data-directory entries that never move: the update
// cache (re-downloaded), the one-time setup credential, temporary files and
// a moved marker.
func panelSkip(rel string, dir bool) bool {
	base := filepath.Base(rel)
	switch {
	case rel == "updates" && dir:
		return true
	case rel == "setup-token", rel == agent.ControlMovedFile:
		return true
	case strings.HasPrefix(base, ".postgres-env-"), strings.HasPrefix(base, ".state-"), strings.HasPrefix(base, ".tmp"), strings.HasSuffix(base, ".tmp"):
		return true
	}
	return false
}

// panelDataSize counts what panelSnapshot sends.
func panelDataSize(dir string) (files int, size int64, err error) {
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		if panelSkip(filepath.ToSlash(rel), d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			files++
			size += info.Size()
		}
		return nil
	})
	return files, size, err
}

// panelSnapshot tars the data directory with the rewritten state sealed in
// place of control.enc.json (and of the servers.json boot seed).
func panelSnapshot(dir string, key []byte, state saved) ([]byte, int, error) {
	plain, err := json.Marshal(state)
	if err != nil {
		return nil, 0, err
	}
	sealed, err := credentials.Encrypt(key, plain)
	if err != nil {
		return nil, 0, err
	}
	encState, _ := json.MarshalIndent(sealed, "", "  ")
	seed, _ := json.MarshalIndent(state.Servers, "", "  ")
	replaced := map[string][]byte{"control.enc.json": encState}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	files := 0
	total := int64(0)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if panelSkip(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Name: rel + "/", Typeflag: tar.TypeDir, Mode: 0o700, ModTime: time.Now()})
		case !d.Type().IsRegular():
			return fmt.Errorf("%s is not a regular file", rel)
		}
		data, ok := replaced[rel]
		if !ok && rel == "servers.json" {
			data, ok = seed, true
		}
		if !ok {
			data, err = os.ReadFile(p)
			if err != nil {
				return err
			}
		}
		if total += int64(len(data)); total > 256<<20 {
			return errors.New("the data directory exceeds 256 MiB")
		}
		files++
		if err := tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	if _, err := os.Stat(filepath.Join(dir, "control.enc.json")); os.IsNotExist(err) {
		return nil, 0, errors.New("the panel's state file is missing")
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	return b.Bytes(), files, nil
}

// panelEnvPattern selects the unit environment the panel itself reads;
// anything the destination's host decides (address, data directory,
// standby, proxies) is set there instead.
var panelEnvPattern = regexp.MustCompile(`^(PUBLIC_URL|APP_URL|DATABASE_URL|CONTROL_[A-Z0-9_]+|AUTH_[A-Z0-9_]+|LIDZA_[A-Z0-9_]+|MAIL_[A-Z0-9_]+|SMTP_[A-Z0-9_]+|GITHUB_[A-Z0-9_]+)$`)

var panelEnvHost = map[string]bool{"LIDZA_ADDR": true, "CONTROL_DATA_DIR": true, agent.ControlStandbyEnv: true, "CONTROL_SETUP_ORIGIN": true, "LIDZA_TRUSTED_PROXIES": true}

func panelEnvironment(environ []string, dataDir string) map[string]string {
	out := map[string]string{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !panelEnvPattern.MatchString(k) || panelEnvHost[k] || strings.ContainsAny(v, "\x00\r\n") {
			continue
		}
		if k == "CONTROL_SERVERS_FILE" && dataDir != "" {
			if rel, err := filepath.Rel(dataDir, v); err == nil && !strings.HasPrefix(rel, "..") {
				v = filepath.Join(agent.ControlDataDir, rel)
			}
		}
		out[k] = v
	}
	return out
}

// panelDatabaseInfo reports whether the panel's database is its managed
// container, and its size.
func (c *Control) panelDatabaseInfo(ctx context.Context) (managed bool, size int64, err error) {
	if c.panelDatabase != nil {
		return c.panelDatabase(ctx)
	}
	saved, err := credentials.Read(c.cfg.DataDir)
	if err != nil {
		return false, 0, errors.New("the panel's credentials cannot be read")
	}
	if saved["SETUP_DB_PASSWORD"] == "" {
		return false, 0, nil
	}
	_, name := agent.ControlDatabaseContainer(c.cfg.DataDir)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "exec", name, "psql", "-U", "lidza", "-d", "lidza_control", "-Atc", "SELECT pg_database_size('lidza_control')").Output()
	if err != nil {
		return true, 0, errors.New("the panel's database container did not answer")
	}
	size, _ = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return true, size, nil
}

// panelDump writes the managed database as a custom-format archive.
func (c *Control) panelDump(ctx context.Context, w io.Writer) error {
	if c.panelDumpFixture != nil {
		return c.panelDumpFixture(ctx, w)
	}
	_, name := agent.ControlDatabaseContainer(c.cfg.DataDir)
	cmd := exec.CommandContext(ctx, "docker", "exec", name, "pg_dump", "-Fc", "-U", "lidza", "-d", "lidza_control")
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, moveBounded(stderr.String(), 300))
	}
	return nil
}
