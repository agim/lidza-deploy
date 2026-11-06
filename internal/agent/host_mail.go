package agent

import (
	"bufio"
	"cmp"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// The host's own mail server. Distribution images often install Postfix
// listening on every address only to deliver cron and system mail to local
// mailboxes; it then holds port 25 that a mail application wants to publish.
// The move plan asks the destination what holds a public mail port and, for
// Postfix, whether it served anyone outside the host in the last week. A
// Postfix that did not can be bound to loopback (inet_interfaces =
// loopback-only): local system mail keeps working and the port is free.

// hostMailPorts are the mail ports whose holder the probe describes.
var hostMailPorts = []int{25, 465, 587}

const (
	hostMailWindow       = 7 * 24 * time.Hour
	hostMailLogLimit     = 256 << 20 // decompressed bytes read from mail logs
	hostMailLoopbackKind = "host-mail-loopback"
	postfixMainCF        = "/etc/postfix/main.cf"
)

// MoveHostMail describes the process that holds a public mail port. MTA is
// postfix, exim, sendmail or "" (unknown, only Process is known); the other
// fields are reported for Postfix only. LogsRead reports that the mail logs
// of the last 7 days were read completely, so zero counts mean no traffic.
type MoveHostMail struct {
	Ports                 []StackPort `json:"ports"`
	Process               string      `json:"process"`
	MTA                   string      `json:"mta,omitempty"`
	InetInterfaces        string      `json:"inet_interfaces,omitempty"`
	Relayhost             string      `json:"relayhost,omitempty"`
	ExternalConnections7d int         `json:"external_connections_7d"`
	RelayedOut7d          int         `json:"relayed_out_7d"`
	LogsRead              bool        `json:"logs_read"`
	LogSource             string      `json:"log_source,omitempty"`
}

// HostMailLoopbackRequest asks the agent to bind the host's Postfix to
// loopback; Ports are the ports that must be free afterwards (default 25,
// 465 and 587), Trigger the job's origin (move or manual) and Reference a
// note for the job record, such as the move id.
type HostMailLoopbackRequest struct {
	Ports     []StackPort `json:"ports,omitempty"`
	Trigger   string      `json:"trigger,omitempty"`
	Reference string      `json:"reference,omitempty"`
}

// HostMailLoopbackResult is what the change did.
type HostMailLoopbackResult struct {
	Changed bool   `json:"changed"`
	Backup  string `json:"backup,omitempty"`
	Summary string `json:"summary"`
}

func hostMailMTA(process string) string {
	switch process {
	case "master":
		return "postfix"
	case "exim", "exim4":
		return "exim"
	case "sendmail", "sendmail-mta", "sendmail-msp":
		return "sendmail"
	}
	return ""
}

// probeHostMail describes the holder of the first bound public mail port,
// or nil when none is bound (or the ports are unknown).
func probeHostMail(ctx context.Context, listening []ListeningPort) *MoveHostMail {
	var mail *MoveHostMail
	for _, p := range listening {
		if p.Protocol != "tcp" || !slices.Contains(hostMailPorts, p.Port) {
			continue
		}
		if mail == nil {
			mail = &MoveHostMail{Process: p.Process, MTA: hostMailMTA(p.Process)}
		}
		if p.Process == mail.Process {
			mail.Ports = append(mail.Ports, p.Stack())
		}
	}
	if mail == nil || mail.MTA != "postfix" {
		return mail
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	values, err := postconfValues(ctx, "inet_interfaces", "relayhost")
	if err != nil {
		// "master" without postconf is not a Postfix we can manage.
		mail.MTA = ""
		return mail
	}
	mail.InetInterfaces, mail.Relayhost = values[0], values[1]
	stats := readPostfixLogs(ctx, time.Now())
	mail.ExternalConnections7d, mail.RelayedOut7d, mail.LogsRead, mail.LogSource = stats.external, stats.relayed, stats.complete, stats.source
	return mail
}

// postconfValues reads main.cf parameters with postconf -h, one per line in
// the order asked.
func postconfValues(ctx context.Context, names ...string) ([]string, error) {
	out, err := hostCommand(ctx, "postconf", append([]string{"-h"}, names...)...)
	if err != nil {
		return nil, fmt.Errorf("postconf: %s", strings.TrimSpace(out))
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != len(names) {
		return nil, fmt.Errorf("postconf printed %d values for %d parameters", len(lines), len(names))
	}
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return lines, nil
}

type postfixLogStats struct {
	external, relayed int
	complete          bool
	source            string
}

var (
	postfixConnect = regexp.MustCompile(`postfix/(?:[\w.-]+/)?smtpd\[\d+\]: connect from [^\[\s]*\[([^\]]*)\]`)
	postfixRelay   = regexp.MustCompile(`postfix/(?:[\w.-]+/)?(?:smtp|lmtp)\[\d+\]: .*relay=([^,\s]+).*status=sent`)
)

// countPostfixLine adds one log line to the counts when it falls inside the
// window: an smtpd connection from an address that is not loopback, or a
// message sent anywhere but a local mailbox. Lines whose time cannot be
// read count, so an unknown format errs towards evidence.
func (s *postfixLogStats) countPostfixLine(line string, since, now time.Time) {
	if !strings.Contains(line, "postfix/") {
		return
	}
	if at, ok := syslogTime(line, now); ok && at.Before(since) {
		return
	}
	if m := postfixConnect.FindStringSubmatch(line); m != nil {
		if ip := net.ParseIP(m[1]); ip == nil || !ip.IsLoopback() {
			s.external++
		}
		return
	}
	if m := postfixRelay.FindStringSubmatch(line); m != nil && m[1] != "local" {
		s.relayed++
	}
}

// syslogTime reads a line's leading timestamp: RFC 3339 (rsyslog's high
// precision format, journalctl -o short-iso) or the traditional "Jan _2
// 15:04:05" without a year, taken as the most recent such time.
func syslogTime(line string, now time.Time) (time.Time, bool) {
	first, _, _ := strings.Cut(line, " ")
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05-0700"} {
		if t, err := time.Parse(layout, first); err == nil {
			return t, true
		}
	}
	if len(line) < 15 {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("Jan _2 15:04:05", line[:15], now.Location())
	if err != nil {
		return time.Time{}, false
	}
	t = t.AddDate(now.Year(), 0, 0)
	if t.After(now.Add(24 * time.Hour)) {
		t = t.AddDate(-1, 0, 0)
	}
	return t, true
}

// readPostfixLogs counts the last 7 days of /var/log/mail.log and its
// rotations (gzip included), bounded in size; without such files it asks
// the journal. complete is false when nothing could be read or the bound
// cut the reading short.
func readPostfixLogs(ctx context.Context, now time.Time) postfixLogStats {
	since := now.Add(-hostMailWindow)
	stats := postfixLogStats{}
	files, _ := filepath.Glob(hostPath("/var/log/mail.log") + "*")
	slices.Sort(files)
	read := int64(0)
	readable := false
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.ModTime().Before(since) {
			readable = true // wholly older than the window
			continue
		}
		n, err := scanMailLog(file, hostMailLogLimit-read, func(line string) { stats.countPostfixLine(line, since, now) })
		read += n
		if err != nil {
			return stats
		}
		readable = true
	}
	if len(files) > 0 {
		stats.complete, stats.source = readable, "/var/log/mail.log"
		return stats
	}
	out, err := hostCommand(ctx, "journalctl", "--no-pager", "-q", "-o", "short-iso", "--since", since.UTC().Format("2006-01-02 15:04:05 UTC"),
		"-t", "postfix/smtpd", "-t", "postfix/submission/smtpd", "-t", "postfix/smtps/smtpd", "-t", "postfix/smtp", "-t", "postfix/lmtp")
	if err != nil || len(out) >= ConsoleMaxOutput {
		return stats
	}
	for _, line := range strings.Split(out, "\n") {
		stats.countPostfixLine(line, since, now)
	}
	stats.complete, stats.source = true, "journal"
	return stats
}

var errMailLogLimit = errors.New("mail log size limit reached")

// scanMailLog feeds one log file's lines to fn, decompressing .gz files, and
// stops with errMailLogLimit after limit bytes.
func scanMailLog(path string, limit int64, fn func(string)) (int64, error) {
	if limit <= 0 {
		return 0, errMailLogLimit
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return 0, err
		}
		defer gz.Close()
		r = gz
	}
	counted := &countingReader{r: io.LimitReader(r, limit+1)}
	scanner := bufio.NewScanner(counted)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		fn(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return counted.n, err
	}
	if counted.n > limit {
		return counted.n, errMailLogLimit
	}
	return counted.n, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ---- binding Postfix to loopback ---------------------------------------------

var (
	hostMailMu sync.Mutex
	// Replaced in tests.
	hostMailVerifyTimeout  = 15 * time.Second
	hostMailVerifyInterval = 500 * time.Millisecond
)

func (m *Manager) hostMailRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/host/mail/loopback", func(w http.ResponseWriter, r *http.Request) {
		var in HostMailLoopbackRequest
		if r.ContentLength != 0 {
			if err := Decode(w, r, &in); err != nil {
				Fail(w, 400, err)
				return
			}
		}
		result, err := m.hostMailLoopback(r.Context(), in)
		if err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, result)
	})
}

