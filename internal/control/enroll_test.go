package control

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/buildinfo"
	"github.com/agim/lidza-deploy/internal/updates"
	"github.com/agim/lidza/packs/audit"
	"golang.org/x/crypto/ssh"
)

// enrollFixture is a signed-channel panel plus a new server whose agent
// answers at https://server1.example.com once it is "ready".
type enrollFixture struct {
	*channelFixture
	mu         sync.Mutex
	ready      bool
	audited    []string
	agentToken string
}

func newEnrollFixture(t *testing.T) *enrollFixture {
	t.Helper()
	f := &enrollFixture{channelFixture: newChannelFixture(t), agentToken: strings.Repeat("b", 64)}
	if w := f.post(t, "channel", `{"repository":"acme/deploy-private"}`); w.Code != 200 {
		t.Fatal("channel configuration failed", w.Code, w.Body)
	}
	savedAudit := enrollmentAudit
	t.Cleanup(func() { enrollmentAudit = savedAudit })
	enrollmentAudit = func(_ context.Context, event audit.Event) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.audited = append(f.audited, event.Action+" "+event.Resource)
		return nil
	}
	fleet := f.control.client.Transport
	f.control.client = &http.Client{Transport: updateTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "server1.example.com" {
			return fleet.RoundTrip(r)
		}
		f.mu.Lock()
		ready := f.ready
		f.mu.Unlock()
		if !ready {
			return nil, fmt.Errorf("dial tcp: no route to host")
		}
		code, body := 200, "[]"
		if r.Header.Get("Authorization") != "Bearer "+f.agentToken {
			code, body = 401, `{"error":"unauthorized"}`
		}
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	return f
}

func (f *enrollFixture) create(t *testing.T, body string) (*httptest.ResponseRecorder, Enrollment) {
	t.Helper()
	w := httptest.NewRecorder()
	f.control.createEnrollment(w, httptest.NewRequest("POST", "/api/control/server-enrollments", strings.NewReader(body)))
	var e Enrollment
	json.Unmarshal(w.Body.Bytes(), &e)
	return w, e
}

func (f *enrollFixture) public(method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.SetPathValue("token", token)
	w := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(path, "/install.sh"):
		f.control.enrollmentScript(w, r)
	case strings.HasSuffix(path, "/complete"):
		f.control.completeEnrollment(w, r)
	default:
		r.SetPathValue("arch", path[strings.LastIndex(path, "/")+1:])
		f.control.enrollmentBundle(w, r)
	}
	return w
}

func enrollmentToken(t *testing.T, command string) string {
	t.Helper()
	_, rest, ok := strings.Cut(command, "/enroll/")
	token, _, ok2 := strings.Cut(rest, "/install.sh")
	if !ok || !ok2 || len(token) != 64 {
		t.Fatalf("no enrollment token in command %q", command)
	}
	return token
}

