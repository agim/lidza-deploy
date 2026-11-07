package agent

// Control panel hosting: a control panel moving to this server is installed
// here by the panel that is moving, through this agent (the panel already
// reaches every agent; no new port is opened). Everything privileged -
// installing the panel binary and its unit, writing its data directory,
// placing a certificate into Caddy's storage, editing a Caddyfile, stopping
// a unit - is a fixed operation of the root upgrade helper. The agent only
// stages inputs in <data dir>/control-move and queues the operation name;
// no path or command from a caller reaches the helper.
//
//   GET    /v1/host/control                    status of this host for a panel move
//   POST   /v1/host/control/bundle/manifest    stage the signed bundle of this agent's own version
//   PUT    /v1/host/control/bundle/{version}   its archive
//   PUT    /v1/host/control/files              the panel's data directory as a tar stream
//   POST   /v1/host/control/apply              install or update the panel (standby or active)
//   PUT    /v1/host/control/database           pg_dump -Fc stream restored into the panel's database
//   GET    /v1/host/control/certificate        a certificate from this host's Caddy storage
//   PUT    /v1/host/control/certificate        place a certificate into this host's Caddy storage
//   PUT    /v1/host/control/route              publish the panel hostname on this host's ingress
//   POST   /v1/host/control/probe              can this host reach another agent?
//   POST   /v1/host/control/retire             the old panel host: replace its site, stop its unit
//   DELETE /v1/host/control                    remove what a move installed here

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agim/lidza-deploy/internal/updates"
)

const (
	// ControlDataDir is where a moved control panel keeps its state; its
	// managed database container is named from it (ControlDatabaseContainer).
	ControlDataDir = "/var/lib/lidza-control"
	// ControlListen is the panel's loopback address. The upgrade helper
	// checks the panel's health there after a bundle upgrade.
	ControlListen = "127.0.0.1:3000"
	// ControlStandbyEnv starts the panel binary in standby: it serves a
	// standby page and runs nothing else (cmd/web).
	ControlStandbyEnv = "CONTROL_STANDBY"
	// ControlMovedFile in a panel's data directory marks a panel that moved
	// away; it serves only a notice naming the new location.
	ControlMovedFile = "control-moved.json"

	controlHelperMarker   = "lidza-control-move-v1"
	controlRouteID        = "lidza-control-panel"
	controlActionApply    = "control-apply"
	controlActionRemove   = "control-remove"
	controlActionCert     = "control-certificate"
	controlActionCaddy    = "control-caddyfile"
	controlActionStop     = "control-stop"
	controlMaxFiles       = 512 << 20
	controlCaddyfile      = "/etc/caddy/Caddyfile"
	controlCaddyfileBegin = "# lidza-control-panel begin"
	controlCaddyfileEnd   = "# lidza-control-panel end"
)

// Replaced in tests.
var (
	controlHelperPath = "/usr/local/libexec/lidza-agent-upgrade"
	controlUnitPath   = "/etc/systemd/system/lidza-control.service"
	controlMarkerPath = "/etc/lidza-control/.moved-in"
	controlMachineID  = func() string { b, _ := os.ReadFile("/etc/machine-id"); return strings.TrimSpace(string(b)) }
	controlListenFree = func(addr string) bool {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return false
		}
		l.Close()
		return true
	}
	controlHelperWait  = 4 * time.Minute
	controlHelperPoll  = time.Second
	controlCaddyfileAt = controlCaddyfile
	controlDocker      = func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Stdin = stdin
		cmd.WaitDelay = 5 * time.Second
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return out, fmt.Errorf("%w: %s", err, boundedErrorText(strings.TrimSpace(stderr.String()), 300))
		}
		return out, nil
	}
	// caddyProcessEnv is the environment of the running Caddy process
	// (for HOME and XDG_DATA_HOME); nil when none runs.
	caddyProcessEnv = func() map[string]string {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return nil
		}
		for _, entry := range entries {
			if _, err := strconv.Atoi(entry.Name()); err != nil {
				continue
			}
			comm, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
			if err != nil || strings.TrimSpace(string(comm)) != "caddy" {
				continue
			}
			raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
			if err != nil {
				continue
			}
			out := map[string]string{}
			for _, kv := range bytes.Split(raw, []byte{0}) {
				if k, v, ok := strings.Cut(string(kv), "="); ok {
					out[k] = v
				}
			}
			return out
		}
		return nil
	}
	caddyUserHome = func(name string) string {
		if u, err := user.Lookup(name); err == nil {
			return u.HomeDir
		}
		return ""
	}
)

// ControlDatabaseContainer names a control panel's managed PostgreSQL
// container (and volume) from its data directory, as first-run setup creates
// it; id is the container's io.lidza.control.database label.
func ControlDatabaseContainer(dir string) (id, name string) {
	sum := sha256.Sum256([]byte(dir))
	id = hex.EncodeToString(sum[:6])
	return id, "lidza-control-db-" + id
}

