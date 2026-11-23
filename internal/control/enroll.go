package control

// Server enrollment: connecting a new server without building an installer
// or copying an agent token by hand. An operator names the server and its
// hostname; the panel issues a one-time token and either hands out one command
// to run on the server or runs that command itself over SSH (enroll_ssh.go).
// The command installs the agent release this panel can update afterwards,
// then reports the agent's connection back; the panel checks that it can
// reach the agent at the hostname the operator gave before saving it.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/buildinfo"
	"github.com/agim/lidza/packs/audit"
)

const (
	enrollmentLifetime = time.Hour
	enrollmentLogLimit = 16 << 10
	// enrollmentReleases is where a build without a signed channel gets the
	// agent release matching the panel.
	enrollmentReleases = "https://github.com/agim/lidza-deploy/releases"
)

var (
	enrollmentID       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
	enrollmentHostname = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	enrollmentEmail    = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]+$`)
	enrollmentRelease  = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	enrollmentNow      = time.Now
	// enrollmentAudit records the audit entry of a finished enrollment; unit
	// tests without the framework's audit service replace it.
	enrollmentAudit = func(ctx context.Context, event audit.Event) error { return audit.From(ctx).Record(ctx, event) }
)

// Enrollment is one pending or finished connection of a new server. It lives
// in memory only: the token is a credential and the record is useless after a
// restart of the panel, when the operator simply starts again.
type Enrollment struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Hostname string    `json:"hostname"`
	Email    string    `json:"email,omitempty"`
	Mode     string    `json:"mode"`  // "command" or "ssh"
	State    string    `json:"state"` // waiting, installing, connecting, connected, failed
	Message  string    `json:"message,omitempty"`
	Log      string    `json:"log,omitempty"`
	HostKey  string    `json:"host_key,omitempty"`
	Command  string    `json:"command,omitempty"`
	Expires  time.Time `json:"expires"`
	token    string
	cancel   context.CancelFunc
}

type enrollments struct {
	mu   sync.Mutex
	byID map[string]*Enrollment
}

// enrollmentInput is the operator's request; SSH is absent for the command mode.
type enrollmentInput struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Hostname string         `json:"hostname"`
	Email    string         `json:"email"`
	SSH      *enrollmentSSH `json:"ssh"`
}

func (c *Control) enrollmentRoutes(handle func(string, http.HandlerFunc)) {
	handle("POST /api/control/server-enrollments", c.createEnrollment)
	handle("GET /api/control/server-enrollments/{id}", c.showEnrollment)
	handle("DELETE /api/control/server-enrollments/{id}", c.cancelEnrollment)
}

// enrollmentPublicRoutes are reached by the new server itself, which has no
// session: the one-time token in the path is the credential.
func (c *Control) enrollmentPublicRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /enroll/{token}/install.sh", c.enrollmentScript)
	mux.HandleFunc("GET /enroll/{token}/bundle/{arch}", c.enrollmentBundle)
	mux.HandleFunc("POST /enroll/{token}/complete", c.panelReadOnlyGuard("POST /enroll/{token}/complete", c.completeEnrollment))
}

func (c *Control) publicOrigin() string { return strings.TrimSuffix(c.cfg.PublicURL, "/") }

func (c *Control) createEnrollment(w http.ResponseWriter, r *http.Request) {
	var in enrollmentInput
	if err := agent.Decode(w, r, &in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	in.Name, in.Hostname, in.Email = strings.TrimSpace(in.Name), strings.ToLower(strings.TrimSpace(in.Hostname)), strings.TrimSpace(in.Email)
	switch {
	case !enrollmentID.MatchString(in.ID):
		agent.Fail(w, 400, errors.New("server ID must be lowercase letters, digits and dashes, starting with a letter"))
		return
	case in.Name == "" || len(in.Name) > 100:
		agent.Fail(w, 400, errors.New("display name is required (at most 100 characters)"))
		return
	case len(in.Hostname) > 253 || !enrollmentHostname.MatchString(in.Hostname):
		agent.Fail(w, 400, errors.New("hostname must be a DNS name such as server1.example.com"))
		return
	case in.Email != "" && !enrollmentEmail.MatchString(in.Email):
		agent.Fail(w, 400, errors.New("contact email is not valid"))
		return
	case !strings.HasPrefix(c.cfg.PublicURL, "https://"):
		agent.Fail(w, 409, errors.New("this control panel needs a public HTTPS address before a new server can reach it"))
		return
	}
	if in.SSH != nil {
		if err := in.SSH.validate(); err != nil {
			agent.Fail(w, 400, err)
			return
		}
	}
	url := "https://" + in.Hostname
	for _, s := range c.servers() {
		if s.ID == in.ID {
			agent.Fail(w, 409, errors.New("server ID already exists"))
			return
		}
		if s.URL == url {
			agent.Fail(w, 409, fmt.Errorf("%s is already connected as %s", in.Hostname, s.Name))
			return
		}
	}
	if _, err := c.enrollmentSource(); err != nil {
		agent.Fail(w, 409, err)
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		agent.Fail(w, 500, errors.New("could not create an enrollment token"))
		return
	}
	e := &Enrollment{ID: in.ID, Name: in.Name, Hostname: in.Hostname, Email: in.Email, Mode: "command", State: "waiting",
		Message: "Waiting for the command to run on the server", Expires: enrollmentNow().Add(enrollmentLifetime).UTC(), token: hex.EncodeToString(raw)}
	e.Command = "curl -fsSL " + c.publicOrigin() + "/enroll/" + e.token + "/install.sh | sudo sh"
	c.enroll.mu.Lock()
	if c.enroll.byID == nil {
		c.enroll.byID = map[string]*Enrollment{}
	}
	c.pruneEnrollmentsLocked()
	if old := c.enroll.byID[in.ID]; old != nil && old.cancel != nil {
		old.cancel()
	}
	for _, other := range c.enroll.byID {
		if other.ID != in.ID && other.Hostname == in.Hostname && other.State != "connected" && other.State != "failed" {
			c.enroll.mu.Unlock()
			agent.Fail(w, 409, fmt.Errorf("%s is already being connected as %s", in.Hostname, other.ID))
			return
		}
	}
	if in.SSH != nil {
		e.Mode, e.State, e.Message = "ssh", "installing", "Connecting over SSH"
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
		e.cancel = cancel
		go c.runSSHEnrollment(ctx, e, *in.SSH)
	}
	c.enroll.byID[in.ID] = e
	view := e.view()
	c.enroll.mu.Unlock()
	agent.JSON(w, 201, view)
}

// view is what the console sees. The command, which carries the token, is
// only part of it while a person still has to run it.
func (e *Enrollment) view() Enrollment {
	out := *e
	out.token, out.cancel = "", nil
	if e.Mode != "command" || e.State == "connected" || e.State == "failed" {
		out.Command = ""
	}
	return out
}

// Caller holds enroll.mu.
func (c *Control) pruneEnrollmentsLocked() {
	now := enrollmentNow()
	for id, e := range c.enroll.byID {
		if now.After(e.Expires.Add(enrollmentLifetime)) {
			if e.cancel != nil {
				e.cancel()
			}
			delete(c.enroll.byID, id)
		}
	}
}

func (c *Control) showEnrollment(w http.ResponseWriter, r *http.Request) {
	c.enroll.mu.Lock()
	defer c.enroll.mu.Unlock()
	e := c.enroll.byID[r.PathValue("id")]
	if e == nil {
		http.NotFound(w, r)
		return
	}
	if e.State != "connected" && e.State != "failed" && enrollmentNow().After(e.Expires) {
		e.State, e.Message = "failed", "The enrollment expired before the server connected; start again"
	}
	agent.JSON(w, 200, e.view())
}

func (c *Control) cancelEnrollment(w http.ResponseWriter, r *http.Request) {
	c.enroll.mu.Lock()
	defer c.enroll.mu.Unlock()
	e := c.enroll.byID[r.PathValue("id")]
	if e == nil {
		http.NotFound(w, r)
		return
	}
	if e.cancel != nil {
		e.cancel()
	}
	delete(c.enroll.byID, e.ID)
	agent.JSON(w, 200, map[string]string{"status": "cancelled"})
}

// enrollmentByToken finds a live enrollment. Tokens are compared in constant
// time; an expired, finished or unknown one reads as absent.
func (c *Control) enrollmentByToken(token string) *Enrollment {
	c.enroll.mu.Lock()
	defer c.enroll.mu.Unlock()
	var found *Enrollment
	for _, e := range c.enroll.byID {
		if subtle.ConstantTimeCompare([]byte(e.token), []byte(token)) == 1 {
			found = e
		}
	}
	if found == nil || found.State == "connected" || enrollmentNow().After(found.Expires) {
		return nil
	}
	return found
}

func (c *Control) setEnrollment(id string, edit func(*Enrollment)) {
	c.enroll.mu.Lock()
	defer c.enroll.mu.Unlock()
	if e := c.enroll.byID[id]; e != nil {
		edit(e)
	}
}

// enrollmentBundleSource says where a new server gets the agent release.
type enrollmentBundleSource struct {
	// Sums lists the archive checksum per architecture when the panel serves
	// its verified signed bundle itself; empty when the server downloads the
	// project's published release.
	Sums    map[string]string
	Release string // download base of the published release
}

// enrollmentSource picks the release a new server installs: on a signed
// channel the bundle this panel verified (the only build whose updates the
// agent will accept later), otherwise the published release of this panel's
// own version.
func (c *Control) enrollmentSource() (enrollmentBundleSource, error) {
	if c.channelConfigured() {
		v := c.updateSettingsSnapshot()
		if v.Bundle == nil {
			return enrollmentBundleSource{}, errors.New("no verified release is known yet; open Updates and check for releases first")
		}
		m, _, err := channelTrust().Verify([]byte(v.Bundle.Manifest), mustJSON(v.Bundle.Signature))
		if err != nil || m.Version != v.Bundle.Version {
			return enrollmentBundleSource{}, errors.New("the recorded release no longer verifies; open Updates and check for releases again")
		}
		sums := map[string]string{}
		for _, a := range m.Assets {
			sums[a.Arch] = a.SHA256
		}
		return enrollmentBundleSource{Sums: sums}, nil
	}
	if channelTrust().Configured() {
		return enrollmentBundleSource{}, errors.New("this build installs only its signed releases; bind the release channel under Updates first")
	}
	if enrollmentRelease.MatchString(buildinfo.Version) {
		return enrollmentBundleSource{Release: enrollmentReleases + "/download/" + buildinfo.Version}, nil
	}
	return enrollmentBundleSource{Release: enrollmentReleases + "/latest/download"}, nil
}

func (c *Control) enrollmentScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	e := c.enrollmentByToken(r.PathValue("token"))
	if e == nil {
		http.Error(w, "echo 'This enrollment link is unknown or has expired; start again from the control panel.' >&2; exit 1\n", http.StatusNotFound)
		return
	}
	source, err := c.enrollmentSource()
	if err != nil {
		http.Error(w, "echo 'The control panel has no release to install right now; try again from the control panel.' >&2; exit 1\n", http.StatusConflict)
		return
	}
	c.setEnrollment(e.ID, func(e *Enrollment) {
		if e.State == "waiting" {
			e.State, e.Message = "installing", "The server fetched the installer"
		}
	})
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = w.Write([]byte(enrollmentInstallScript(c.publicOrigin(), e, source)))
}

// enrollmentInstallScript is the whole of what runs on the new server. Every
// interpolated value was validated against a strict pattern when the
// enrollment was created (hostname, email) or is generated here (token, sums).
func enrollmentInstallScript(origin string, e *Enrollment, source enrollmentBundleSource) string {
	var b bytes.Buffer
	base := origin + "/enroll/" + e.token
	fmt.Fprintf(&b, `#!/bin/sh