func TestEnrollmentConnectsANewServerWithOneCommand(t *testing.T) {
	f := newEnrollFixture(t)
	c := f.control
	for _, bad := range []string{
		`{"id":"Bad ID","name":"One","hostname":"server1.example.com"}`,
		`{"id":"one","name":"One","hostname":"server1"}`,
		`{"id":"one","name":"One","hostname":"server1.example.com'; rm -rf /"}`,
		`{"id":"one","name":"One","hostname":"server1.example.com","email":"x'y@example.com"}`,
		`{"id":"one","name":"","hostname":"server1.example.com"}`,
	} {
		if w, _ := f.create(t, bad); w.Code != 400 {
			t.Fatalf("invalid enrollment accepted: %s -> %d %s", bad, w.Code, w.Body)
		}
	}
	if w, _ := f.create(t, `{"id":"remote","name":"One","hostname":"server1.example.com"}`); w.Code != 409 {
		t.Fatal("an existing server ID was accepted", w.Code)
	}
	w, e := f.create(t, `{"id":"one","name":"Server One","hostname":"Server1.Example.com","email":"ops@example.com"}`)
	if w.Code != 201 || e.State != "waiting" || e.Mode != "command" || e.Hostname != "server1.example.com" ||
		!strings.HasPrefix(e.Command, "curl -fsSL https://deploy.example/enroll/") || !strings.HasSuffix(e.Command, "/install.sh | sudo sh") {
		t.Fatalf("enrollment %d %+v", w.Code, e)
	}
	token := enrollmentToken(t, e.Command)
	if strings.Contains(w.Body.String(), `"token"`) {
		t.Fatal("the enrollment view names its token field")
	}
	if w, _ := f.create(t, `{"id":"two","name":"Two","hostname":"server1.example.com"}`); w.Code != 409 {
		t.Fatal("the same hostname was enrolled twice at once", w.Code)
	}
	if w := f.public("GET", "/enroll/x/install.sh", strings.Repeat("0", 64), ""); w.Code != 404 {
		t.Fatal("an unknown token got a script", w.Code)
	}
	// The script installs the very bundle this panel verified.
	script := f.public("GET", "/enroll/x/install.sh", token, "")
	sum := sha256.Sum256(f.archive)
	for _, want := range []string{"amd64) sum=" + hex.EncodeToString(sum[:]), "https://deploy.example/enroll/" + token + "/bundle/", "--fqdn 'server1.example.com' --email 'ops@example.com'", "/enroll/" + token + "/complete"} {
		if script.Code != 200 || !strings.Contains(script.Body.String(), want) {
			t.Fatalf("install script %d lacks %q:\n%s", script.Code, want, script.Body)
		}
	}
	if strings.Contains(script.Body.String(), "github.com") {
		t.Fatal("a signed-channel panel sent the server to the published releases")
	}
	if got := f.public("GET", "/enroll/x/bundle/amd64", token, ""); got.Code != 200 || !bytes.Equal(got.Body.Bytes(), f.archive) {
		t.Fatal("the verified bundle was not served to the enrolling server", got.Code)
	}
	if got := f.public("GET", "/enroll/x/bundle/amd64", strings.Repeat("0", 64), ""); got.Code != 404 {
		t.Fatal("the bundle was served without a valid token", got.Code)
	}
	connection := `{"id":"agent-0123456789ab","name":"server1.example.com","url":"https://elsewhere.example.com","token":"` + f.agentToken + `"}`
	// Not reachable yet (DNS, firewall, certificate): the server is told to retry.
	if got := f.public("POST", "/enroll/x/complete", token, connection); got.Code != http.StatusTooEarly {
		t.Fatal("an unreachable agent was not asked to retry", got.Code, got.Body)
	}
	show := func() Enrollment {
		r := httptest.NewRequest("GET", "/api/control/server-enrollments/one", nil)
		r.SetPathValue("id", "one")
		w := httptest.NewRecorder()
		c.showEnrollment(w, r)
		var e Enrollment
		json.Unmarshal(w.Body.Bytes(), &e)
		return e
	}
	if e := show(); e.State != "connecting" || !strings.Contains(e.Message, "waiting for https://server1.example.com") {
		t.Fatalf("pending state %+v", e)
	}
	f.mu.Lock()
	f.ready = true
	f.mu.Unlock()
	if got := f.public("POST", "/enroll/x/complete", token, strings.Replace(connection, f.agentToken, strings.Repeat("c", 64), 1)); got.Code != 409 {
		t.Fatal("a token the agent at that hostname rejects was saved", got.Code)
	}
	c.enroll.mu.Lock()
	c.enroll.byID["one"].State = "connecting" // the operator runs the command again
	c.enroll.mu.Unlock()
	if got := f.public("POST", "/enroll/x/complete", token, connection); got.Code != 201 {
		t.Fatal("a reachable agent was not connected", got.Code, got.Body)
	}
	var saved *Server
	for _, s := range c.servers() {
		if s.ID == "one" {
			saved = &s
		}
	}
	// The address is the hostname the operator named, never the one the server sent.
	if saved == nil || saved.URL != "https://server1.example.com" || saved.Name != "Server One" || saved.Token != f.agentToken {
		t.Fatalf("saved server %+v", saved)
	}
	if len(f.audited) != 1 || f.audited[0] != "control.server.enrolled server/one" {
		t.Fatalf("enrollment audit entries %v", f.audited)
	}
	if e := show(); e.State != "connected" || e.Command != "" {
		t.Fatalf("finished enrollment still shows its command: %+v", e)
	}
	// The token is spent.
	if got := f.public("GET", "/enroll/x/install.sh", token, ""); got.Code != 404 {
		t.Fatal("a spent token still serves the script", got.Code)
	}
	if got := f.public("POST", "/enroll/x/complete", token, connection); got.Code != 404 {
		t.Fatal("a spent token connected again", got.Code)
	}
}

