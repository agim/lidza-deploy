package agent

// Docker on demand: hosts that only run native applications do not need a
// container runtime. The first container deployment on a server installs
// Docker Engine from Docker's official apt repository, verified against the
// published signing key fingerprint. On hosts whose Docker the agent manages
// it also keeps live-restore on, so a daemon upgrade or restart no longer
// restarts every container.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	dockerKeyring      = "/etc/apt/keyrings/docker.asc"
	dockerSourceList   = "/etc/apt/sources.list.d/docker.list"
	dockerDaemonConfig = "/etc/docker/daemon.json"
	// Docker's release signing key, as published in its installation guide.
	dockerKeyFingerprint = "9DC858229FC7DD38854AE2D88D81803C0EBFCD88"
)

// dockerDefaultDaemon rotates container logs and keeps containers running
// across daemon restarts. It is written only when no daemon.json exists.
const dockerDefaultDaemon = `{
  "log-driver": "json-file",
  "log-opts": { "max-size": "10m", "max-file": "3" },
  "live-restore": true
}
`

var dockerInstallMu sync.Mutex

// dockerReloadWait spaces the checks for a reloaded configuration; tests
// shorten it.
var dockerReloadWait = time.Second

func init() {
	ensureDocker = ensureDockerEngine
}

// ensureDockerEngine returns quickly when Docker is running, starts an
// installed but stopped daemon, and otherwise installs Docker Engine. On hosts
// where the agent cannot install packages (not root, not apt/systemd) it does
// nothing, and the deployment reports Docker's own error.
func ensureDockerEngine(ctx context.Context, m *Manager) error {
	if dockerRunning(ctx) {
		return nil
	}
	if os.Geteuid() != 0 {
		return nil
	}
	if ok, _ := aptHost(); !ok {
		return nil
	}
	dockerInstallMu.Lock()
	defer dockerInstallMu.Unlock()
	if dockerRunning(ctx) {
		return nil
	}
	commandDiagnostic(ctx, "deployment", "Docker is not running on this server; preparing the container runtime.")
	if hostToolAvailable("dockerd") {
		// A host where Docker was switched off by masking its units
		// (persistently or with --runtime) cannot start it until they are
		// unmasked; systemctl then fails with "Unit docker.service is
		// masked". The application asked for containers, so unmask them.
		if masked := dockerMaskedUnits(ctx); len(masked) > 0 {
			commandDiagnostic(ctx, "deployment", "Docker's units were masked on this server ("+strings.Join(masked, ", ")+"); unmasking them to start the container runtime.")
			for _, flags := range [][]string{{"unmask"}, {"unmask", "--runtime"}} {
				args := append(slices.Clone(flags), masked...)
				if out, err := hostCommand(ctx, "systemctl", args...); err != nil {
					return fmt.Errorf("Docker's units are masked and could not be unmasked: %s", strings.TrimSpace(out))
				}
			}
			_, _ = hostCommand(ctx, "systemctl", "daemon-reload")
		}
		if out, err := hostCommand(ctx, "systemctl", "enable", "--now", "containerd.service", "docker.socket", "docker.service"); err != nil {
			return fmt.Errorf("Docker is installed but could not be started: %s", strings.TrimSpace(out))
		}
		return waitDocker(ctx)
	}
	if err := installDockerEngine(ctx); err != nil {
		return fmt.Errorf("installing Docker: %w", err)
	}
	m.updateDockerState(func(s *HostDockerState) { s.InstalledByAgent = true })
	commandDiagnostic(ctx, "deployment", "Docker Engine installed from Docker's official repository.")
	return waitDocker(ctx)
}

// updateDockerState edits the persisted Docker record under the host lock.
func (m *Manager) updateDockerState(edit func(*HostDockerState)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.hostUpdatesStateLocked()
	if state.Docker == nil {
		state.Docker = &HostDockerState{}
	}
	edit(state.Docker)
	if err := m.save(); err != nil {
		log.Printf("docker state: %v", err)
	}
}

// dockerManaged reports whether the daemon configuration is the agent's to
// reconcile: it installed Docker, or it runs hosted applications in
// containers on this host. Hosts that merely have Docker are left alone.
func (m *Manager) dockerManaged() bool {
	_, containers := m.runtime.(*Docker)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.HostUpdates != nil && m.data.HostUpdates.Docker != nil && m.data.HostUpdates.Docker.InstalledByAgent {
		return true
	}
	if !containers {
		return false
	}
	for _, a := range m.data.Apps {
		if a.Native == nil && a.Current != nil {
			return true
		}
	}
	return false
}

