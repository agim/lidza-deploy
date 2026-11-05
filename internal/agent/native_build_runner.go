package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// runNativeBuild runs one static-site build or post-deploy command as the
// application owner in a transient, resource-bounded systemd unit with the
// application's literal environment.
func (m *Manager) runNativeBuild(ctx context.Context, a NativeApplication, candidate, id string, args []string) (string, error) {
	memoryBytes := int64(1 << 30)
	if m.nativeBuildCommand != nil {
		return m.nativeBuildCommand(ctx, a, candidate, args)
	}
	if !regexp.MustCompile(`^[a-f0-9]{24}(-post)?$`).MatchString(id) || len(args) == 0 || strings.ContainsAny(m.cfg.Native.Root, " \t\r\n") {
		return "", errors.New("invalid native build identity or directory")
	}
	account, err := user.Lookup(m.cfg.Native.User)
	if err != nil {
		return "", errors.New("native build user is unavailable")
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil || uid <= 0 {
		return "", errors.New("native builds require a non-root user")
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(candidate, ".lidza-build-env-")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return "", err
	}
	if err = file.Chown(uid, gid); err != nil {
		file.Close()
		return "", err
	}
	searchPath := account.HomeDir + "/.asdf/shims:" + account.HomeDir + "/.asdf/bin:/usr/local/bin:/usr/bin:/bin"
	var values map[string]string
	if a.Runtime == "astro" {
		values, err = m.nativeStaticEnvironment(a)
	} else {
		values, err = m.nativeBuildEnvironment(a.Name)
	}
	if err != nil {
		file.Close()
		return "", err
	}
	for key, value := range values {
		if strings.ContainsAny(value, "\r\n\x00") {
			file.Close()
			return "", errors.New("native environment contains unsupported line breaks or null bytes; review it before deployment")
		}
		if _, err = fmt.Fprintf(file, "export %s='%s'\n", key, strings.ReplaceAll(value, "'", "'\\''")); err != nil {
			file.Close()
			return "", err
		}
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	unit := "lidza-native-build-" + id + "-" + newID()[:8] + ".service"
	inaccessible := "/run/user /run/systemd/private /run/dbus/system_bus_socket -/var/run/docker.sock"
	protectHome := "read-only"
	temporaryFilesystems := "/tmp:rw,nosuid,nodev,size=512M,mode=1777"
	// No app environment is present before entering the PID namespace. In
	// particular LD_PRELOAD/BASH_ENV cannot execute app code outside isolation.
	argv := []string{"--quiet", "--wait", "--pipe", "--collect", "--expand-environment=no", "--unit=" + unit,
		"--property=User=root", "--property=WorkingDirectory=" + candidate,
		"--property=MemoryMax=" + strconv.FormatInt(memoryBytes, 10), "--property=MemorySwapMax=0", "--property=OOMScoreAdjust=500",
		"--property=CPUQuota=50%", "--property=IOWeight=10", "--property=TasksMax=256", "--property=RuntimeMaxSec=1200",
		"--property=ProtectSystem=strict", "--property=ProtectHome=" + protectHome, "--property=ReadWritePaths=" + candidate,
		"--property=TemporaryFileSystem=" + temporaryFilesystems,
		"--property=InaccessiblePaths=" + inaccessible,
		"--property=NoNewPrivileges=yes", "--property=CapabilityBoundingSet=CAP_SYS_ADMIN CAP_SETUID CAP_SETGID CAP_SETPCAP", "--property=KillMode=control-group",
		"--setenv=HOME=" + account.HomeDir, "--setenv=PATH=" + searchPath, "--setenv=npm_config_cache=/tmp/npm",
		// The fixed root launcher creates namespaces even where unprivileged
		// user namespaces are disabled. Repository code starts only after
		// setpriv irreversibly drops uid/gid and the entire capability bounding
		// set. No host namespace policy or original service unit is modified.
		"--", "/usr/bin/unshare", "--mount", "--pid", "--fork", "--mount-proc", "--",
		"/usr/bin/setpriv", "--reuid=" + account.Uid, "--regid=" + account.Gid, "--init-groups", "--bounding-set=-all", "--inh-caps=-all", "--ambient-caps=-all", "--no-new-privs", "--",
		"/bin/sh", "-c", "set -eu; set -a; . \"$1\"; set +a; shift; exec \"$@\"", "lidza-build-env", file.Name()}
	argv = append(argv, args...)
	output, err := command(ctx, "", nil, "systemd-run", argv...)
	if ctx.Err() != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = command(cleanup, "", nil, "systemctl", "stop", unit)
	}
	return output, err
}

// Imported shell files may contain expressions. Builds accept only literal
// assignments rather than silently changing a quoted secret or evaluating code.
func (m *Manager) nativeBuildEnvironment(name string) (map[string]string, error) {
	values := map[string]string{}
	root, err := os.OpenRoot(m.cfg.Native.Root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	current, err := filepath.EvalSymlinks(filepath.Join(m.cfg.Native.Root, name, "current"))
	if err != nil || !strings.HasPrefix(current, filepath.Join(m.cfg.Native.Root, name, "releases")+string(filepath.Separator)) {
		return nil, errors.New("native environment release is unavailable")
	}
	release, err := filepath.Rel(m.cfg.Native.Root, current)
	if err != nil {
		return nil, err
	}
	for _, path := range []string{filepath.Join(release, ".env"), filepath.Join(release, ".asdf-vars"), filepath.Join(name, ".asdf-vars")} {
		file, err := root.Open(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, errors.New("native build environment is unreadable")
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 128<<10 {
			file.Close()
			return nil, errors.New("native build environment exceeds the supported literal format")
		}
		data, err := io.ReadAll(io.LimitReader(file, (128<<10)+1))
		file.Close()
		if err != nil || len(data) > 128<<10 {
			return nil, errors.New("native build environment is unreadable")
		}
		if err := parseNativeLiteralEnvironment(data, values); err != nil {
			return nil, err
		}
	}
	return values, nil
}

// parseNativeLiteralEnvironment adds KEY=value assignments from a shell-style
// environment file to values. Only literal assignments are accepted.
func parseNativeLiteralEnvironment(data []byte, values map[string]string) error {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok || !envPattern.MatchString(key) {
			return errors.New("native environment contains shell code; review it before deployment")
		}
		if regexp.MustCompile(`^'[^']*'$`).MatchString(value) || regexp.MustCompile("^\"[^\"\\\\$`]*\"$").MatchString(value) {
			value = value[1 : len(value)-1]
		} else if !regexp.MustCompile(`^[A-Za-z0-9_./:@%+,=!-]*$`).MatchString(value) {
			return errors.New("native environment contains nonliteral assignments; review them before deployment")
		}
		values[key] = value
	}
	return nil
}
