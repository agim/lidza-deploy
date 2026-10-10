package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const sshKeyPath = "etc/ssh/lidza-deploy-keys/deploy"
const sshConfigPath = "etc/ssh/sshd_config.d/00-lidza-deploy-keys.conf"
const sshSudoPath = "etc/sudoers.d/lidza-deploy-ssh"
const sshAuthorizedPath = "/etc/ssh/lidza-deploy-keys/%u"

type sshCommand func(string, ...string) ([]byte, error)

// ApplySSHAccess is the fixed privileged operation invoked by the root updater.
// The account, data directory and destination files cannot be selected by a caller.
func ApplySSHAccess() error {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("managed SSH access requires the Linux root helper")
	}
	account, err := user.Lookup("lidza-agent")
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return err
	}
	data, err := os.OpenRoot("/var/lib/lidza-agent")
	if err != nil {
		return err
	}
	defer data.Close()
	host, err := os.OpenRoot("/")
	if err != nil {
		return err
	}
	defer host.Close()
	command := func(name string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			return out, fmt.Errorf("%s failed: %w", name, err)
		}
		return out, nil
	}
	return applySSHRequest(data, host, command, uid, gid)
}

func saveSSHStatus(root *os.Root, v SSHAccess, uid, gid int) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err = writeSSHFile(root, "ssh-access/status.json", data, 0600); err != nil {
		return err
	}
	return root.Chown("ssh-access/status.json", uid, gid)
}

func applySSHRequest(data, host *os.Root, command sshCommand, uid, gid int) error {
	var previous SSHAccess
	_ = readSSHFile(data, "ssh-access/status.json", &previous)
	previous, err := validateSSHAccess(previous)
	if err != nil {
		previous = SSHAccess{User: "deploy", Keys: []SSHKey{}}
	}
	var requested SSHAccess
	err = readSSHFile(data, "ssh-access/request.json", &requested)
	if err == nil {
		requested, err = validateSSHAccess(requested)
	}
	if err == nil {
		applying := previous
		applying.State = "applying"
		applying.Message = "Applying managed SSH access"
		if e := saveSSHStatus(data, applying, uid, gid); e != nil {
			return e
		}
	}
	// Requests are consumed even when invalid; successful state only describes applied access.
	if e := data.Remove("ssh-access/request.json"); e != nil && !os.IsNotExist(e) {
		return e
	}
	if err == nil {
		err = applySSHProfile(host, command, requested)
	}
	result := requested
	if err != nil {
		result = previous
		result.State = "failed"
		result.Message = err.Error()
	} else {
		result.State = "ready"
		result.Message = "Managed SSH access applied"
	}
	if e := saveSSHStatus(data, result, uid, gid); e != nil {
		return e
	}
	return err
}

func authorizedKeysFiles(output []byte) (string, error) {
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "authorizedkeysfile ") {
			paths := strings.Fields(strings.TrimPrefix(line, "authorizedkeysfile "))
			kept := []string{}
			for _, path := range paths {
				if path != sshAuthorizedPath && path != "none" {
					kept = append(kept, path)
				}
			}
			kept = append(kept, sshAuthorizedPath)
			return strings.Join(kept, " "), nil
		}
	}
	return "", errors.New("sshd did not report its AuthorizedKeysFile configuration")
}