// ControlHostState is what this agent remembers about a panel it installed.
// It holds no secrets and lives beside the staged inputs.
type ControlHostState struct {
	Hostname string `json:"hostname"`
	Listen   string `json:"listen"`
	// Phase is standby or active once installed.
	Phase string `json:"phase,omitempty"`
	// Route is how the hostname is published: agent (this agent's proxy),
	// caddy (admin API route) or caddyfile (a marked site block).
	Route       string    `json:"route,omitempty"`
	TLSSubject  bool      `json:"tls_subject,omitempty"`
	Certificate string    `json:"certificate,omitempty"` // issuer directory of a placed certificate
	Updated     time.Time `json:"updated"`
}

// ControlHelperStatus is the root helper's report of its last control
// operation.
type ControlHelperStatus struct {
	Action  string `json:"action,omitempty"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

// ControlHostStatus answers GET /v1/host/control.
type ControlHostStatus struct {
	// Supported: the root helper knows the control operations.
	Supported bool   `json:"supported"`
	Detail    string `json:"detail,omitempty"`
	Version   string `json:"version"`
	Arch      string `json:"arch"`
	MachineID string `json:"machine_id,omitempty"`
	// AgentURL is how a panel on this host reaches this agent.
	AgentURL string `json:"agent_url"`
	// Listen is the panel's loopback address; ListenFree reports nothing
	// else listens there (or the moved-in panel does).
	Listen     string `json:"listen"`
	ListenFree bool   `json:"listen_free"`
	// Installed: a control panel unit exists; Owned: a move installed it.
	Installed    bool                 `json:"installed"`
	Owned        bool                 `json:"owned"`
	State        *ControlHostState    `json:"state,omitempty"`
	Helper       *ControlHelperStatus `json:"helper,omitempty"`
	BundleStaged bool                 `json:"bundle_staged"`
	// Ingress is how the hostname would be published here and why.
	Ingress       string `json:"ingress"`
	IngressDetail string `json:"ingress_detail,omitempty"`
	// CertificateStorage is the Caddy storage certificates are copied
	// into; empty when it cannot be found.
	CertificateStorage string `json:"certificate_storage,omitempty"`
	Reserved           bool   `json:"reserved,omitempty"`
	Docker             bool   `json:"docker"`
}

type controlHost struct {
	mu sync.Mutex // serializes control operations
}

var controlHostOps controlHost

func (m *Manager) controlDir() string { return filepath.Join(m.cfg.DataDir, "control-move") }

func (m *Manager) controlState() *ControlHostState {
	var s ControlHostState
	raw, err := os.ReadFile(filepath.Join(m.controlDir(), "state.json"))
	if err != nil || json.Unmarshal(raw, &s) != nil || s.Hostname == "" {
		return nil
	}
	return &s
}

func (m *Manager) saveControlState(s *ControlHostState) error {
	if err := os.MkdirAll(m.controlDir(), 0o700); err != nil {
		return err
	}
	if s == nil {
		err := os.Remove(filepath.Join(m.controlDir(), "state.json"))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	s.Updated = time.Now().UTC()
	raw, _ := json.MarshalIndent(s, "", "  ")
	return writeFileAtomic(filepath.Join(m.controlDir(), "state.json"), raw, 0o600)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(f.Name(), mode)
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	return err
}

func controlHelperSupported() bool {
	raw, err := os.ReadFile(controlHelperPath)
	return err == nil && bytes.Contains(raw, []byte(controlHelperMarker))
}

func (m *Manager) controlHelperStatus() *ControlHelperStatus {
	var s ControlHelperStatus
	raw, err := os.ReadFile(filepath.Join(m.controlDir(), "status.json"))
	if err != nil || json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return &s
}

// controlHelper queues one fixed helper operation and waits for its report.
func (m *Manager) controlHelper(ctx context.Context, action string) error {
	if !controlHelperSupported() {
		return errors.New("this server's upgrade helper predates control panel moves; update the server first")
	}
	dir := filepath.Join(m.cfg.DataDir, "upgrade")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(m.controlDir(), 0o700); err != nil {
		return err
	}
	request := filepath.Join(dir, "request")
	deadline := time.Now().Add(controlHelperWait)
	for {
		if _, err := os.Stat(request); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("another host operation is queued; retry when it finishes")
		}
		if err := sleepCtx(ctx, controlHelperPoll); err != nil {
			return err
		}
	}
	status := filepath.Join(m.controlDir(), "status.json")
	pending, _ := json.Marshal(ControlHelperStatus{Action: action, State: "queued"})
	if err := writeFileAtomic(status, pending, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(request, []byte(action+"\n"), 0o600); err != nil {
		return err
	}
	for {
		if s := m.controlHelperStatus(); s != nil && s.Action == action && (s.State == "succeeded" || s.State == "failed") {
			if s.State == "failed" {
				return errors.New(cmpOr(s.Message, "the host operation failed"))
			}
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("the host operation did not report back in time; check the upgrade helper's journal")
		}
		if err := sleepCtx(ctx, controlHelperPoll); err != nil {
			return err
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// controlStagedBundle is the verified staged bundle of this agent's own
// version, or an error naming what is missing.
func (m *Manager) controlStagedBundle() (string, error) {
	dir, err := updates.StagedDir(m.upgradeDir(), upgradeInstalled())
	if err != nil {
		return "", errors.New("this agent's version is not a signed release; install a signed bundle first")
	}
	if _, err := updates.ReadStaged(dir, runtime.GOARCH, upgradeTrust()); err != nil {
		return "", fmt.Errorf("no verified bundle of %s is staged here: %w", upgradeInstalled(), err)
	}
	return filepath.Join(dir, updates.StagedArchive), nil
}

// stageControlManifest stages the signed manifest of this agent's own
// version, which the bundle upgrade path refuses (it requires newer).
func (m *Manager) stageControlManifest(in upgradeManifestRequest) error {
	raw, err := json.Marshal(in.Signature)
	if err != nil {
		return err
	}
	manifest, _, err := upgradeTrust().Verify([]byte(in.Manifest), raw)
	if err != nil {
		return err
	}
	if manifest.Version != upgradeInstalled() {
		return fmt.Errorf("bundle %s is not this agent's version %s; update the server first", manifest.Version, upgradeInstalled())
	}
	if _, err := manifest.Asset(runtime.GOARCH); err != nil {
		return err
	}
	dir, err := updates.StagedDir(m.upgradeDir(), manifest.Version)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, updates.StagedManifest), []byte(in.Manifest), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, updates.StagedSignature), raw, 0o600)
}

// extractControlBinary copies bin/lidza-control and deploy/lidza-control.service
// out of a verified bundle archive into the stage directory.
func extractControlBinary(archive, stage string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return errors.New("bundle archive is not gzip")
	}
	defer gz.Close()
	wanted := map[string]string{"bin/lidza-control": filepath.Join(stage, "bin", "lidza-control"), "deploy/lidza-control.service": filepath.Join(stage, "lidza-control.service")}
	found := map[string]bool{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("bundle archive is unreadable")
		}
		name := strings.TrimPrefix(path.Clean(h.Name), "./")
		target, ok := wanted[name]
		if !ok || h.Typeflag != tar.TypeReg {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if strings.HasPrefix(name, "bin/") {
			mode = 0o700
		}
		out, err := os.OpenFile(target+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, io.LimitReader(tr, 256<<20))
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(target+".tmp", target)
		}
		if err != nil {
			return err
		}
		found[name] = true
	}
	if len(found) != len(wanted) {
		return errors.New("bundle archive lacks the control panel binary or its unit")
	}
	return nil
}

var controlEnvName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// renderControlEnv is the panel's systemd environment file. Values are
// double-quoted with backslash escapes; names the host decides are set here.
func renderControlEnv(values map[string]string, standby bool) ([]byte, error) {
	out := map[string]string{}
	for k, v := range values {
		if !controlEnvName.MatchString(k) || strings.ContainsAny(v, "\x00\r\n") || len(v) > 8192 {
			return nil, fmt.Errorf("environment value %s is not allowed", k)
		}
		out[k] = v
	}
	if len(out) > 200 {
		return nil, errors.New("too many environment values")
	}
	delete(out, ControlStandbyEnv)
	out["LIDZA_ADDR"] = ControlListen
	out["CONTROL_DATA_DIR"] = ControlDataDir
	if standby {
		out[ControlStandbyEnv] = "1"
	}
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteString("# Written by a control panel move (lidza-control-move-v1).\n")
	for _, k := range keys {
		v := strings.ReplaceAll(out[k], `\`, `\\`)
		v = strings.ReplaceAll(v, `"`, `\"`)
		fmt.Fprintf(&b, "%s=\"%s\"\n", k, v)
	}
	return b.Bytes(), nil
}