// hostMailLoopback binds the host's Postfix to loopback: it backs up
// main.cf, sets inet_interfaces = loopback-only, restarts Postfix and
// verifies that the ports are no longer bound on public addresses. A
// Postfix already bound to loopback with the ports free is left alone.
// Changes are recorded in the host job history.
func (m *Manager) hostMailLoopback(ctx context.Context, in HostMailLoopbackRequest) (HostMailLoopbackResult, error) {
	if err := m.hostOperationsAllowed(); err != nil {
		return HostMailLoopbackResult{}, err
	}
	ports := in.Ports
	if len(ports) == 0 {
		for _, p := range hostMailPorts {
			ports = append(ports, StackPort{Port: p, Protocol: "tcp"})
		}
	}
	trigger := in.Trigger
	if trigger != "move" {
		trigger = "manual"
	}
	hostMailMu.Lock()
	defer hostMailMu.Unlock()
	values, err := postconfValues(ctx, "inet_interfaces")
	if err != nil {
		return HostMailLoopbackResult{}, fmt.Errorf("Postfix is not available on this server: %w", err)
	}
	if values[0] == "loopback-only" && hostMailPortsBound(ctx, ports) == "" {
		return HostMailLoopbackResult{Summary: "Postfix no longer listens on the mail ports; they are free."}, nil
	}
	now := time.Now().UTC()
	job := newHostJob(hostMailLoopbackKind, trigger, now)
	m.mu.Lock()
	m.recordHostJobLocked(job)
	_ = m.save()
	m.mu.Unlock()
	var out strings.Builder
	if in.Reference != "" {
		fmt.Fprintf(&out, "Requested by %s.\n", in.Reference)
	}
	result, err := hostMailApplyLoopback(ctx, values[0], ports, now, &out)
	finished := time.Now().UTC()
	job.Finished = &finished
	job.Output = strings.TrimSpace(out.String())
	job.Status, job.Summary = "succeeded", result.Summary
	if err != nil {
		job.Status, job.Summary = "failed", err.Error()
	}
	m.mu.Lock()
	m.recordHostJobLocked(job)
	_ = m.save()
	m.mu.Unlock()
	return result, err
}

