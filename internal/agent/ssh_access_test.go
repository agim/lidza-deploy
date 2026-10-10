package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"golang.org/x/crypto/ssh"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func sshTestKey(t *testing.T) SSHKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	v, err := normalizeSSHKey("MacBook", string(ssh.MarshalAuthorizedKey(k)))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestSSHKeyValidation(t *testing.T) {
	k := sshTestKey(t)
	for _, bad := range []string{"-----BEGIN OPENSSH PRIVATE KEY-----\nsecret", `command="id" ` + k.PublicKey, k.PublicKey + "\n" + k.PublicKey, "not a key"} {
		if _, err := normalizeSSHKey("Mac", bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, err := normalizeSSHKey("bad\nlabel", k.PublicKey); err == nil {
		t.Fatal("accepted control characters")
	}
	v, err := validateSSHAccess(SSHAccess{User: "root", Keys: []SSHKey{k}})
	if err != nil || v.User != "deploy" {
		t.Fatal(v, err)
	}
	if _, err = validateSSHAccess(SSHAccess{Keys: []SSHKey{k, k}}); err == nil {
		t.Fatal("accepted duplicate")
	}
}
func TestSSHQueuePrivateAndAuthenticated(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	k := sshTestKey(t)
	if err := m.queueSSHAccess(SSHAccess{Keys: []SSHKey{k}}); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"ssh-access/request.json", "upgrade/request"} {
		info, err := os.Stat(filepath.Join(m.cfg.DataDir, file))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal(info, err)
		}
	}
	if m.sshAccess().State != "queued" || !m.upgradePending() {
		t.Fatal("lost pending request")
	}
	if err := m.queueSSHAccess(SSHAccess{}); err == nil {
		t.Fatal("overwrote pending request")
	}
	r := httptest.NewRequest("GET", "/v1/ssh-access", nil)
	w := httptest.NewRecorder()
	Handler(m).ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("unprotected SSH API: %d", w.Code)
	}
}
func sshHostFixture(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	for _, p := range []string{"etc/ssh/sshd_config.d", "etc/ssh/lidza-deploy-keys", "etc/sudoers.d", "home/deploy/.ssh"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "home/deploy/.ssh/authorized_keys"), []byte("unmanaged-key"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root, dir
}
func sshFakeCommand(failValidate bool) sshCommand {
	return func(name string, args ...string) ([]byte, error) {
		if name == "/usr/bin/getent" && args[0] == "passwd" {
			return []byte("deploy:x:1001:1001::/home/deploy:/bin/bash\n"), nil
		}
		if name == "/usr/sbin/sshd" {
			if args[0] == "-t" && failValidate {
				return nil, errors.New("invalid config")
			}
			return []byte("authorizedkeysfile .ssh/authorized_keys .ssh/authorized_keys2 " + sshAuthorizedPath + "\n"), nil
		}
		return nil, nil
	}
}
func TestSSHApplyRevokeAndSudo(t *testing.T) {
	root, dir := sshHostFixture(t)
	v := SSHAccess{Keys: []SSHKey{sshTestKey(t)}}
	if err := applySSHProfile(root, sshFakeCommand(false), v); err != nil {
		t.Fatal(err)
	}
	key, err := root.ReadFile(sshKeyPath)
	if err != nil || !strings.Contains(string(key), v.Keys[0].PublicKey) {
		t.Fatal(string(key), err)
	}
	info, _ := root.Stat(sshKeyPath)
	if info.Mode().Perm() != 0644 {
		t.Fatal("deploy cannot read key file")
	}
	if _, err = root.Stat(sshSudoPath); !os.IsNotExist(err) {
		t.Fatal("sudo enabled by default")
	}
	config, _ := root.ReadFile(sshConfigPath)
	if strings.Count(string(config), sshAuthorizedPath) != 1 || !strings.Contains(string(config), ".ssh/authorized_keys2") {
		t.Fatal("existing key sources lost or duplicated")
	}
	v.Sudo = true
	if err = applySSHProfile(root, sshFakeCommand(false), v); err != nil {
		t.Fatal(err)
	}
	sudo, _ := root.ReadFile(sshSudoPath)
	if !strings.Contains(string(sudo), "deploy ALL=(ALL:ALL) NOPASSWD: ALL") {
		t.Fatal("explicit sudo not applied")
	}
	v.Sudo = false
	v.Keys = nil
	if err = applySSHProfile(root, sshFakeCommand(false), v); err != nil {
		t.Fatal(err)
	}
	key, _ = root.ReadFile(sshKeyPath)
	if strings.TrimSpace(string(key)) != "" {
		t.Fatal("revoked key remains")
	}
	unmanaged, _ := os.ReadFile(filepath.Join(dir, "home/deploy/.ssh/authorized_keys"))
	if string(unmanaged) != "unmanaged-key" {
		t.Fatal("unmanaged keys changed")
	}
}
func TestSSHValidationFailureRollsBack(t *testing.T) {
	root, _ := sshHostFixture(t)
	v := SSHAccess{Keys: []SSHKey{sshTestKey(t)}}
	if err := applySSHProfile(root, sshFakeCommand(false), v); err != nil {
		t.Fatal(err)
	}
	before, _ := root.ReadFile(sshKeyPath)
	v.Keys = []SSHKey{sshTestKey(t)}
	v.Sudo = true
	if err := applySSHProfile(root, sshFakeCommand(true), v); err == nil {
		t.Fatal("invalid SSH config accepted")
	}
	after, _ := root.ReadFile(sshKeyPath)
	if string(before) != string(after) {
		t.Fatal("lost working keys")
	}
	if _, err := root.Stat(sshSudoPath); !os.IsNotExist(err) {
		t.Fatal("failed operation left elevated privileges")
	}
}
func TestSSHRejectsRootAccountAndDestinationSymlink(t *testing.T) {
	root, dir := sshHostFixture(t)
	cmd := func(string, ...string) ([]byte, error) { return []byte("deploy:x:0:0::/root:/bin/bash"), nil }
	if err := applySSHProfile(root, cmd, SSHAccess{}); err == nil {
		t.Fatal("accepted UID0")
	}
	outside := filepath.Join(t.TempDir(), "untouched")
	os.WriteFile(outside, []byte("safe"), 0600)
	os.Symlink(outside, filepath.Join(dir, sshKeyPath))
	if err := applySSHProfile(root, sshFakeCommand(false), SSHAccess{}); err == nil {
		t.Fatal("accepted symlink")
	}
	b, _ := os.ReadFile(outside)
	if string(b) != "safe" {
		t.Fatal("wrote through symlink")
	}
}
func TestSSHRequestCannotEscapeAndRetainsAppliedStateOnFailure(t *testing.T) {
	host, _ := sshHostFixture(t)
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "ssh-access"), 0700)
	data, _ := os.OpenRoot(dir)
	defer data.Close()
	old := SSHAccess{Keys: []SSHKey{sshTestKey(t)}, State: "ready"}
	b, _ := json.Marshal(old)
	writeSSHFile(data, "ssh-access/status.json", b, 0600)
	next := SSHAccess{Keys: []SSHKey{sshTestKey(t)}, Sudo: true}
	b, _ = json.Marshal(next)
	writeSSHFile(data, "ssh-access/request.json", b, 0600)
	if err := applySSHRequest(data, host, sshFakeCommand(true), -1, -1); err == nil {
		t.Fatal("accepted invalid SSH config")
	}
	var status SSHAccess
	readSSHFile(data, "ssh-access/status.json", &status)
	if status.State != "failed" || status.Sudo || status.Keys[0].ID != old.Keys[0].ID {
		t.Fatal("failed status advertised unapplied access", status)
	}
	outside := filepath.Join(t.TempDir(), "request.json")
	os.WriteFile(outside, b, 0600)
	os.Symlink(outside, filepath.Join(dir, "ssh-access/request.json"))
	called := false
	cmd := func(string, ...string) ([]byte, error) { called = true; return nil, nil }
	if err := applySSHRequest(data, host, cmd, -1, -1); err == nil || called {
		t.Fatal("escaped request root")
	}
}