// receiveControlFiles unpacks the panel's data directory (a tar stream of
// regular files and directories with relative names) into the stage.
func receiveControlFiles(body io.Reader, stage string) (int, error) {
	next := filepath.Join(stage, "data.next")
	if err := os.RemoveAll(next); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(next, 0o700); err != nil {
		return 0, err
	}
	fail := func(err error) (int, error) { os.RemoveAll(next); return 0, err }
	tr := tar.NewReader(body)
	count := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(errors.New("the data stream is not a valid tar archive"))
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00") {
			return fail(errors.New("the data stream names a path outside the data directory"))
		}
		target := filepath.Join(next, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fail(err)
			}
		case tar.TypeReg:
			if count++; count > 4096 {
				return fail(errors.New("the data stream has too many files"))
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return fail(err)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return fail(errors.New("the data stream repeats a file"))
			}
			_, err = io.Copy(out, io.LimitReader(tr, h.Size))
			if closeErr := out.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return fail(err)
			}
		default:
			return fail(errors.New("the data stream may hold only files and directories"))
		}
	}
	data := filepath.Join(stage, "data")
	if err := os.RemoveAll(data); err != nil {
		return 0, err
	}
	return count, os.Rename(next, data)
}

// ---- Caddy storage ---------------------------------------------------------------

