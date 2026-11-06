package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMailHost is a host whose Postfix holds port 25 on every address until
// it is bound to loopback and restarted.
type fakeMailHost struct {
	mu        sync.Mutex
	root      string
	inet      string
	relayhost string
	bound     bool // master holds 25/tcp publicly
	restarts  int
	commands  []string
	sticky    bool // the port stays bound after a restart
	noPostfix bool
	smtpd     bool // master.cf's smtp listener is on
	disabled  bool // postconf -M# smtp/inet ran
}

func newFakeMailHost(t *testing.T) *fakeMailHost {
	t.Helper()
	h := &fakeMailHost{root: t.TempDir(), inet: "all", bound: true}
	if err := os.MkdirAll(filepath.Join(h.root, "etc/postfix"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.root, "var/log"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.root, postfixMainCF), []byte("inet_interfaces = all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.root, postfixMasterCF), []byte("smtp      inet  n       -       y       -       -       smtpd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.smtpd = true
	previousCommand, previousPath := hostCommand, hostPath
	previousTimeout, previousInterval := hostMailVerifyTimeout, hostMailVerifyInterval
	t.Cleanup(func() {
		hostCommand, hostPath = previousCommand, previousPath
		hostMailVerifyTimeout, hostMailVerifyInterval = previousTimeout, previousInterval
	})
	hostMailVerifyTimeout, hostMailVerifyInterval = 50*time.Millisecond, time.Millisecond
	hostPath = func(path string) string { return filepath.Join(h.root, path) }
	hostCommand = h.run
	return h
}

func (h *fakeMailHost) run(_ context.Context, name string, args ...string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	joined := strings.Join(args, " ")
	h.commands = append(h.commands, name+" "+joined)
	switch {
	case name == "ss" && strings.Contains(joined, "-t"):
		out := "LISTEN 0 4096 0.0.0.0:22 0.0.0.0:* users:((\"sshd\",pid=700,fd=3))\n"
		switch {
		case h.bound:
			out += "LISTEN 0 100 0.0.0.0:25 0.0.0.0:* users:((\"master\",pid=1234,fd=13))\nLISTEN 0 100 [::]:25 [::]:* users:((\"master\",pid=1234,fd=14))\n"
		case h.smtpd:
			out += "LISTEN 0 100 127.0.0.1:25 0.0.0.0:* users:((\"master\",pid=1300,fd=13))\n"
		}
		return out, nil
	case name == "ss":
		return "", nil
	case name == "postconf" && h.noPostfix:
		return "postconf: not found", errors.New("exit status 127")
	case name == "postconf" && args[0] == "-h":
		values := map[string]string{"inet_interfaces": h.inet, "relayhost": h.relayhost}
		var out []string
		for _, key := range args[1:] {
			out = append(out, values[key])
		}
		return strings.Join(out, "\n") + "\n", nil
	case name == "postconf" && args[0] == "-M#":
		if args[1] == "smtp/inet" {
			h.disabled = true
		}
		return "", nil
	case name == "postconf" && args[0] == "-e":
		key, value, _ := strings.Cut(args[1], " = ")
		if key == "inet_interfaces" {
			h.inet = value
		}
		return "", nil
	case name == "systemctl" && joined == "restart postfix":
		h.restarts++
		h.bound = h.sticky || h.inet != "loopback-only"
		h.smtpd = h.sticky || !h.disabled
		return "", nil
	case name == "journalctl":
		return "", errors.New("no journal")
	}
	return "", errors.New("unexpected command " + name + " " + joined)
}

func (h *fakeMailHost) writeLog(t *testing.T, name, content string, modified time.Time) {
	t.Helper()
	path := filepath.Join(h.root, "var/log", name)
	data := []byte(content)
	if strings.HasSuffix(name, ".gz") {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		gz.Write(data)
		gz.Close()
		data = buf.Bytes()
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func mailBody(in HostMailLoopbackRequest) *bytes.Reader {
	raw, _ := json.Marshal(in)
	return bytes.NewReader(raw)
}

func syslogStamp(at time.Time) string { return at.Format("Jan _2 15:04:05") }

func TestHostMailProbeReportsPostfixEvidenceFromTheMailLogs(t *testing.T) {
	h := newFakeMailHost(t)
	now := time.Now()
	recent := syslogStamp(now.Add(-2 * time.Hour))
	old := syslogStamp(now.Add(-10 * 24 * time.Hour))
	// Local cron mail only: loopback connections and local deliveries.
	h.writeLog(t, "mail.log", strings.Join([]string{
		recent + " host postfix/smtpd[100]: connect from localhost[127.0.0.1]",
		recent + " host postfix/smtpd[100]: disconnect from localhost[127.0.0.1] ehlo=1 quit=1 commands=2",
		recent + " host postfix/local[101]: 4F2A1: to=<root@host>, relay=local, delay=0.01, status=sent (delivered to mailbox)",
		recent + " host postfix/pickup[102]: 4F2A1: uid=0 from=<root>",
	}, "\n")+"\n", now)
	// A rotated log with an outside connection from before the window.
	h.writeLog(t, "mail.log.2.gz", old+" host postfix/smtpd[90]: connect from unknown[198.51.100.7]\n", now.Add(-3*24*time.Hour))
	listening := moveListeningPorts(context.Background())
	mail := probeHostMail(context.Background(), listening)
	if mail == nil || mail.MTA != "postfix" || mail.Process != "master" || mail.InetInterfaces != "all" || len(mail.Ports) != 1 || mail.Ports[0] != (StackPort{25, "tcp"}) {
		t.Fatalf("probe %+v", mail)
	}
	if !mail.LogsRead || mail.ExternalConnections7d != 0 || mail.RelayedOut7d != 0 || mail.LogSource != "/var/log/mail.log" {
		t.Fatalf("local-only postfix: %+v", mail)
	}
	// Outside connections (also in a compressed rotation, also through the
	// submission service) and mail sent to other servers count.
	h.writeLog(t, "mail.log.1", strings.Join([]string{
		recent + " host postfix/smtpd[200]: connect from mail-ed1.example.net[203.0.113.4]",
		now.Add(-time.Hour).Format(time.RFC3339Nano) + " host postfix/submission/smtpd[201]: connect from unknown[2001:db8::4]",
		recent + " host postfix/smtp[202]: 8AB1: to=<ops@example.com>, relay=mx.example.com[192.0.2.9]:25, delay=1, status=sent (250 ok)",
		recent + " host postfix/smtp[203]: 8AB2: to=<ops@example.com>, relay=mx.example.com[192.0.2.9]:25, delay=1, status=deferred (timeout)",
	}, "\n")+"\n", now)
	h.writeLog(t, "mail.log.3.gz", recent+" host postfix/smtpd[300]: connect from unknown[198.51.100.8]\n", now.Add(-time.Hour))
	h.relayhost = "[smtp.example.net]:587"
	mail = probeHostMail(context.Background(), listening)
	if mail.ExternalConnections7d != 3 || mail.RelayedOut7d != 1 || mail.Relayhost != "[smtp.example.net]:587" {
		t.Fatalf("evidence %+v", mail)
	}
	raw, _ := json.Marshal(mail)
	for _, want := range []string{`"mta":"postfix"`, `"external_connections_7d":3`, `"relayed_out_7d":1`, `"inet_interfaces":"all"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("json %s lacks %s", raw, want)
		}
	}
}

func TestHostMailProbeNamesOtherHoldersOnly(t *testing.T) {
	newFakeMailHost(t)
	if probeHostMail(context.Background(), []ListeningPort{{22, "tcp", "sshd"}, {25, "udp", "x"}}) != nil {
		t.Fatal("no public mail port, no probe")
	}
	if probeHostMail(context.Background(), nil) != nil {
		t.Fatal("unknown ports, no probe")
	}
	for process, mta := range map[string]string{"exim4": "exim", "sendmail-mta": "sendmail", "smtpd": ""} {
		mail := probeHostMail(context.Background(), []ListeningPort{{25, "tcp", process}, {587, "tcp", process}})
		if mail == nil || mail.MTA != mta || mail.Process != process || len(mail.Ports) != 2 || mail.InetInterfaces != "" || mail.LogsRead {
			t.Fatalf("%s: %+v", process, mail)
		}
	}
}

func TestHostMailProbeWithoutLogsIsIncomplete(t *testing.T) {
	newFakeMailHost(t)
	mail := probeHostMail(context.Background(), []ListeningPort{{25, "tcp", "master"}})
	if mail == nil || mail.MTA != "postfix" || mail.LogsRead {
		t.Fatalf("no mail log and no journal: the evidence is unknown: %+v", mail)
	}
}

func TestHostMailLoopbackBacksUpRestartsVerifiesAndIsIdempotent(t *testing.T) {
	h := newFakeMailHost(t)
	m := testManager(t, &fakeRuntime{})
	w := moveRequest(t, Handler(m), "POST", "/v1/host/mail/loopback", mailBody(HostMailLoopbackRequest{Ports: []StackPort{{25, "tcp"}}, Trigger: "move", Reference: "move mv-1"}), nil)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var result HostMailLoopbackResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !strings.HasPrefix(result.Backup, postfixMainCF+".lidza-") || h.inet != "loopback-only" || h.restarts != 1 {
		t.Fatalf("result %+v inet %q restarts %d", result, h.inet, h.restarts)
	}
	if backup, err := os.ReadFile(filepath.Join(h.root, result.Backup)); err != nil || string(backup) != "inet_interfaces = all\n" {
		t.Fatalf("backup %q %v", backup, err)
	}
	m.mu.Lock()
	jobs := m.hostUpdatesStateLocked().Jobs
	m.mu.Unlock()
	if len(jobs) != 1 || jobs[0].Kind != hostMailLoopbackKind || jobs[0].Trigger != "move" || jobs[0].Status != "succeeded" ||
		!strings.Contains(jobs[0].Output, "move mv-1") || !strings.Contains(jobs[0].Output, "postconf -e inet_interfaces = loopback-only") || !strings.Contains(jobs[0].Output, "postconf -M# smtp/inet") || !strings.Contains(jobs[0].Summary, "local system mail is still delivered") {
		t.Fatalf("host job %+v", jobs)
	}
	// Again: nothing to do, no restart and no further job.
	w = moveRequest(t, Handler(m), "POST", "/v1/host/mail/loopback", mailBody(HostMailLoopbackRequest{Trigger: "move"}), nil)
	json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || result.Changed || h.restarts != 1 {
		t.Fatalf("second call %d %+v restarts %d", w.Code, result, h.restarts)
	}
	m.mu.Lock()
	jobs = m.hostUpdatesStateLocked().Jobs
	m.mu.Unlock()
	if len(jobs) != 1 {
		t.Fatalf("an idempotent call records nothing: %+v", jobs)
	}
}

func TestHostMailLoopbackFailsWhenThePortStaysBound(t *testing.T) {
	h := newFakeMailHost(t)
	h.sticky = true
	m := testManager(t, &fakeRuntime{})
	w := moveRequest(t, Handler(m), "POST", "/v1/host/mail/loopback", mailBody(HostMailLoopbackRequest{Trigger: "move"}), nil)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "25/tcp (master) is still bound") {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	m.mu.Lock()
	jobs := m.hostUpdatesStateLocked().Jobs
	m.mu.Unlock()
	if len(jobs) != 1 || jobs[0].Status != "failed" {
		t.Fatalf("failed job %+v", jobs)
	}
	// Without Postfix the request is refused before anything changes.
	h.noPostfix = true
	w = moveRequest(t, Handler(m), "POST", "/v1/host/mail/loopback", nil, nil)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "Postfix is not available") {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}