func TestEnrollmentExpiresAndUsesPublishedReleasesWithoutASignedChannel(t *testing.T) {
	f := newEnrollFixture(t)
	now := time.Now()
	savedNow, savedTrust, savedVersion := enrollmentNow, channelTrust, buildinfo.Version
	t.Cleanup(func() { enrollmentNow, channelTrust, buildinfo.Version = savedNow, savedTrust, savedVersion })
	enrollmentNow = func() time.Time { return now }
	_, e := f.create(t, `{"id":"one","name":"One","hostname":"server1.example.com"}`)
	token := enrollmentToken(t, e.Command)
	enrollmentNow = func() time.Time { return now.Add(enrollmentLifetime + time.Minute) }
	if got := f.public("GET", "/enroll/x/install.sh", token, ""); got.Code != 404 {
		t.Fatal("an expired token still serves the script", got.Code)
	}
	// A build without pinned keys follows the project's published releases.
	enrollmentNow = func() time.Time { return now }
	channelTrust = func() updates.Trust { return updates.Trust{} }
	buildinfo.Version = "v0.2.106"
	_, e = f.create(t, `{"id":"two","name":"Two","hostname":"server2.example.com"}`)
	token = enrollmentToken(t, e.Command)
	script := f.public("GET", "/enroll/x/install.sh", token, "").Body.String()
	if !strings.Contains(script, "https://github.com/agim/lidza-deploy/releases/download/v0.2.106/lidza-agent-linux-") || strings.Contains(script, token+"/bundle/") {
		t.Fatalf("published-release script:\n%s", script)
	}
	if got := f.public("GET", "/enroll/x/bundle/amd64", token, ""); got.Code != 404 {
		t.Fatal("a build without a signed channel served a bundle", got.Code)
	}
	// A signed build whose channel is not bound has nothing it may install.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	channelTrust = func() updates.Trust { return updates.Trust{Keys: []ed25519.PublicKey{pub}} }
	f.control.mu.Lock()
	f.control.data.Updates.Channel = nil
	f.control.mu.Unlock()
	if w, _ := f.create(t, `{"id":"three","name":"Three","hostname":"server3.example.com"}`); w.Code != 409 {
		t.Fatal("an unbound signed build offered an install", w.Code, w.Body)
	}
}