// caddyStorageRoot finds the file storage of the running Caddy: the admin
// API's storage module root, else $XDG_DATA_HOME/caddy, else
// $HOME/.local/share/caddy of the process or of its account. Another
// storage module (a database, S3) has no files to copy: "".
func caddyStorageRoot(adminStorage map[string]any, env map[string]string, account string) string {
	if adminStorage != nil {
		module, _ := adminStorage["module"].(string)
		if root, _ := adminStorage["root"].(string); module == "file_system" && filepath.IsAbs(root) {
			return filepath.Clean(root)
		}
		if module != "" && module != "file_system" {
			return ""
		}
	}
	if v := env["XDG_DATA_HOME"]; filepath.IsAbs(v) {
		return filepath.Join(v, "caddy")
	}
	home := env["HOME"]
	if !filepath.IsAbs(home) && account != "" {
		home = caddyUserHome(account)
	}
	if !filepath.IsAbs(home) {
		return ""
	}
	return filepath.Join(home, ".local", "share", "caddy")
}

func (m *Manager) caddyStorage(ctx context.Context) string {
	var storage map[string]any
	if raw, err := hostIngressAdminGet(ctx, hostIngressDefaultAdmin); err == nil {
		var cfg struct {
			Storage map[string]any `json:"storage"`
		}
		if json.Unmarshal(raw, &cfg) == nil {
			storage = cfg.Storage
		}
	}
	root := caddyStorageRoot(storage, caddyProcessEnv(), caddyProcessUser())
	if root == "" {
		return ""
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return ""
	}
	return root
}

var controlHostnamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
var controlIssuerPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)

// ControlCertificate is one certificate in Caddy's storage layout:
// certificates/<issuer>/<name>/<name>.{crt,key,json}.
type ControlCertificate struct {
	Host   string `json:"host"`
	Issuer string `json:"issuer"`
	Name   string `json:"name"` // the storage name: the host or wildcard_.<parent>
	Cert   string `json:"crt"`  // base64
	Key    string `json:"key"`  // base64
	Meta   string `json:"json,omitempty"`
}

// findCaddyCertificate reads the certificate covering host from a Caddy
// storage root: an exact one first, else a wildcard of its parent.
func findCaddyCertificate(root, host string) (*ControlCertificate, error) {
	if !controlHostnamePattern.MatchString(host) {
		return nil, errors.New("invalid hostname")
	}
	names := []string{host}
	if _, parent, ok := strings.Cut(host, "."); ok && strings.Contains(parent, ".") {
		names = append(names, "wildcard_."+parent)
	}
	issuers, err := os.ReadDir(filepath.Join(root, "certificates"))
	if err != nil {
		return nil, nil
	}
	for _, name := range names {
		for _, issuer := range issuers {
			if !issuer.IsDir() || !controlIssuerPattern.MatchString(issuer.Name()) {
				continue
			}
			dir := filepath.Join(root, "certificates", issuer.Name(), name)
			crt, err1 := readRegular(filepath.Join(dir, name+".crt"), 1<<20)
			key, err2 := readRegular(filepath.Join(dir, name+".key"), 1<<20)
			if err1 != nil || err2 != nil {
				continue
			}
			meta, _ := readRegular(filepath.Join(dir, name+".json"), 1<<20)
			return &ControlCertificate{Host: host, Issuer: issuer.Name(), Name: name, Cert: base64.StdEncoding.EncodeToString(crt), Key: base64.StdEncoding.EncodeToString(key), Meta: base64.StdEncoding.EncodeToString(meta)}, nil
		}
	}
	return nil, nil
}

func readRegular(p string, limit int64) ([]byte, error) {
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("not a small regular file")
	}
	return os.ReadFile(p)
}

// stageControlCertificate writes a certificate into the stage in Caddy's
// layout, with the storage root the helper copies it to.
func stageControlCertificate(stage, storage string, cert ControlCertificate) error {
	_, parent, _ := strings.Cut(cert.Host, ".")
	if !controlIssuerPattern.MatchString(cert.Issuer) || !controlHostnamePattern.MatchString(cert.Host) || (cert.Name != cert.Host && cert.Name != "wildcard_."+parent) {
		return errors.New("certificate names are invalid")
	}
	crt, err1 := base64.StdEncoding.DecodeString(cert.Cert)
	key, err2 := base64.StdEncoding.DecodeString(cert.Key)
	meta, err3 := base64.StdEncoding.DecodeString(cert.Meta)
	if err1 != nil || err2 != nil || err3 != nil || !bytes.Contains(crt, []byte("BEGIN CERTIFICATE")) || !bytes.Contains(key, []byte("PRIVATE KEY")) {
		return errors.New("certificate or key is not PEM")
	}
	dir := filepath.Join(stage, "cert")
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	leaf := filepath.Join(dir, cert.Issuer, cert.Name)
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		return err
	}
	files := map[string][]byte{cert.Name + ".crt": crt, cert.Name + ".key": key}
	if len(meta) > 0 {
		files[cert.Name+".json"] = meta
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(leaf, name), data, 0o600); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(stage, "cert-storage"), []byte(storage+"\n"), 0o600)
}

// ---- ingress ----------------------------------------------------------------------