# Connects this server (%s) to the control panel at %s.
set -eu
[ "$(id -u)" -eq 0 ] || { echo 'Run this as root (pipe it to: sudo sh).' >&2; exit 1; }
case "$(uname -m)" in x86_64|amd64) arch=amd64;; aarch64|arm64) arch=arm64;; *) echo 'Only amd64 and arm64 servers are supported.' >&2; exit 1;; esac
command -v curl >/dev/null 2>&1 || { apt-get update -q && apt-get install -y -q curl ca-certificates; }
work=$(mktemp -d /tmp/lidza-enroll.XXXXXX)
trap 'rm -rf "$work"' EXIT
fetch() { curl --proto '=https' --tlsv1.2 -fsSL --retry 3 "$1" -o "$2"; }
echo 'Downloading the agent release…'
`, e.Hostname, origin)
	if len(source.Sums) > 0 {
		b.WriteString("case \"$arch\" in\n")
		for _, arch := range []string{"amd64", "arm64"} {
			if sum := source.Sums[arch]; sum != "" {
				fmt.Fprintf(&b, " %s) sum=%s;;\n", arch, sum)
			}
		}
		b.WriteString(" *) echo \"This control panel's release has no build for $arch servers.\" >&2; exit 1;;\nesac\n")
		fmt.Fprintf(&b, "fetch '%s/bundle/'\"$arch\" \"$work/bundle.tar.gz\"\n", base)
	} else {
		fmt.Fprintf(&b, "fetch '%s/lidza-agent-linux-'\"$arch\"'.tar.gz' \"$work/bundle.tar.gz\"\n", source.Release)
		fmt.Fprintf(&b, "fetch '%s/lidza-agent-linux-'\"$arch\"'.tar.gz.sha256' \"$work/sum\"\n", source.Release)
		b.WriteString("sum=$(awk 'NR==1 {print $1}' \"$work/sum\")\n")
	}
	b.WriteString(`actual=$(sha256sum "$work/bundle.tar.gz"); actual=${actual%% *}
