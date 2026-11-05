package agent

// Docker on demand: hosts that only run native applications do not need a
// container runtime. The first container deployment on a server installs
// Docker Engine from Docker's official apt repository, verified against the
// published signing key fingerprint.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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
	if m.cfg.HostMode == "coexist" {
		return errors.New("Docker is not running, and this agent shares the host (coexist mode); start Docker on the host before deploying containers")
	}
	dockerInstallMu.Lock()
	defer dockerInstallMu.Unlock()
	if dockerRunning(ctx) {
		return nil
	}
	commandDiagnostic(ctx, "deployment", "Docker is not running on this server; preparing the container runtime.")
	if hostToolAvailable("dockerd") {
		if out, err := hostCommand(ctx, "systemctl", "enable", "--now", "docker.service"); err != nil {
			return fmt.Errorf("Docker is installed but could not be started: %s", strings.TrimSpace(out))
		}
		return waitDocker(ctx)
	}
	if err := installDockerEngine(ctx); err != nil {
		return fmt.Errorf("installing Docker: %w", err)
	}
	commandDiagnostic(ctx, "deployment", "Docker Engine installed from Docker's official repository.")
	return waitDocker(ctx)
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
	return fingerprints, nil
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