// renderControlRoute is the admin-API route for the panel hostname.
func renderControlRoute(host, upstream string) []byte {
	return mustJSON(map[string]any{
		"@id":   controlRouteID,
		"match": []any{map[string]any{"host": []string{host}}},
		"handle": []any{map[string]any{"handler": "subroute", "routes": []any{
			map[string]any{"match": []any{map[string]any{"path": []string{"/metrics", "/readyz", "/healthz", "/tls-allow", "/tls-allow/*"}}}, "handle": []any{map[string]any{"handler": "static_response", "status_code": 404}}},
			map[string]any{"handle": []any{map[string]any{"handler": "reverse_proxy", "upstreams": []any{map[string]any{"dial": upstream}}}}},
		}}},
		"terminal": true,
	})
}

func (c nativeCaddy) publishControlRoute(ctx context.Context, host, upstream string) error {
	route := renderControlRoute(host, upstream)
	var current json.RawMessage
	if found, err := c.get(ctx, "/id/"+controlRouteID, &current); err != nil {
		return err
	} else if found && jsonEqual(current, route) {
		return nil
	}
	status, err := c.call(ctx, http.MethodPatch, "/id/"+controlRouteID, route)
	if err != nil || status != http.StatusNotFound {
		return err
	}
	_, err = c.call(ctx, http.MethodPut, "/config/apps/http/servers/"+c.cfg.Server+"/routes/0", route)
	return err
}

// controlCaddyfileSite returns content with one marked site block for host
// (added, or replacing an earlier marked block or an unmarked top-level
// block whose address is exactly host). An empty body removes it.
func controlCaddyfileSite(content, host, body string) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	out := []string{}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == controlCaddyfileBegin {
			for i < len(lines) && strings.TrimSpace(lines[i]) != controlCaddyfileEnd {
				i++
			}
			continue
		}
		trimmed := strings.TrimSpace(stripCaddyComment(line))
		addr := strings.TrimSpace(strings.TrimSuffix(trimmed, "{"))
		topLevel := !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t")
		if topLevel && strings.HasSuffix(trimmed, "{") && (addr == host || addr == "https://"+host || addr == host+":443") {
			depth := 0
			for ; i < len(lines); i++ {
				clean := stripCaddyComment(lines[i])
				depth += strings.Count(clean, "{") - strings.Count(clean, "}")
				if depth <= 0 {
					break
				}
			}
			continue
		}
		out = append(out, line)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	text := strings.Join(out, "\n") + "\n"
	if body == "" {
		return text
	}
	return text + "\n" + controlCaddyfileBegin + "\n" + host + " {\n" + body + "}\n" + controlCaddyfileEnd + "\n"
}

func stripCaddyComment(line string) string {
	if i := strings.Index(line, "#"); i >= 0 {
		return line[:i]
	}
	return line
}

func controlProxyBody(upstream string) string {
	return "\t@internal path /metrics /readyz /healthz /tls-allow /tls-allow/*\n\trespond @internal 404\n\treverse_proxy " + upstream + "\n"
}

// controlIngress decides how the panel hostname is published here.
func (m *Manager) controlIngress(ctx context.Context) (mode, detail string) {
	status := m.hostIngress(ctx, false)
	if status.Mode == HostIngressCaddy {
		return "caddy", "a route through the Caddy admin API, with the hostname in its TLS automation so Caddy renews the certificate"
	}
	if !status.AdminAPI {
		if raw, err := os.ReadFile(controlCaddyfileAt); err == nil && !bytes.Contains(raw, []byte(m.cfg.ProxyListen)) {
			return "caddyfile", "a site block in " + controlCaddyfileAt + ", validated with caddy validate before Caddy reloads"
		}
	}
	return "agent", "this agent's proxy, behind the web server's on-demand certificates"
}

// publishControlRoute publishes the hostname; tls adds it to the TLS
// automation of an admin-API Caddy (which issues at once unless a copied
// certificate is already in its storage).
func (m *Manager) publishControlRoute(ctx context.Context, host string, tls bool) (string, error) {
	mode, _ := m.controlIngress(ctx)
	switch mode {
	case "caddy":
		caddy, ok := m.nativeCaddy(ctx)
		if !ok {
			return mode, errors.New("the Caddy admin API is unavailable")
		}
		if err := caddy.publishControlRoute(ctx, host, ControlListen); err != nil {
			return mode, err
		}
		if tls {
			if _, err := caddy.ensureTLSSubjects(ctx, []string{host}); err != nil {
				return mode, fmt.Errorf("tls automation: %w", err)
			}
		}
	case "caddyfile":
		raw, err := os.ReadFile(controlCaddyfileAt)
		if err != nil {
			return mode, err
		}
		next := controlCaddyfileSite(string(raw), host, controlProxyBody(ControlListen))
		if next != string(raw) {
			if err := writeFileAtomic(filepath.Join(m.controlDir(), "Caddyfile.next"), []byte(next), 0o600); err != nil {
				return mode, err
			}
			if err := m.controlHelper(ctx, controlActionCaddy); err != nil {
				return mode, err
			}
		}
	}
	return mode, nil
}

// controlRouteLoop keeps an admin-API route in place: a Caddy reloaded
// from its own configuration file loses routes added through the API.
func (m *Manager) controlRouteLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
		}
		s := m.controlState()
		if s == nil || s.Route != "caddy" || s.Phase == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
		_, _ = m.publishControlRoute(ctx, s.Hostname, s.TLSSubject)
		cancel()
	}
}