func hostMailApplyLoopback(ctx context.Context, previous string, ports []StackPort, now time.Time, out *strings.Builder) (HostMailLoopbackResult, error) {
	result := HostMailLoopbackResult{}
	run := func(name string, args ...string) error {
		text, err := hostCommand(ctx, name, args...)
		fmt.Fprintf(out, "$ %s %s\n%s", name, strings.Join(args, " "), text)
		if text != "" && !strings.HasSuffix(text, "\n") {
			out.WriteString("\n")
		}
		if err != nil {
			return fmt.Errorf("%s failed: %s", name, cmp.Or(strings.TrimSpace(text), err.Error()))
		}
		return nil
	}
	if previous != "loopback-only" {
		backup := postfixMainCF + ".lidza-" + now.Format("20060102T150405Z")
		if err := copyHostFile(hostPath(postfixMainCF), hostPath(backup)); err != nil {
			return result, fmt.Errorf("could not back up %s: %w", postfixMainCF, err)
		}
		result.Backup = backup
		fmt.Fprintf(out, "Backed up %s to %s (inet_interfaces was %q).\n", postfixMainCF, backup, previous)
		if err := run("postconf", "-e", "inet_interfaces = loopback-only"); err != nil {
			return result, err
		}
		result.Changed = true
	}
	// Loopback-only is not enough: Docker publishes an application's port
	// on every address, and the kernel refuses that while Postfix holds
	// 127.0.0.1 on the same port. Local system mail does not need an SMTP
	// listener at all (sendmail hands it to the pickup queue), so the
	// listeners for the requested ports are switched off in master.cf.
	if services := postfixServicesFor(ports); len(services) > 0 && hostMailPortsBound(ctx, ports) != "" {
		backup := postfixMasterCF + ".lidza-" + now.Format("20060102T150405Z")
		if err := copyHostFile(hostPath(postfixMasterCF), hostPath(backup)); err != nil {
			return result, fmt.Errorf("could not back up %s: %w", postfixMasterCF, err)
		}
		fmt.Fprintf(out, "Backed up %s to %s.\n", postfixMasterCF, backup)
		for _, service := range services {
			// An absent service only prints a warning.
			_ = run("postconf", "-M#", service)
		}
		result.Changed = true
	}
	if err := run("systemctl", "restart", "postfix"); err != nil {
		return result, err
	}
	deadline := time.Now().Add(hostMailVerifyTimeout)
	for {
		held := hostMailPortsBound(ctx, ports)
		if held == "" {
			break
		}
		if time.Now().After(deadline) {
			return result, fmt.Errorf("after the restart %s is still bound", held)
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(hostMailVerifyInterval):
		}
	}
	result.Summary = "Postfix no longer listens for SMTP on port " + stackPortLabels(ports) + " (loopback only, its listeners switched off); local system mail is still delivered through its pickup queue, and the port is free."
	if result.Backup != "" {
		result.Summary += " The previous main.cf is " + result.Backup + "."
	}
	out.WriteString("Summary: " + result.Summary + "\n")
	return result, nil
}