// observeDocker reads the daemon's live-restore state from `docker info` and
// the configured one from daemon.json. It never changes anything.
func observeDocker(ctx context.Context) *HostDockerStatus {
	status := &HostDockerStatus{Installed: hostToolAvailable("dockerd") || hostToolAvailable("docker")}
	if !status.Installed {
		return status
	}
	status.Configured, _ = dockerLiveRestoreConfigured()
	if !hostToolAvailable("docker") {
		return status
	}
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := hostCommand(bounded, "docker", "info", "--format", "{{.ServerVersion}} {{.LiveRestoreEnabled}}")
	if err != nil {
		return status
	}
	fields := strings.Fields(out)
	if len(fields) >= 1 {
		status.Running = true
		status.Version = fields[0]
	}
	if len(fields) >= 2 {
		status.LiveRestore = fields[1] == "true"
	}
	_, err = hostCommand(bounded, "docker", "buildx", "version")
	status.Buildx = err == nil
	return status
}

// installBuildx adds Docker's BuildKit plugin: docker-buildx-plugin when
// Docker comes from Docker's repository, docker-buildx when it is the
// distribution's docker.io package (the two do not mix).
func installBuildx(ctx context.Context) (string, error) {
	if out, err := hostCommand(ctx, "apt-get", "update", "-q", "-o", "DPkg::Lock::Timeout=300"); err != nil {
		return out, fmt.Errorf("apt-get update: %s", lastLines(out))
	}
	pkg := buildxPackage(ctx)
	apt := []string{"install", "-y", "-q", "-o", "DPkg::Lock::Timeout=300", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold", pkg}
	out, err := hostCommand(ctx, "apt-get", apt...)
	if err != nil {
		return out, fmt.Errorf("installing %s: %s", pkg, lastLines(out))
	}
	buildModeMu.Lock()
	buildModeChecked = time.Time{} // builds pick BuildKit up at once
	buildModeMu.Unlock()
	return out + "\nSummary: Docker buildx plugin installed; image builds use BuildKit.", nil
}

// dockerWarning explains an inactive live-restore and what happens next.
func dockerWarning(d HostDockerStatus, policy HostUpdatesPolicy, nextWindow time.Time) string {
	if !d.Running || d.LiveRestore {
		return ""
	}
	consequence := "Docker live-restore is off: a Docker upgrade or daemon restart stops and restarts every container. "
	switch {
	case !d.Managed:
		return consequence + "Set \"live-restore\": true in /etc/docker/daemon.json and reload Docker; the agent does not manage Docker on this server."
	case d.RestartPending && policy.deferDisruptive() && !nextWindow.IsZero():
		return consequence + "daemon.json now enables it, but the running daemon did not apply it on reload; Docker restarts in the maintenance window starting " + nextWindow.Format("Mon 02 Jan 15:04 UTC") + " (containers restart once)."
	case d.RestartPending:
		return consequence + "daemon.json now enables it, but the running daemon did not apply it on reload; restart Docker at a quiet moment to apply it."
	}
	return consequence + "The agent enables it on its next hourly check by merging it into daemon.json and reloading the daemon, which keeps containers running."
}

// dockerDaemonConfig reads /etc/docker/daemon.json as a generic object so
// other keys survive a merge. A missing file is an empty configuration; a
// malformed one is an error, never silently replaced.
func readDockerDaemonConfig() (map[string]any, bool, error) {
	raw, err := os.ReadFile(hostPath(dockerDaemonConfig))
	if os.IsNotExist(err) {
		return map[string]any{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	cfg := map[string]any{}
	if strings.TrimSpace(string(raw)) == "" {
		return cfg, true, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, true, fmt.Errorf("%s is not valid JSON: %w", dockerDaemonConfig, err)
	}
	return cfg, true, nil
}

func dockerLiveRestoreConfigured() (bool, error) {
	cfg, _, err := readDockerDaemonConfig()
	if err != nil {
		return false, err
	}
	enabled, _ := cfg["live-restore"].(bool)
	return enabled, nil
}

// dockerLiveRestoreActive asks the running daemon.
func dockerLiveRestoreActive(ctx context.Context) bool {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := hostCommand(bounded, "docker", "info", "--format", "{{.LiveRestoreEnabled}}")
	return err == nil && strings.TrimSpace(out) == "true"
}

// enableDockerLiveRestore merges "live-restore": true into daemon.json,
// keeping every other key and a backup of the previous file, then reloads
// the daemon. live-restore is one of the options dockerd re-reads on SIGHUP,
// so the reload applies it without restarting containers; the result is
// verified with `docker info`. When the daemon does not report it, only a
// restart would help, and that is left to the maintenance window.
func (m *Manager) enableDockerLiveRestore(ctx context.Context) (string, error) {
	var transcript strings.Builder
	cfg, exists, err := readDockerDaemonConfig()
	if err != nil {
		return "", fmt.Errorf("Docker configuration left untouched: %w", err)
	}
	path := hostPath(dockerDaemonConfig)
	if enabled, _ := cfg["live-restore"].(bool); !enabled {
		cfg["live-restore"] = true
		data, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return "", err
		}
		data = append(data, '\n')
		if exists {
			previous, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			backup := path + ".bak-" + time.Now().UTC().Format("20060102T150405Z")
			if err := writeHostFile(backup, previous, 0644); err != nil {
				return "", fmt.Errorf("backing up daemon.json: %w", err)
			}
			m.updateDockerState(func(s *HostDockerState) { s.ConfigBackup = backup })
			transcript.WriteString("Previous configuration saved as " + backup + ".\n")
		}
		if hostToolAvailable("dockerd") {
			staged := path + ".lidza-validate"
			if err := writeHostFile(staged, data, 0644); err != nil {
				return transcript.String(), err
			}
			out, err := hostCommand(ctx, "dockerd", "--validate", "--config-file", staged)
			_ = os.Remove(staged)
			// Older daemons have no --validate; anything else is a real rejection.
			if err != nil && !strings.Contains(out, "unknown flag") && !strings.Contains(out, "flag provided but not defined") {
				return transcript.String(), errors.New("dockerd rejected the merged configuration; nothing was changed: " + lastLines(out))
			}
		}
		if err := writeHostFile(path, data, 0644); err != nil {
			return transcript.String(), err
		}
		transcript.WriteString("Set \"live-restore\": true in " + dockerDaemonConfig + "; other keys unchanged.\n")
	} else {
		transcript.WriteString(dockerDaemonConfig + " already enables live-restore; the daemon had not applied it.\n")
	}
	out, err := hostCommand(ctx, "systemctl", "reload", "docker.service")
	transcript.WriteString("$ systemctl reload docker.service\n" + out + "\n")
	if err != nil {
		// Units without ExecReload still accept the signal dockerd reloads on.
		out, err = hostCommand(ctx, "systemctl", "kill", "--kill-whom=main", "--signal=SIGHUP", "docker.service")
		transcript.WriteString("$ systemctl kill --kill-whom=main --signal=SIGHUP docker.service\n" + out + "\n")
		if err != nil {
			return transcript.String(), errors.New("Docker could not be reloaded: " + lastLines(out))
		}
	}
	for range 10 {
		if dockerLiveRestoreActive(ctx) {
			m.updateDockerState(func(s *HostDockerState) { s.RestartPending = false; s.LiveRestoreApplied = time.Now().UTC() })
			transcript.WriteString("Summary: Docker live-restore is on; the daemon reloaded its configuration and containers kept running.\n")
			return transcript.String(), nil
		}
		select {
		case <-ctx.Done():
			return transcript.String(), ctx.Err()
		case <-time.After(dockerReloadWait):
		}
	}
	m.updateDockerState(func(s *HostDockerState) { s.RestartPending = true })
	return transcript.String(), errors.New("daemon.json enables live-restore but the running daemon did not apply it on reload; Docker restarts in the maintenance window to pick it up")
}

// restartDocker applies a configuration the daemon did not reload. It runs
// only inside the maintenance window: without live-restore active every
// container restarts with the daemon.
func (m *Manager) restartDocker(ctx context.Context) (string, error) {
	out, err := hostCommand(ctx, "systemctl", "restart", "docker.service")
	transcript := "$ systemctl restart docker.service\n" + out + "\n"
	if err != nil {
		return transcript, errors.New("Docker restart failed: " + lastLines(out))
	}
	if err := waitDocker(ctx); err != nil {
		return transcript, err
	}
	if !dockerLiveRestoreActive(ctx) {
		return transcript, errors.New("Docker restarted but still reports live-restore off; check /etc/docker/daemon.json and the unit's flags by hand")
	}
	m.updateDockerState(func(s *HostDockerState) { s.RestartPending = false; s.LiveRestoreApplied = time.Now().UTC() })
	return transcript + "Summary: Docker restarted in the maintenance window; live-restore is now on and later daemon restarts keep containers running.\n", nil
}

// dockerTick reconciles live-restore on managed hosts: hourly it enables and
// reloads when the daemon runs without it; a restart the reload could not
// avoid waits for the maintenance window.
func (m *Manager) dockerTick(now time.Time) {
	if os.Geteuid() != 0 || !m.dockerManaged() {
		return
	}
	m.mu.Lock()
	policy := m.hostPolicyLocked()
	restartPending := m.data.HostUpdates != nil && m.data.HostUpdates.Docker != nil && m.data.HostUpdates.Docker.RestartPending
	m.mu.Unlock()
	if restartPending {
		if _, ok := policy.RebootWindow.active(now); ok && policy.deferDisruptive() {
			_ = m.startHostJob("docker-restart", "schedule", m.restartDocker)
		}
		return
	}
	hc := m.host()
	hc.mu.Lock()
	due := now.Sub(hc.dockerChecked) >= time.Hour
	if due {
		hc.dockerChecked = now
	}
	hc.mu.Unlock()
	if !due {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
	facts := observeDocker(ctx)
	cancel()
	if facts.Running && !facts.Buildx && hostFileExists(dockerSourceList) {
		// Docker's repository is configured (the agent's install): add
		// the BuildKit plugin builds use.
		if err := m.startHostJob("docker-buildx", "schedule", installBuildx); err != nil {
			hc.mu.Lock()
			hc.dockerChecked = time.Time{}
			hc.mu.Unlock()
		}
		return
	}
	if !facts.Running || facts.LiveRestore {
		return
	}
	if err := m.startHostJob("docker-live-restore", "schedule", m.enableDockerLiveRestore); err != nil {
		// Busy host: look again next minute rather than next hour.
		hc.mu.Lock()
		hc.dockerChecked = time.Time{}
		hc.mu.Unlock()
	}
}

func dockerRunning(ctx context.Context) bool {
	if !hostToolAvailable("docker") {
		return false
	}
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := hostCommand(bounded, "docker", "info", "--format", "{{.ServerVersion}}")
	return err == nil
}

func waitDocker(ctx context.Context) error {
	for range 30 {
		if dockerRunning(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("Docker was started but does not answer; inspect `systemctl status docker` on the host")
}

// dockerRepositoryLine renders Docker's apt source for this distribution.
func dockerRepositoryLine(osRelease map[string]string, arch string) (string, error) {
	distro := osRelease["ID"]
	codename := osRelease["VERSION_CODENAME"]
	if distro == "ubuntu" && osRelease["UBUNTU_CODENAME"] != "" {
		codename = osRelease["UBUNTU_CODENAME"]
	}
	if distro != "ubuntu" && distro != "debian" {
		return "", errors.New("Docker's repository supports Ubuntu and Debian hosts")
	}
	if codename == "" || strings.ContainsAny(codename, " \t\n/") || arch == "" || strings.ContainsAny(arch, " \t\n/") {
		return "", errors.New("the distribution release could not be identified")
	}
	return fmt.Sprintf("deb [arch=%s signed-by=%s] https://download.docker.com/linux/%s %s stable\n", arch, dockerKeyring, distro, codename), nil
}

// gpgFingerprints lists primary-key fingerprints in an armored key file.
func gpgFingerprints(ctx context.Context, path string) ([]string, error) {
	out, err := hostCommand(ctx, "gpg", "--batch", "--no-default-keyring", "--show-keys", "--with-colons", "--with-fingerprint", path)
	if err != nil {
		return nil, errors.New("the signing key could not be read with gpg")
	}
	return parseGPGFingerprints(out), nil
}

// parseGPGFingerprints reads primary-key fingerprints from gpg's
// --with-colons listing; subkey fingerprints are skipped.
func parseGPGFingerprints(out string) []string {
	var fingerprints []string
	expect := false
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, ":")
		switch {
		case fields[0] == "pub":
			expect = true
		case fields[0] == "fpr" && expect && len(fields) > 9:
			fingerprints = append(fingerprints, fields[9])
			expect = false
		case fields[0] == "sub":
			expect = false
		}
	}
	return fingerprints
}

func installDockerEngine(ctx context.Context) error {
	raw, err := os.ReadFile(hostPath("/etc/os-release"))
	if err != nil {
		return err
	}
	osRelease := parseOSRelease(string(raw))
	archOut, err := hostCommand(ctx, "dpkg", "--print-architecture")
	if err != nil {
		return errors.New("the package architecture could not be determined")
	}
	line, err := dockerRepositoryLine(osRelease, strings.TrimSpace(archOut))
	if err != nil {
		return err
	}
	apt := []string{"-y", "-q", "-o", "DPkg::Lock::Timeout=300", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold"}
	if !hostToolAvailable("gpg") {
		if out, err := hostCommand(ctx, "apt-get", "update", "-q", "-o", "DPkg::Lock::Timeout=300"); err != nil {
			return fmt.Errorf("apt-get update: %s", lastLines(out))
		}
		if out, err := hostCommand(ctx, "apt-get", append([]string{"install"}, append(apt, "ca-certificates", "gnupg")...)...); err != nil {
			return fmt.Errorf("installing gnupg: %s", lastLines(out))
		}
	}
	key, err := downloadSigningKey(ctx, "https://download.docker.com/linux/"+osRelease["ID"]+"/gpg")
	if err != nil {
		return err
	}
	staged := hostPath(dockerKeyring + ".download")
	if err := writeHostFile(staged, key, 0644); err != nil {
		return err
	}
	defer os.Remove(staged)
	fingerprints, err := gpgFingerprints(ctx, staged)
	if err != nil {
		return err
	}
	if len(fingerprints) != 1 || !strings.EqualFold(fingerprints[0], dockerKeyFingerprint) {
		return fmt.Errorf("Docker's signing key fingerprint %v does not match the published %s; nothing was installed", fingerprints, dockerKeyFingerprint)
	}
	if err := writeHostFile(hostPath(dockerKeyring), key, 0644); err != nil {
		return err
	}
	if err := writeHostFile(hostPath(dockerSourceList), []byte(line), 0644); err != nil {
		return err
	}
	// Defaults only for a fresh daemon; an existing daemon.json is the
	// operator's and stays untouched.
	if _, err := os.Stat(hostPath(dockerDaemonConfig)); os.IsNotExist(err) {
		if err := writeHostFile(hostPath(dockerDaemonConfig), []byte(dockerDefaultDaemon), 0644); err != nil {
			return err
		}
	}
	if out, err := hostCommand(ctx, "apt-get", "update", "-q", "-o", "DPkg::Lock::Timeout=300"); err != nil {
		return fmt.Errorf("apt-get update: %s", lastLines(out))
	}
	if out, err := hostCommand(ctx, "apt-get", append([]string{"install"}, append(apt, "docker-ce", "docker-ce-cli", "containerd.io", "docker-buildx-plugin", "docker-compose-plugin")...)...); err != nil {
		return fmt.Errorf("apt-get install: %s", lastLines(out))
	}
	if out, err := hostCommand(ctx, "systemctl", "enable", "--now", "docker.service", "containerd.service"); err != nil {
		return fmt.Errorf("starting Docker: %s", lastLines(out))
	}
	return nil
}

func downloadSigningKey(ctx context.Context, url string) ([]byte, error) {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(bounded, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading the signing key: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading the signing key: HTTP %d", res.StatusCode)
	}
	key, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(key, []byte("BEGIN PGP PUBLIC KEY BLOCK")) {
		return nil, errors.New("the downloaded signing key is not an armored OpenPGP key")
	}
	return key, nil
}

func lastLines(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	return strings.Join(lines, " | ")
}

// dockerMaskedUnits lists Docker's units that systemd reports as masked
// ("masked" or "masked-runtime").
func dockerMaskedUnits(ctx context.Context) []string {
	units := []string{"containerd.service", "docker.socket", "docker.service"}
	masked := []string{}
	for _, unit := range units {
		out, _ := hostCommand(ctx, "systemctl", "is-enabled", unit)
		if strings.HasPrefix(strings.TrimSpace(out), "masked") {
			masked = append(masked, unit)
		}
	}
	return masked
}

// buildxPackage names the buildx package matching the installed Docker:
// docker-buildx for the distribution's docker.io, docker-buildx-plugin
// otherwise (Docker's repository).
func buildxPackage(ctx context.Context) string {
	out, _ := hostCommand(ctx, "dpkg-query", "-W", "-f=${Status}", "docker.io")
	if strings.Contains(out, "install ok installed") {
		return "docker-buildx"
	}
	return "docker-buildx-plugin"
}
