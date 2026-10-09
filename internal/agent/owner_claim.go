package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
func (d *Docker) prepareOwnerClaim(a App) (string, error) {
	if !idPattern.MatchString(a.ID) {
		return "", errors.New("invalid application")
	}
	path := d.ownerClaimDirectory(a)
	if err := os.MkdirAll(path, 0700); err != nil {
		return "", err
	}
	if err := os.Chown(path, 65532, 65532); err != nil {
		return "", errors.New("cannot prepare private setup directory")
	}
	return path, nil
}
func (d *Docker) OwnerClaim(ctx context.Context, a App) (OwnerClaim, error) {
	if a.Env["AUTH_OWNER_CLAIM"] != "true" {
		return OwnerClaim{State: "disabled"}, nil
	}
	root, err := os.OpenRoot(d.ownerClaimDirectory(a))
	if err != nil {
		return OwnerClaim{}, errors.New("setup status unavailable; reload the app with the current agent")
	}
	defer root.Close()
	read := func(name string, max int) ([]byte, error) {
		f, e := root.Open(name)
		if e != nil {
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