// serveControlPanel forwards the moved-in panel's hostname through this
// agent's proxy, for hosts whose web server forwards every hostname here.
func (m *Manager) serveControlPanel(w http.ResponseWriter, r *http.Request, host string) bool {
	s := m.controlState()
	if s == nil || s.Phase == "" || s.Route != "agent" || !strings.EqualFold(s.Hostname, host) {
		return false
	}
	if operatorPath(r.URL.Path) || path.Clean(r.URL.Path) == "/healthz" {
		http.NotFound(w, r)
		return true
	}
	target := &url.URL{Scheme: "http", Host: cmpOr(s.Listen, ControlListen)}
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(req *http.Request) { director(req); req.Host = r.Host }
	proxy.ServeHTTP(w, r)
	return true
}

// controlDomainAllowed lets on-demand TLS issue for the moved-in panel.
func (m *Manager) controlDomainAllowed(domain string) bool {
	s := m.controlState()
	return s != nil && s.Phase != "" && strings.EqualFold(s.Hostname, domain)
}

// ---- database ----------------------------------------------------------------------

// restoreControlDatabase replaces the panel's database with a pg_dump -Fc
// stream. The standby panel created the container on its first start.
func restoreControlDatabase(ctx context.Context, body io.Reader) error {
	id, name := ControlDatabaseContainer(ControlDataDir)
	var label string
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, err := controlDocker(ctx, nil, "inspect", "--format", `{{index .Config.Labels "io.lidza.control.database"}}`, name)
		label = strings.TrimSpace(string(out))
		if err == nil && label == id {
			if _, err := controlDocker(ctx, nil, "exec", name, "pg_isready", "-h", "127.0.0.1", "-U", "lidza", "-d", "postgres"); err == nil {
				break
			}
		} else if err == nil && label != id {
			return errors.New("the database container belongs to another installation")
		}
		if time.Now().After(deadline) {
			return errors.New("the standby panel's database container is not ready; check journalctl -u lidza-control")
		}
		if err := sleepCtx(ctx, 2*time.Second); err != nil {
			return err
		}
	}
	reset := "DROP DATABASE IF EXISTS lidza_control WITH (FORCE);\nCREATE DATABASE lidza_control OWNER lidza;\n"
	if _, err := controlDocker(ctx, strings.NewReader(reset), "exec", "-i", name, "psql", "-U", "lidza", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-q"); err != nil {
		return fmt.Errorf("could not reset the panel database: %w", err)
	}
	if _, err := controlDocker(ctx, body, "exec", "-i", name, "pg_restore", "-U", "lidza", "-d", "lidza_control", "--no-owner", "--single-transaction", "--exit-on-error"); err != nil {
		return fmt.Errorf("pg_restore failed: %w", err)
	}
	return nil
}

// ---- probe -------------------------------------------------------------------------

// probeAgent reports whether another agent answers at url with token.
func probeAgent(ctx context.Context, raw, token string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"))) || len(token) < 32 {
		return errors.New("an agent URL must be https (or loopback http) with a token")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(raw, "/")+"/v1/move/host", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return errors.New("no answer from " + u.Host)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("%s answered HTTP %d", u.Host, res.StatusCode)
	}
	return nil
}

// ---- routes ------------------------------------------------------------------------

// controlAgentURL is how a panel on this host reaches this agent: its
// listener, on loopback when it listens on every address.
func controlAgentURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func (m *Manager) controlHostStatus(ctx context.Context, host string) ControlHostStatus {
	s := ControlHostStatus{Supported: controlHelperSupported(), Version: upgradeInstalled(), Arch: runtime.GOARCH, MachineID: controlMachineID(), AgentURL: controlAgentURL(m.cfg.Listen), Listen: ControlListen, State: m.controlState(), Helper: m.controlHelperStatus()}
	if !s.Supported {
		s.Detail = "This server's upgrade helper predates control panel moves; update the server first."
	}
	if _, err := os.Stat(controlUnitPath); err == nil {
		s.Installed = true
	}
	if _, err := os.Stat(controlMarkerPath); err == nil {
		s.Owned = true
	}
	s.ListenFree = controlListenFree(ControlListen) || (s.Owned && s.Installed)
	_, err := m.controlStagedBundle()
	s.BundleStaged = err == nil
	s.Ingress, s.IngressDetail = m.controlIngress(ctx)
	s.CertificateStorage = m.caddyStorage(ctx)
	if host != "" {
		s.Reserved = m.hostReserved(strings.ToLower(host))
	}
	_, derr := exec.LookPath("docker")
	s.Docker = derr == nil
	return s
}

