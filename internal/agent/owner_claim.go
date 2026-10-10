package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const ownerClaimMount = "/run/lidza-owner-claim"

type OwnerClaim struct {
	State string `json:"state"`
	Token string `json:"token,omitempty"`
}
type ownerClaimRuntime interface {
	OwnerClaim(context.Context, App) (OwnerClaim, error)
}

func (d *Docker) ownerClaimDirectory(a App) string {
	return filepath.Join(d.Root, "owner-claims", a.ID)
}
func (d *Docker) prepareOwnerClaim(ctx context.Context, a App) (string, error) {
	if !idPattern.MatchString(a.ID) {
		return "", errors.New("invalid application")
	}
	path := d.ownerClaimDirectory(a)
	if err := os.MkdirAll(path, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("invalid private setup directory")
	}
	// The agent runs as deploy, which cannot chown host files to the app UID.
	// Docker performs this narrowly scoped operation on the private bind mount.
	if err := dockerStream(ctx, nil, io.Discard, nil, "run", "--rm", "--network", "none", "--user", "0:0", "--read-only", "--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "FOWNER", "--security-opt", "no-new-privileges", "--memory", "64m", "--pids-limit", "32", "--mount", "type=bind,src="+path+",dst=/claim", cacheImage, "sh", "-c", "chown 65532:65532 /claim && chmod 700 /claim"); err != nil {
		return "", errors.New("cannot initialize private setup directory through Docker; check agent Docker access")
	}
	return path, nil
}
func (d *Docker) OwnerClaim(ctx context.Context, a App) (OwnerClaim, error) {
	if a.Env["AUTH_OWNER_CLAIM"] != "true" {
		return OwnerClaim{State: "disabled"}, nil
	}
	root, err := os.OpenRoot(d.ownerClaimDirectory(a))
	if err != nil && !os.IsPermission(err) {
		return OwnerClaim{}, errors.New("setup status unavailable; reload the app with the current agent")
	}
	if root != nil {
		defer root.Close()
	}
	read := func(name string, max int) ([]byte, error) {
		if root == nil {
			return d.readOwnerClaimFile(ctx, a, name, max)
		}
		f, e := root.Open(name)
		if e != nil {
			if os.IsPermission(e) {
				return d.readOwnerClaimFile(ctx, a, name, max)
			}
			return nil, e
		}
		defer f.Close()
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Size() > int64(max) {
			return nil, errors.New("invalid setup file")
		}
		b, e := io.ReadAll(io.LimitReader(f, int64(max+1)))
		if e != nil || len(b) > max {
			return nil, errors.New("invalid setup file")
		}
		return b, nil
	}
	b, err := read("status.json", 4096)
	if err != nil {
		return OwnerClaim{}, errors.New("setup status unavailable; check app startup and framework version")
	}
	var out OwnerClaim
	if json.Unmarshal(b, &out) != nil || (out.State != "claimed" && out.State != "unclaimed") {
		return OwnerClaim{}, errors.New("invalid setup status")
	}
	out.Token = "" // The status file never authorizes arbitrary token content.
	if out.State == "claimed" {
		return out, nil
	}
	token := a.Env["AUTH_OWNER_CLAIM_TOKEN"]
	if token == "" {
		b, err = read("token", 4096)
		if err != nil {
			return OwnerClaim{}, errors.New("setup token unavailable; it may already be claimed")
		}
		token = strings.TrimSpace(string(b))
	}
	if len(token) < 32 || len(token) > 4096 {
		return OwnerClaim{}, errors.New("invalid setup token")
	}
	out.Token = token
	return out, nil
}

func (d *Docker) readOwnerClaimFile(ctx context.Context, a App, name string, max int) ([]byte, error) {
	if !idPattern.MatchString(a.ID) || (name != "status.json" && name != "token") || max != 4096 {
		return nil, errors.New("invalid setup file")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Read as the application's UID. The deploy account cannot directly read
	// its 0700 directory / 0600 files. Never route token bytes into deployment logs.
	script := `test ! -L /claim/` + name + ` && test -f /claim/` + name + ` && test "$(wc -c < /claim/` + name + `)" -le 4096 && cat /claim/` + name
	c := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--user", "65532:65532", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "64m", "--pids-limit", "32", "--log-driver", "none", "--mount", "type=bind,src="+d.ownerClaimDirectory(a)+",dst=/claim,readonly", cacheImage, "sh", "-c", script)
	c.WaitDelay = 5 * time.Second
	var output limitedBuffer
	c.Stdout = &output
	c.Stderr = io.Discard
	if err := c.Run(); err != nil || len(output.String()) > max {
		return nil, errors.New("setup file unavailable through Docker")
	}
	return []byte(output.String()), nil
}
func (m *Manager) ownerClaimRoute(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	m.mu.Lock()
	a, ok := m.data.Apps[r.PathValue("id")]
	a.Env = maps.Clone(a.Env)
	m.mu.Unlock()
	if !ok || a.Retiring || a.Current == nil {
		Fail(w, 409, errors.New("app has no active release"))
		return
	}
	runtime, ok := m.runtime.(ownerClaimRuntime)
	if !ok {
		Fail(w, 409, errors.New("agent does not support setup tokens"))
		return
	}
	out, err := runtime.OwnerClaim(r.Context(), a)
	if err != nil {
		Fail(w, 409, err)
		return
	}
	JSON(w, 200, out)
}