// Exercise real OpenSSH parsing without changing accounts, services or host files.
func TestSSHRealOpenSSHConfiguration(t *testing.T) {
	if os.Getenv("TEST_SSH") != "1" {
		t.Skip("set TEST_SSH=1 with OpenSSH installed")
	}
	root, dir := sshHostFixture(t)
	keyPath := filepath.Join(dir, "host_key")
	if out, err := exec.Command("/usr/bin/ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("host key: %s %v", out, err)
	}
	configPath := filepath.Join(dir, "sshd_config")
	config := "HostKey " + keyPath + "\nInclude " + filepath.Join(dir, "etc/ssh/sshd_config.d/*.conf") + "\nAuthorizedKeysFile .ssh/authorized_keys\n"
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	fake := sshFakeCommand(false)
	command := func(name string, args ...string) ([]byte, error) {
		if name == "/usr/sbin/sshd" {
			args = append([]string{"-f", configPath}, args...)
			out, err := exec.Command(name, args...).CombinedOutput()
			if err != nil {
				t.Logf("sshd: %s", out)
			}
			return out, err
		}
		return fake(name, args...)
	}
	profile := SSHAccess{Keys: []SSHKey{sshTestKey(t)}}
	for i := 0; i < 2; i++ {
		if err := applySSHProfile(root, command, profile); err != nil {
			t.Fatal(err)
		}
	}
	output, err := command("/usr/sbin/sshd", "-T", "-C", "user=deploy,host=localhost,addr=127.0.0.1")
	if err != nil || !strings.Contains(string(output), "authorizedkeysfile .ssh/authorized_keys "+sshAuthorizedPath) {
		t.Fatalf("incorrect effective config: %s %v", output, err)
	}
}

func TestSSHCreatesDedicatedAccountWithoutPasswordOrImplicitSudo(t *testing.T) {
	root, _ := sshHostFixture(t)
	reads := 0
	created := false
	fake := sshFakeCommand(false)
	command := func(name string, args ...string) ([]byte, error) {
		if name == "/usr/bin/getent" && args[0] == "passwd" {
			reads++
			if reads == 1 {
				return nil, errors.New("account missing")
			}
		}
		if name == "/usr/sbin/useradd" {
			created = true
			if strings.Join(args, " ") != "--create-home --shell /bin/bash --password * deploy" {
				t.Fatalf("unsafe account creation: %v", args)
			}
		}
		if name == "/usr/sbin/usermod" && (strings.Contains(strings.Join(args, " "), "sudo") || strings.Contains(strings.Join(args, " "), "docker")) {
			t.Fatal("implicit root-level group membership")
		}
		return fake(name, args...)
	}
	if err := applySSHProfile(root, command, SSHAccess{Keys: []SSHKey{sshTestKey(t)}}); err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("account was not created")
	}
	if _, err := root.Stat(sshSudoPath); !os.IsNotExist(err) {
		t.Fatal("implicit sudo grant")
	}
}