[ "$actual" = "$sum" ] || { echo 'The download does not match its checksum; nothing was installed.' >&2; exit 1; }
if tar -tzf "$work/bundle.tar.gz" | awk '/^\// || /(^|\/)\.\.(\/|$)/ {bad=1} END {exit !bad}'; then echo 'Unsafe archive; nothing was installed.' >&2; exit 1; fi
mkdir "$work/bundle"
tar -xzf "$work/bundle.tar.gz" -C "$work/bundle"
[ -f "$work/bundle/install-agent.sh" ] || { echo 'This release cannot set up a new server yet; update the control panel, then start again.' >&2; exit 1; }
`)
	fmt.Fprintf(&b, "bash \"$work/bundle/install-agent.sh\" --fqdn '%s'", e.Hostname)
	if e.Email != "" {
		fmt.Fprintf(&b, " --email '%s'", e.Email)
	}
	fmt.Fprintf(&b, `
echo 'Connecting this server to the control panel…'
tries=0
while :; do
 code=$(curl --proto '=https' --tlsv1.2 -sS -o "$work/reply" -w '%%{http_code}' -X POST -H 'Content-Type: application/json' --data-binary @/etc/lidza-agent/connection.json '%s/complete' || echo 000)
 case "$code" in
  201) echo 'Connected. This server now appears in the control panel.'; exit 0;;
  425|502|503|000) ;;
  *) echo "The control panel refused the connection ($code): $(cat "$work/reply" 2>/dev/null)" >&2; exit 1;;
 esac
 tries=$((tries+1))
 [ "$tries" -lt 60 ] || { echo 'The agent is installed, but the control panel cannot reach https://%s yet. Point its DNS record at this server and allow ports 80 and 443, then run this command again.' >&2; exit 1; }
 sleep 10