func (m *Manager) controlRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/host/control", func(w http.ResponseWriter, r *http.Request) {
		JSON(w, 200, m.controlHostStatus(r.Context(), r.URL.Query().Get("host")))
	})
	mux.HandleFunc("POST /v1/host/control/bundle/manifest", func(w http.ResponseWriter, r *http.Request) {
		var in upgradeManifestRequest
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.stageControlManifest(in); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]any{"version": upgradeInstalled(), "arch": runtime.GOARCH})
	})
	mux.HandleFunc("PUT /v1/host/control/bundle/{version}", func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(20 * time.Minute))
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(20 * time.Minute))
		if r.PathValue("version") != upgradeInstalled() {
			Fail(w, 409, errors.New("the bundle is not this agent's version"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, updates.MaxArchiveSize+1)
		if err := m.receiveUpgradeArchive(r.PathValue("version"), r.Body); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]any{"version": upgradeInstalled(), "verified": true})
	})
	mux.HandleFunc("PUT /v1/host/control/files", func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(20 * time.Minute))
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(20 * time.Minute))
		controlHostOps.mu.Lock()
		defer controlHostOps.mu.Unlock()
		if err := os.MkdirAll(m.controlDir(), 0o700); err != nil {
			Fail(w, 500, err)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, controlMaxFiles)
		count, err := receiveControlFiles(r.Body, m.controlDir())
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]any{"files": count})
	})
	mux.HandleFunc("POST /v1/host/control/apply", func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(controlHelperWait + time.Minute))
		var in struct {
			Hostname string            `json:"hostname"`
			Standby  bool              `json:"standby"`
			Env      map[string]string `json:"env"`
		}
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		controlHostOps.mu.Lock()
		defer controlHostOps.mu.Unlock()
		if err := m.applyControl(r.Context(), in.Hostname, in.Standby, in.Env); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, m.controlHostStatus(r.Context(), in.Hostname))
	})
	mux.HandleFunc("PUT /v1/host/control/database", func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(60 * time.Minute))
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(60 * time.Minute))
		controlHostOps.mu.Lock()
		defer controlHostOps.mu.Unlock()
		if s := m.controlState(); s == nil || s.Phase == "" {
			Fail(w, 409, errors.New("install the standby control panel first"))
			return
		}
		m.moveTransfers.Add(1)
		defer m.moveTransfers.Add(-1)
		if err := restoreControlDatabase(r.Context(), r.Body); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "restored"})
	})
	mux.HandleFunc("GET /v1/host/control/certificate", func(w http.ResponseWriter, r *http.Request) {
		root := m.caddyStorage(r.Context())
		if root == "" {
			Fail(w, 404, errors.New("this server's Caddy storage was not found"))
			return
		}
		cert, err := findCaddyCertificate(root, strings.ToLower(r.URL.Query().Get("host")))
		if err != nil {
			Fail(w, 400, err)
			return
		}
		if cert == nil {
			Fail(w, 404, errors.New("no certificate for this hostname in Caddy's storage"))
			return
		}
		if r.URL.Query().Get("check") == "1" {
			// Planning only asks whether one exists; the key stays here.
			cert.Cert, cert.Key, cert.Meta = "", "", ""
		}
		w.Header().Set("Cache-Control", "no-store")
		JSON(w, 200, cert)
	})
	mux.HandleFunc("PUT /v1/host/control/certificate", func(w http.ResponseWriter, r *http.Request) {
		var in ControlCertificate
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		controlHostOps.mu.Lock()
		defer controlHostOps.mu.Unlock()
		s := m.controlState()
		if s == nil || !strings.EqualFold(s.Hostname, in.Host) {
			Fail(w, 409, errors.New("the certificate is not for the control panel installed here"))
			return
		}
		root := m.caddyStorage(r.Context())
		if root == "" {
			Fail(w, 409, errors.New("this server's Caddy storage was not found; Caddy will issue a certificate once DNS points here"))
			return
		}
		if err := stageControlCertificate(m.controlDir(), root, in); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := m.controlHelper(r.Context(), controlActionCert); err != nil {
			Fail(w, 409, err)
			return
		}
		s.Certificate = in.Issuer
		_ = m.saveControlState(s)
		JSON(w, 200, map[string]string{"storage": root, "issuer": in.Issuer})
	})
	mux.HandleFunc("PUT /v1/host/control/route", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Hostname string `json:"hostname"`
			TLS      bool   `json:"tls"`
		}
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		controlHostOps.mu.Lock()
		defer controlHostOps.mu.Unlock()
		s := m.controlState()
		if s == nil || !strings.EqualFold(s.Hostname, in.Hostname) {
			Fail(w, 409, errors.New("install the control panel here first"))
			return
		}
		mode, err := m.publishControlRoute(r.Context(), s.Hostname, in.TLS)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		s.Route, s.TLSSubject = mode, s.TLSSubject || in.TLS
		_ = m.saveControlState(s)
		_, detail := m.controlIngress(r.Context())
		JSON(w, 200, map[string]string{"mode": mode, "detail": detail})
	})
	mux.HandleFunc("POST /v1/host/control/probe", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			URL   string `json:"url"`
			Token string `json:"token"`
		}
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		if err := probeAgent(r.Context(), in.URL, in.Token); err != nil {
			JSON(w, 200, map[string]any{"reachable": false, "detail": err.Error()})
			return
		}
		JSON(w, 200, map[string]any{"reachable": true})
	})
	mux.HandleFunc("POST /v1/host/control/retire", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Hostname string `json:"hostname"`
			MovedTo  string `json:"moved_to"`
		}
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		controlHostOps.mu.Lock()
		defer controlHostOps.mu.Unlock()
		notes, err := m.retireControl(r.Context(), strings.ToLower(in.Hostname))
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]any{"notes": notes})
	})
	mux.HandleFunc("DELETE /v1/host/control", func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(controlHelperWait + time.Minute))
		controlHostOps.mu.Lock()
		defer controlHostOps.mu.Unlock()
		if err := m.removeControl(r.Context()); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "removed"})
	})
}