// The generated script runs for real against stand-ins for curl and the
// installer: download, checksum, extraction, install, then connect with
// retries. A corrupted download must stop before anything is installed.
func TestEnrollmentScriptInstallsAndConnects(t *testing.T) {
	dir := t.TempDir()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	installer := "#!/bin/bash\necho \"installer $*\" >> \"$ENROLL_LOG\"\n"
	tw.WriteHeader(&tar.Header{Name: "install-agent.sh", Mode: 0o755, Size: int64(len(installer))})
	tw.Write([]byte(installer))
	tw.Close()
	gz.Close()
	os.WriteFile(filepath.Join(dir, "bundle.tar.gz"), archive.Bytes(), 0o644)
	sum := sha256.Sum256(archive.Bytes())
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "id"), []byte("#!/bin/sh\necho 0\n"), 0o755)
	os.WriteFile(filepath.Join(bin, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	// curl stand-in: serves the bundle, and answers the completion call with
	// 425 twice before 201.
	os.WriteFile(filepath.Join(bin, "curl"), []byte(`#!/bin/sh
out=''; prev=''; post=0
for a in "$@"; do [ "$prev" = -o ] && out=$a; [ "$a" = POST ] && post=1; prev=$a; done
if [ "$post" = 1 ]; then
 n=$(cat "$ENROLL_DIR/tries" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$ENROLL_DIR/tries"
 echo "complete $n" >> "$ENROLL_LOG"; : > "$out"
 if [ "$n" -lt 3 ]; then printf 425; else printf 201; fi
 exit 0
fi
cp "$ENROLL_DIR/$ENROLL_BUNDLE" "$out"
`), 0o755)
	e := &Enrollment{ID: "one", Hostname: "server1.example.com", Email: "ops@example.com", token: strings.Repeat("a", 64)}
	script := enrollmentInstallScript("https://deploy.example", e, enrollmentBundleSource{Sums: map[string]string{"amd64": hex.EncodeToString(sum[:]), "arm64": hex.EncodeToString(sum[:])}})
	path := filepath.Join(dir, "install.sh")
	os.WriteFile(path, []byte(script), 0o644)
	if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("script syntax: %v\n%s", err, out)
	}
	run := func(bundle string) (string, string, error) {
		os.Remove(filepath.Join(dir, "tries"))
		os.Remove(filepath.Join(dir, "log"))
		cmd := exec.Command("sh", path)
		cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "ENROLL_DIR=" + dir, "ENROLL_LOG=" + filepath.Join(dir, "log"), "ENROLL_BUNDLE=" + bundle}
		out, err := cmd.CombinedOutput()
		log, _ := os.ReadFile(filepath.Join(dir, "log"))
		return string(out), string(log), err
	}
	out, log, err := run("bundle.tar.gz")
	if err != nil || !strings.Contains(out, "Connected.") {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if want := "installer --fqdn server1.example.com --email ops@example.com\ncomplete 1\ncomplete 2\ncomplete 3\n"; log != want {
		t.Fatalf("script steps:\n%s\nwant:\n%s", log, want)
	}
	os.WriteFile(filepath.Join(dir, "tampered.tar.gz"), append(archive.Bytes(), 'x'), 0o644)
	out, log, err = run("tampered.tar.gz")
	if err == nil || !strings.Contains(out, "does not match its checksum") || log != "" {
		t.Fatalf("a tampered download was installed: %v\n%s\n%s", err, out, log)
	}
}

// sshTestServer accepts one password and runs "exec" requests through run.
func sshTestServer(t *testing.T, user, password string, run func(command string, stdin io.Reader, out io.Writer) uint32) (address, fingerprint string) {
	t.Helper()
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{PasswordCallback: func(meta ssh.ConnMetadata, given []byte) (*ssh.Permissions, error) {
		if meta.User() == user && string(given) == password {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for incoming := range chans {
					channel, requests, err := incoming.Accept()
					if err != nil {
						continue
					}
					go func() {
						defer channel.Close()
						for req := range requests {
							if req.Type != "exec" {
								req.Reply(false, nil)
								continue
							}
							var payload struct{ Command string }
							ssh.Unmarshal(req.Payload, &payload)
							req.Reply(true, nil)
							status := run(payload.Command, channel, channel)
							channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
							return
						}
					}()
				}
			}()
		}
	}()
	return listener.Addr().String(), ssh.FingerprintSHA256(signer.PublicKey())
}