func applySSHProfile(root *os.Root, command sshCommand, v SSHAccess) (resultErr error) {
	// Ubuntu/Debian account and service tools are used directly, never through a shell.
	entry, err := command("/usr/bin/getent", "passwd", "deploy")
	if err != nil {
		// An unusable hash disables password authentication without the account
		// lock marker that causes non-PAM sshd to reject public keys too.
		if _, err = command("/usr/sbin/useradd", "--create-home", "--shell", "/bin/bash", "--password", "*", "deploy"); err != nil {
			return errors.New("could not create deploy account")
		}
		entry, err = command("/usr/bin/getent", "passwd", "deploy")
		if err != nil {
			return err
		}
	}
	fields := strings.Split(strings.TrimSpace(string(entry)), ":")
	if len(fields) != 7 || fields[0] != "deploy" {
		return errors.New("invalid deploy account")
	}
	uid, err := strconv.Atoi(fields[2])
	if err != nil || uid < 1000 || uid == 65534 {
		return errors.New("deploy must be a dedicated non-system account")
	}
	if fields[6] == "/usr/sbin/nologin" || fields[6] == "/bin/false" {
		return errors.New("deploy account does not permit an interactive shell")
	}
	groups := []string{}
	for _, group := range []string{"adm", "systemd-journal"} {
		if _, err = command("/usr/bin/getent", "group", group); err == nil {
			groups = append(groups, group)
		}
	}
	if len(groups) > 0 {
		if _, err = command("/usr/sbin/usermod", "--append", "--groups", strings.Join(groups, ","), "deploy"); err != nil {
			return errors.New("could not grant deploy access to system logs")
		}
	}
	output, err := command("/usr/sbin/sshd", "-T")
	if err != nil {
		return errors.New("OpenSSH server is unavailable or its configuration is invalid; install openssh-server and check sshd -t")
	}
	paths, err := authorizedKeysFiles(output)
	if err != nil {
		return err
	}
	for _, dir := range []string{"etc/ssh/lidza-deploy-keys", "etc/ssh/sshd_config.d", "etc/sudoers.d"} {
		if err = root.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	if err = root.Chmod("etc/ssh/lidza-deploy-keys", 0755); err != nil {
		return err
	}
	type snapshot struct {
		path   string
		data   []byte
		mode   os.FileMode
		exists bool
	}
	snapshots := []snapshot{}
	for _, path := range []string{sshKeyPath, sshConfigPath, sshSudoPath} {
		// These paths live in root-owned directories and are never agent-selected.
		info, e := root.Lstat(path)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if e == nil && !info.Mode().IsRegular() {
			return errors.New("managed SSH destination must be a regular file")
		}
		s := snapshot{path: path, exists: e == nil}
		if s.exists {
			s.data, err = root.ReadFile(path)
			if err != nil {
				return err
			}
			s.mode = info.Mode().Perm()
		}
		snapshots = append(snapshots, s)
	}
	defer func() {
		if resultErr != nil {
			var rollbackErr error
			for _, s := range snapshots {
				if s.exists {
					rollbackErr = errors.Join(rollbackErr, writeSSHFile(root, s.path, s.data, s.mode))
				} else {
					if e := root.Remove(s.path); e != nil && !os.IsNotExist(e) {
						rollbackErr = errors.Join(rollbackErr, e)
					}
				}
			}
			if rollbackErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("restoring previous managed files failed: %w", rollbackErr))
				return
			}
			if _, e := command("/usr/bin/systemctl", "reload-or-restart", "ssh.service"); e != nil {
				resultErr = errors.Join(resultErr, errors.New("previous managed files restored, but SSH reload failed; check ssh.service"))
			}
		}
	}()
	lines := []string{}
	for _, k := range v.Keys {
		lines = append(lines, k.PublicKey+" lidza-deploy:"+k.ID+" "+k.Label)
	}
	if err = writeSSHFile(root, sshKeyPath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		return err
	}
	config := "# Managed by Lidza Deploy. Existing key sources are preserved.\nAuthorizedKeysFile " + paths + "\n"
	if err = writeSSHFile(root, sshConfigPath, []byte(config), 0644); err != nil {
		return err
	}
	if v.Sudo {
		if err = writeSSHFile(root, sshSudoPath, []byte("# Managed by Lidza Deploy\ndeploy ALL=(ALL:ALL) NOPASSWD: ALL\n"), 0440); err != nil {
			return err
		}
		if _, err = command("/usr/sbin/visudo", "-c", "-f", "/"+sshSudoPath); err != nil {
			return errors.New("sudo configuration validation failed")
		}
	} else {
		if err = root.Remove(sshSudoPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if _, err = command("/usr/sbin/sshd", "-t"); err != nil {
		return errors.New("SSH configuration validation failed")
	}
	effective, err := command("/usr/sbin/sshd", "-T", "-C", "user=deploy,host=localhost,addr=127.0.0.1")
	if err != nil || !strings.Contains(string(effective), sshAuthorizedPath) {
		return errors.New("existing SSH configuration overrides managed keys")
	}
	if _, err = command("/usr/bin/systemctl", "reload-or-restart", "ssh.service"); err != nil {
		return errors.New("could not reload SSH service")
	}
	return nil
}