done
`, base, e.Hostname)
	return b.String()
}

// enrollmentBundle streams the panel's verified signed bundle to the server
// being enrolled. Builds without a signed channel never serve it: their
// servers download the published release.
func (c *Control) enrollmentBundle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	e := c.enrollmentByToken(r.PathValue("token"))
	arch := r.PathValue("arch")
	if e == nil || (arch != "amd64" && arch != "arm64") || !c.channelConfigured() {
		http.NotFound(w, r)
		return
	}
	v := c.updateSettingsSnapshot()
	if v.Bundle == nil {
		http.NotFound(w, r)
		return
	}
	path, err := c.cachedArchive(r.Context(), *v.Bundle, arch)
	if err != nil {
		agent.Fail(w, http.StatusBadGateway, errors.New("the release archive is not available right now"))
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	http.ServeFile(w, r, path)
}

// completeEnrollment receives the new agent's connection file. Only its
// token is used: the address is the hostname the operator named, and the
// panel must reach the agent there with that token before anything is saved.
func (c *Control) completeEnrollment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	e := c.enrollmentByToken(r.PathValue("token"))
	if e == nil {
		agent.Fail(w, http.StatusNotFound, errors.New("this enrollment is unknown or has expired"))
		return
	}
	var in struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := agent.Decode(w, r, &in); err != nil || len(in.Token) < 32 || len(in.Token) > 256 {
		agent.Fail(w, 400, errors.New("the agent connection file is not valid"))
		return
	}
	server := Server{ID: e.ID, Name: e.Name, URL: "https://" + e.Hostname, Token: in.Token}
	c.setEnrollment(e.ID, func(e *Enrollment) {
		e.State, e.Message = "connecting", "Agent installed; checking that the panel can reach it"
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	next := append(c.servers(), server)
	if err := validateServers(next); err != nil {
		c.setEnrollment(e.ID, func(e *Enrollment) { e.State, e.Message = "failed", err.Error() })
		agent.Fail(w, 409, err)
		return
	}
	var remote []agent.App
	if err := c.agentRequest(r, server, "GET", "/v1/apps", nil, &remote); err != nil {
		var refused *agentHTTPError
		if errors.As(err, &refused) && (refused.status == http.StatusUnauthorized || refused.status == http.StatusForbidden) {
			message := "The agent at https://" + e.Hostname + " did not accept the token this server sent; another agent answers at that hostname"
			c.setEnrollment(e.ID, func(e *Enrollment) { e.State, e.Message = "failed", message })
			agent.Fail(w, 409, errors.New(message))
			return
		}
		// DNS, firewall or the certificate is not ready yet: the server retries.
		c.setEnrollment(e.ID, func(e *Enrollment) {
			e.Message = "Agent installed; waiting for https://" + e.Hostname + " to answer (DNS, ports 80/443 and its certificate)"
		})
		agent.Fail(w, http.StatusTooEarly, errors.New("the control panel cannot reach the agent yet"))
		return
	}
	if err := c.saveServers(next); err != nil {
		agent.Fail(w, 500, err)
		return
	}
	// The operator's request was audited when the enrollment was created;
	// this records that the server it named actually joined.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := enrollmentAudit(ctx, audit.Event{Action: "control.server.enrolled", Resource: "server/" + e.ID, Scope: fleetScope, Outcome: audit.OK, Meta: map[string]string{"id": e.ID}}); err != nil {
		log.Printf("server enrollment %s: audit entry not saved: %v", e.ID, err)
	}
	c.setEnrollment(e.ID, func(e *Enrollment) { e.State, e.Message, e.Command = "connected", "Server connected", "" })
	agent.JSON(w, 201, map[string]string{"status": "connected", "id": e.ID})
}

// appendEnrollmentLog keeps the tail of the installer's output for the console.
func (c *Control) appendEnrollmentLog(id, token string, p []byte) {
	text := strings.ReplaceAll(string(p), token, "…")
	c.setEnrollment(id, func(e *Enrollment) {
		e.Log += text
		if len(e.Log) > enrollmentLogLimit {
			e.Log = e.Log[len(e.Log)-enrollmentLogLimit:]
			if i := strings.IndexByte(e.Log, '\n'); i >= 0 {
				e.Log = e.Log[i+1:]
			}
		}
	})
}