func TestEnrollmentOverSSHRunsTheInstallCommandOnce(t *testing.T) {
	f := newEnrollFixture(t)
	c := f.control
	var ran struct {
		sync.Mutex
		command, stdin string
	}
	address, fingerprint := sshTestServer(t, "ops", "s3cret-pass", func(command string, stdin io.Reader, out io.Writer) uint32 {
		line := make([]byte, 64)
		n, _ := stdin.Read(line)
		ran.Lock()
		ran.command, ran.stdin = command, string(line[:n])
		ran.Unlock()
		_, rest, _ := strings.Cut(command, "/enroll/")
		token, _, _ := strings.Cut(rest, "/install.sh")
		fmt.Fprintf(out, "Downloading the agent release…\nfetching https://deploy.example/enroll/%s/bundle/amd64\n", token)
		// The installer's last step: the server reports back and is connected.
		c.setEnrollment("one", func(e *Enrollment) { e.State, e.Message = "connected", "Server connected" })
		return 0
	})
	host, port, _ := net.SplitHostPort(address)
	wait := func(id string) Enrollment {
		t.Helper()
		for i := 0; i < 200; i++ {
			c.enroll.mu.Lock()
			e := c.enroll.byID[id].view()
			busy := c.enroll.byID[id].cancel != nil
			c.enroll.mu.Unlock()
			if !busy {
				return e
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("SSH enrollment did not finish")
		return Enrollment{}
	}
	body := func(id, password, hostKey string) string {
		return fmt.Sprintf(`{"id":%q,"name":"Server","hostname":"%s.example.com","ssh":{"address":%q,"port":%s,"user":"ops","password":%q,"host_key":%q}}`, id, id, host, port, password, hostKey)
	}
	if w, _ := f.create(t, `{"id":"zero","name":"Zero","hostname":"zero.example.com","ssh":{"user":"ops"}}`); w.Code != 400 {
		t.Fatal("SSH enrollment without a password or key accepted", w.Code)
	}
	w, e := f.create(t, body("one", "s3cret-pass", ""))
	if w.Code != 201 || e.Mode != "ssh" || e.Command != "" || strings.Contains(w.Body.String(), "s3cret-pass") {
		t.Fatalf("SSH enrollment %d %s", w.Code, w.Body)
	}
	e = wait("one")
	ran.Lock()
	command, stdin := ran.command, ran.stdin
	ran.Unlock()
	if e.State != "connected" || e.HostKey != fingerprint {
		t.Fatalf("SSH enrollment result %+v", e)
	}
	// A non-root login runs the script through sudo, fed the password on stdin, never on the command line.
	if !strings.Contains(command, "https://deploy.example/enroll/") || !strings.Contains(command, "sudo -S -p '' sh") || strings.Contains(command, "s3cret-pass") || stdin != "s3cret-pass\n" {
		t.Fatalf("remote command %q stdin %q", command, stdin)
	}
	// The log shows the installer's output with the token removed.
	if !strings.Contains(e.Log, "Downloading the agent release") || !strings.Contains(e.Log, "/enroll/…/bundle/amd64") || strings.Contains(e.Log, "s3cret-pass") {
		t.Fatalf("enrollment log %q", e.Log)
	}
	ran.Lock()
	ran.command = ""
	ran.Unlock()
	f.create(t, body("two", "wrong-password", ""))
	if e := wait("two"); e.State != "failed" || !strings.Contains(e.Message, "SSH login was refused") {
		t.Fatalf("wrong password: %+v", e)
	}
	f.create(t, body("three", "s3cret-pass", "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"))
	if e := wait("three"); e.State != "failed" || !strings.Contains(e.Message, "different host key") || e.HostKey != fingerprint {
		t.Fatalf("host key mismatch: %+v", e)
	}
	ran.Lock()
	defer ran.Unlock()
	if ran.command != "" {
		t.Fatal("a command ran after a refused login or a host key mismatch")
	}
	if got := sshEnrollmentCommand("https://deploy.example/enroll/t/install.sh", "root", true); strings.Contains(got, "sudo") {
		t.Fatalf("root login used sudo: %s", got)
	}
}