// applyControl installs or updates the panel: binary and unit from this
// agent's own verified bundle, the environment, and staged data if any.
func (m *Manager) applyControl(ctx context.Context, host string, standby bool, env map[string]string) error {
	host = strings.ToLower(strings.TrimSpace(host))
	if !controlHostnamePattern.MatchString(host) {
		return errors.New("the control panel hostname is invalid")
	}
	if m.hostReserved(host) {
		return errors.New("the control panel hostname is reserved on this server")
	}
	if _, err := os.Stat(controlUnitPath); err == nil {
		if _, err := os.Stat(controlMarkerPath); err != nil {
			return errors.New("a control panel that no move installed already runs here")
		}
	} else if !controlListenFree(ControlListen) {
		return fmt.Errorf("%s is already in use on this server", ControlListen)
	}
	archive, err := m.controlStagedBundle()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.controlDir(), 0o700); err != nil {
		return err
	}
	if err := extractControlBinary(archive, m.controlDir()); err != nil {
		return err
	}
	rendered, err := renderControlEnv(env, standby)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(m.controlDir(), "control.env"), rendered, 0o600); err != nil {
		return err
	}
	s := m.controlState()
	if s == nil || s.Hostname != host {
		s = &ControlHostState{Hostname: host, Listen: ControlListen}
	}
	if err := m.saveControlState(s); err != nil {
		return err
	}
	if err := m.controlHelper(ctx, controlActionApply); err != nil {
		return err
	}
	s.Phase = "active"
	if standby {
		s.Phase = "standby"
	}
	return m.saveControlState(s)
}

func (m *Manager) removeControl(ctx context.Context) error {
	s := m.controlState()
	if s != nil && s.Route == "caddy" {
		if caddy, ok := m.nativeCaddy(ctx); ok {
			if _, err := caddy.call(ctx, http.MethodDelete, "/id/"+controlRouteID, nil); err != nil {
				return err
			}
			if s.TLSSubject {
				if err := caddy.removeTLSSubjects(ctx, []string{s.Hostname}); err != nil {
					return err
				}
			}
		}
	}
	if s != nil && s.Route == "caddyfile" {
		if raw, err := os.ReadFile(controlCaddyfileAt); err == nil {
			if next := controlCaddyfileSite(string(raw), s.Hostname, ""); next != string(raw) {
				if err := writeFileAtomic(filepath.Join(m.controlDir(), "Caddyfile.next"), []byte(next), 0o600); err != nil {
					return err
				}
				if err := m.controlHelper(ctx, controlActionCaddy); err != nil {
					return err
				}
			}
		}
	}
	if _, err := os.Stat(controlMarkerPath); err == nil || (s != nil && s.Phase != "") {
		if err := m.controlHelper(ctx, controlActionRemove); err != nil {
			return err
		}
	}
	return os.RemoveAll(m.controlDir())
}

// retireControl runs on the old panel host once DNS points to the new one:
// a Caddyfile site for the hostname is replaced by a notice, an admin-API
// route is withdrawn, and the panel unit is stopped and disabled.
func (m *Manager) retireControl(ctx context.Context, host string) ([]string, error) {
	if !controlHostnamePattern.MatchString(host) {
		return nil, errors.New("invalid hostname")
	}
	if _, err := os.Stat(filepath.Join(ControlDataDir, ControlMovedFile)); err != nil {
		return nil, errors.New("the control panel on this server has not handed over; it must write its moved marker first")
	}
	notes := []string{}
	if raw, err := os.ReadFile(controlCaddyfileAt); err == nil {
		notice := "\trespond \"This control panel has moved to another server. Your DNS resolver still points to the old one; try again shortly.\" 503\n"
		if next := controlCaddyfileSite(string(raw), host, notice); next != string(raw) {
			if err := writeFileAtomic(filepath.Join(m.controlDir(), "Caddyfile.next"), []byte(next), 0o600); err == nil {
				if err := m.controlHelper(ctx, controlActionCaddy); err != nil {
					notes = append(notes, "The Caddyfile site was left unchanged: "+err.Error())
				} else {
					notes = append(notes, "The Caddyfile site now answers that the panel moved.")
				}
			}
		}
	}
	if caddy, ok := m.nativeCaddy(ctx); ok {
		if _, err := caddy.call(ctx, http.MethodDelete, "/id/"+controlRouteID, nil); err == nil {
			notes = append(notes, "The admin-API route was withdrawn.")
		}
	}
	if err := m.controlHelper(ctx, controlActionStop); err != nil {
		return notes, err
	}
	return append(notes, "The control panel unit is stopped and disabled."), nil
}