// hostMailPortsHeld names the first of the ports still bound on a public
// address ("25/tcp by master"), or "" when they are all free. Unknown
// sockets count as held.
func hostMailPortsHeld(ctx context.Context, ports []StackPort) string {
	listening := moveListeningPorts(ctx)
	if listening == nil {
		return "an unknown port (the listening sockets could not be listed)"
	}
	for _, p := range ports {
		for _, l := range listening {
			if l.Port == p.Port && l.Protocol == cmp.Or(p.Protocol, "tcp") {
				label := fmt.Sprintf("%d/%s", l.Port, l.Protocol)
				if l.Process != "" {
					label += " (" + l.Process + ")"
				}
				return label
			}
		}
	}
	return ""
}

func stackPortLabels(ports []StackPort) string {
	labels := make([]string, 0, len(ports))
	for _, p := range ports {
		labels = append(labels, fmt.Sprintf("%d/%s", p.Port, cmp.Or(p.Protocol, "tcp")))
	}
	return strings.Join(labels, ", ")
}

// copyHostFile copies a file with its permissions.
func copyHostFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

const postfixMasterCF = "/etc/postfix/master.cf"

// postfixServicesFor maps mail ports to the master.cf services that listen
// on them.
func postfixServicesFor(ports []StackPort) []string {
	out := []string{}
	for _, p := range ports {
		if cmp.Or(p.Protocol, "tcp") != "tcp" {
			continue
		}
		switch p.Port {
		case 25:
			out = append(out, "smtp/inet")
		case 465:
			out = append(out, "smtps/inet", "submissions/inet")
		case 587:
			out = append(out, "submission/inet")
		}
	}
	return out
}

// hostMailPortsBound is hostMailPortsHeld on every address, loopback
// included: a port bound on 127.0.0.1 still blocks a publication on all
// addresses.
func hostMailPortsBound(ctx context.Context, ports []StackPort) string {
	if held := hostMailPortsHeld(ctx, ports); held != "" {
		return held
	}
	for _, p := range ports {
		if cmp.Or(p.Protocol, "tcp") != "tcp" {
			continue
		}
		out, err := hostCommand(ctx, "ss", "-Hltn")
		if err != nil {
			return "an unknown port (the listening sockets could not be listed)"
		}
		suffix := fmt.Sprintf(":%d", p.Port)
		for _, line := range strings.Split(out, "\n") {
			if fields := strings.Fields(line); len(fields) >= 4 && strings.HasSuffix(fields[3], suffix) {
				return fmt.Sprintf("%d/tcp on %s", p.Port, strings.TrimSuffix(fields[3], suffix))
			}
		}
	}
	return ""
}
