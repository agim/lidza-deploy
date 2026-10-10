package agent

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/crypto/ssh"
)

type SSHKey struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
}
type SSHAccess struct {
	Keys      []SSHKey `json:"keys"`
	Sudo      bool     `json:"sudo"`
	State     string   `json:"state"`
	Message   string   `json:"message,omitempty"`
	Supported bool     `json:"supported"`
	User      string   `json:"user"`
}

func normalizeSSHKey(label, key string) (SSHKey, error) {
	label = strings.TrimSpace(label)
	if label == "" || len(label) > 80 || strings.IndexFunc(label, unicode.IsControl) >= 0 {
		return SSHKey{}, errors.New("enter a key label of 1–80 characters")
	}
	if len(key) > 8192 {
		return SSHKey{}, errors.New("public key is too large")
	}
	pk, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(key)))
	if err != nil || len(options) > 0 || len(strings.TrimSpace(string(rest))) > 0 {
		return SSHKey{}, errors.New("paste one SSH public key without authorized_keys options; private keys are not accepted")
	}
	switch pk.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoRSA, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoSKED25519, ssh.KeyAlgoSKECDSA256:
	default:
		return SSHKey{}, errors.New("use an Ed25519, RSA or ECDSA public key")
	}
	if c, ok := pk.(ssh.CryptoPublicKey); ok {
		if r, ok := c.CryptoPublicKey().(*rsa.PublicKey); ok && r.N.BitLen() < 2048 {
			return SSHKey{}, errors.New("RSA keys must be at least 2048 bits")
		}
	}
	sum := sha256.Sum256(pk.Marshal())
	return SSHKey{ID: hex.EncodeToString(sum[:]), Label: label, PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))), Fingerprint: ssh.FingerprintSHA256(pk)}, nil
}
func validateSSHAccess(v SSHAccess) (SSHAccess, error) {
	if len(v.Keys) > 50 {
		return v, errors.New("at most 50 SSH keys")
	}
	seen := map[string]bool{}
	for i, k := range v.Keys {
		clean, err := normalizeSSHKey(k.Label, k.PublicKey)
		if err != nil {
			return v, err
		}
		if seen[clean.ID] {
			return v, errors.New("duplicate SSH public key")
		}
		seen[clean.ID] = true
		v.Keys[i] = clean
	}
	if v.Keys == nil {
		v.Keys = []SSHKey{}
	}
	v.User = "deploy"
	return v, nil
}
func readSSHFile(root *os.Root, path string, out any) error {
	f, err := root.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil {
		return err
	}
	if err = d.Decode(&struct{}{}); err != io.EOF {
		return errors.New("invalid SSH request")
	}
	return nil
}
func writeSSHFile(root *os.Root, path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp-" + randomSSHNonce()
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	// The root helper runs with umask 077; managed authorized keys must still
	// be readable by deploy, while request/status files stay private.
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	return root.Rename(tmp, path)
}
func randomSSHNonce() string { return rand.Text() }
func (m *Manager) sshAccess() SSHAccess {
	v := SSHAccess{User: "deploy", Keys: []SSHKey{}, State: "idle"}
	if root, err := os.OpenRoot(m.cfg.DataDir); err == nil {
		defer root.Close()
		_ = readSSHFile(root, "ssh-access/status.json", &v)
		var pending SSHAccess
		if readSSHFile(root, "ssh-access/request.json", &pending) == nil {
			v = pending
			v.State = "queued"
			v.Message = "SSH access change queued"
		}
	}
	helper, err := os.ReadFile("/usr/local/libexec/lidza-agent-upgrade")
	v.Supported = err == nil && strings.Contains(string(helper), "# lidza-ssh-access-v1") && m.cfg.DataDir == "/var/lib/lidza-agent" && runtime.GOOS == "linux"
	v.User = "deploy"
	if v.Keys == nil {
		v.Keys = []SSHKey{}
	}
	return v
}
func (m *Manager) queueSSHAccess(v SSHAccess) error {
	if m.upgradePending() {
		return errors.New("wait for the current server update")
	}
	old := m.sshAccess()
	if old.State == "queued" || old.State == "applying" {
		return errors.New("an SSH access change is already pending")
	}
	v, err := validateSSHAccess(v)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(m.cfg.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err = root.MkdirAll("ssh-access", 0700); err != nil {
		return err
	}
	if err = root.MkdirAll("upgrade", 0700); err != nil {
		return err
	}
	v.State = "queued"
	v.Message = ""
	data, _ := json.Marshal(v)
	if err = writeSSHFile(root, "ssh-access/request.json", data, 0600); err != nil {
		return err
	}
	if err = writeSSHFile(root, "upgrade/request", []byte("ssh-keys\n"), 0600); err != nil {
		root.Remove("ssh-access/request.json")
		return err
	}
	return nil
}
func (m *Manager) sshRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/ssh-access", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, m.sshAccess()) })
	mux.HandleFunc("POST /v1/ssh-access/keys", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Label     string `json:"label"`
			PublicKey string `json:"public_key"`
		}
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		k, err := normalizeSSHKey(in.Label, in.PublicKey)
		if err != nil {
			Fail(w, 400, err)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		v := m.sshAccess()
		if !v.Supported {
			Fail(w, 409, errors.New("update this agent and its managed CLI helper to enable SSH access"))
			return
		}
		if slices.ContainsFunc(v.Keys, func(old SSHKey) bool { return old.ID == k.ID }) {
			Fail(w, 409, errors.New("this public key is already added"))
			return
		}
		v.Keys = append(v.Keys, k)
		if err = m.queueSSHAccess(v); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 202, map[string]string{"status": "queued"})
	})
	mux.HandleFunc("DELETE /v1/ssh-access/keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		v := m.sshAccess()
		if !v.Supported {
			Fail(w, 409, errors.New("managed SSH access is unavailable"))
			return
		}
		i := slices.IndexFunc(v.Keys, func(k SSHKey) bool { return k.ID == r.PathValue("key") })
		if i < 0 {
			http.NotFound(w, r)
			return
		}
		v.Keys = slices.Delete(v.Keys, i, i+1)
		if err := m.queueSSHAccess(v); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 202, map[string]string{"status": "queued"})
	})
	mux.HandleFunc("PATCH /v1/ssh-access", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Sudo bool `json:"sudo"`
		}
		if err := Decode(w, r, &in); err != nil {
			Fail(w, 400, err)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		v := m.sshAccess()
		if !v.Supported {
			Fail(w, 409, errors.New("managed SSH access is unavailable"))
			return
		}
		v.Sudo = in.Sudo
		if err := m.queueSSHAccess(v); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 202, map[string]string{"status": "queued"})
	})
}
