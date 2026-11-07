package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Mail ports and their certificate. A stack that publishes a mail port (25,
// 465 or 587/tcp) receives or submits mail on the server's address. Senders
// such as Gmail and Microsoft require STARTTLS on port 25 and defer mail when
// the handshake fails, so a mail server without the primary hostname's
// certificate (public_tls, service_tls.go) loses mail without an error
// anywhere. The agent says so twice: from the manifest, when no service of
// the release declares public_tls, and from a probe of the published port
// that reads the banner, sends EHLO and completes the TLS handshake with the
// primary hostname as SNI. Both are warnings: a deployment never waits for or
// fails on them.

var mailPorts = []int{25, 465, 587}

const mailProbeTimeout = 5 * time.Second
const mailProbeInterval = 15 * time.Minute

// mailPortsOf returns the mail ports among published ports.
func mailPortsOf(ports []StackPort) []StackPort {
	var out []StackPort
	for _, p := range ports {
		if slices.Contains(mailPorts, p.Port) && (p.Protocol == "" || strings.EqualFold(p.Protocol, "tcp")) {
			out = append(out, StackPort{Port: p.Port, Protocol: "tcp"})
		}
	}
	return out
}

func mailPortLabels(ports []StackPort) string {
	labels := make([]string, 0, len(ports))
	for _, p := range ports {
		labels = append(labels, strconv.Itoa(p.Port)+"/tcp")
	}
	if len(labels) <= 1 {
		return strings.Join(labels, "")
	}
	return strings.Join(labels[:len(labels)-1], ", ") + " and " + labels[len(labels)-1]
}

// MailTLSWarning is the warning for mail ports published without any
// service declaring public_tls; "" when ports holds no mail port. The move
// plan of the control panel uses the same text.
func MailTLSWarning(ports []StackPort) string {
	mail := mailPortsOf(ports)
	if len(mail) == 0 {
		return ""
	}
	subject := "Port " + mailPortLabels(mail) + " receives"
	if len(mail) > 1 {
		subject = "Ports " + mailPortLabels(mail) + " receive"
	}
	return subject + ` mail, but no service declares public_tls: senders that require STARTTLS (Gmail, Microsoft) fail and defer mail. Declare "public_tls": {"hostname": "primary", "uid": <the mail server's uid>} on the service that terminates SMTP.`
}

// releasePublicTLS reports whether a service of a stack release declares
// public_tls.
func releasePublicTLS(r *Release) bool {
	if r == nil || r.Stack == nil {
		return false
	}
	return slices.ContainsFunc(r.Stack.Containers, func(c StackContainer) bool { return c.PublicTLS != nil || c.Service.PublicTLS != nil })
}

// releaseMailTLSWarning is the manifest warning of a release: mail ports
// published while no service declares public_tls.
func releaseMailTLSWarning(r *Release) string {
	if r == nil || r.Stack == nil || releasePublicTLS(r) {
		return ""
	}
	return MailTLSWarning(movePublicPorts(r))
}

// probeSMTP checks one published mail port at addr. Implicit TLS (465)
// handshakes at once; the others read the banner, send EHLO and, when
// STARTTLS is offered, upgrade. It returns a warning only when TLS is offered
// and the handshake with SNI hostname fails; a port that does not answer or
// offers no STARTTLS returns "" (container health and the manifest check
// cover those).
func probeSMTP(ctx context.Context, addr, hostname string, port int) string {
	ctx, cancel := context.WithTimeout(ctx, mailProbeTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return ""
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	// Opportunistic SMTP TLS does not verify the chain; senders that require
	// STARTTLS fail on a handshake that does not complete, which is what this
	// reproduces.
	config := &tls.Config{ServerName: hostname, InsecureSkipVerify: true} //nolint:gosec // see above
	label := fmt.Sprintf("Port %d/tcp", port)
	fail := func(err error) string {
		return fmt.Sprintf("%s: STARTTLS offered without a working certificate (TLS handshake for %s failed: %s). Senders that require STARTTLS (Gmail, Microsoft) fail and defer mail.", label, hostname, smtpProbeError(err))
	}
	if port == 465 {
		client := tls.Client(conn, config)
		if err := client.HandshakeContext(ctx); err != nil {
			return strings.Replace(fail(err), "STARTTLS offered", "Implicit TLS offered", 1)
		}
		return ""
	}
	reader := bufio.NewReader(conn)
	if code, _, err := smtpResponse(reader); err != nil || code != 220 {
		return ""
	}
	if _, err := conn.Write([]byte("EHLO lidza-tls-probe.invalid\r\n")); err != nil {
		return ""
	}
	code, lines, err := smtpResponse(reader)
	if err != nil || code != 250 {
		return ""
	}
	offered := slices.ContainsFunc(lines, func(line string) bool {
		fields := strings.Fields(line)
		return len(fields) > 0 && strings.EqualFold(fields[0], "STARTTLS")
	})
	if !offered {
		_, _ = conn.Write([]byte("QUIT\r\n"))
		return ""
	}
	if _, err := conn.Write([]byte("STARTTLS\r\n")); err != nil {
		return fail(err)
	}
	code, lines, err = smtpResponse(reader)
	if err != nil {
		return fail(err)
	}
	if code != 220 {
		return fail(fmt.Errorf("STARTTLS answered %d %s", code, strings.Join(lines, " ")))
	}
	client := tls.Client(conn, config)
	if err := client.HandshakeContext(ctx); err != nil {
		return fail(err)
	}
	_ = client.SetDeadline(deadline)
	_, _ = client.Write([]byte("QUIT\r\n"))
	return ""
}

func smtpProbeError(err error) string {
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "no answer within 5 s"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "no answer within 5 s"
	}
	return err.Error()
}

// smtpResponse reads one (possibly multi-line) SMTP reply: its code and the
// text of each line.
func smtpResponse(r *bufio.Reader) (int, []string, error) {
	var lines []string
	for range 64 {
		line, err := r.ReadString('\n')
		if err != nil {
			return 0, lines, err
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) < 3 {
			return 0, lines, errors.New("malformed SMTP reply")
		}
		code, err := strconv.Atoi(line[:3])
		if err != nil {
			return 0, lines, errors.New("malformed SMTP reply")
		}
		text := ""
		if len(line) > 4 {
			text = line[4:]
		}
		lines = append(lines, text)
		if len(line) == 3 || line[3] == ' ' {
			return code, lines, nil
		}
	}
	return 0, lines, errors.New("SMTP reply too long")
}

// mailProbeTarget is where a published mail port is reached: the loopback
// address, since public ports publish on all addresses. Tests replace it.
var mailProbeTarget = func(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

type mailProbeResult struct {
	release  string
	warnings []string
}

// probeReleaseMail probes the mail ports of a release; the warnings it
// returns are recorded for the application.
func (m *Manager) probeReleaseMail(ctx context.Context, appID, hostname string, r *Release) []string {
	var warnings []string
	if r == nil || r.Stack == nil || hostname == "" {
		return nil
	}
	for _, p := range mailPortsOf(movePublicPorts(r)) {
		if w := probeSMTP(ctx, mailProbeTarget(p.Port), hostname, p.Port); w != "" {
			warnings = append(warnings, w)
		}
	}
	m.mu.Lock()
	if m.mailProbes == nil {
		m.mailProbes = map[string]mailProbeResult{}
	}
	m.mailProbes[appID] = mailProbeResult{release: r.ID, warnings: warnings}
	m.mu.Unlock()
	return warnings
}

// checkReleaseMail records the manifest warning in the deployment log and
// probes the published mail ports in the background, logging what it finds;
// the deployment does not wait for the probe.
func (m *Manager) checkReleaseMail(ctx context.Context, j job, r *Release) {
	if r == nil || r.Stack == nil || len(mailPortsOf(movePublicPorts(r))) == 0 {
		return
	}
	if w := releaseMailTLSWarning(r); w != "" {
		commandDiagnostic(ctx, "warning", w)
	}
	probe := m.deploymentDiagnostics(m.ctx, j)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for _, w := range m.probeReleaseMail(probe, j.app.ID, j.app.Domain, r) {
			commandDiagnostic(probe, "warning", w)
		}
	}()
}

// mailWarningsLocked lists the mail warnings of an application's current
// release: the manifest check and the latest probe of that release.
func (m *Manager) mailWarningsLocked(a App) []string {
	var out []string
	if w := releaseMailTLSWarning(a.Current); w != "" {
		out = append(out, w)
	}
	if a.Current != nil {
		if probe, ok := m.mailProbes[a.ID]; ok && probe.release == a.Current.ID {
			out = append(out, probe.warnings...)
		}
	}
	return out
}

// mailProbeLoop probes the published mail ports of every running stack
// application periodically, so a certificate that stops working shows on
// the application without a new deployment.
func (m *Manager) mailProbeLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(mailProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.probeAllMail(m.ctx)
		}
	}
}

func (m *Manager) probeAllMail(ctx context.Context) {
	type target struct {
		id, domain string
		release    *Release
	}
	var targets []target
	m.mu.Lock()
	for _, a := range m.data.Apps {
		if a.Stopped || a.Retiring || a.Current == nil || a.Current.Stack == nil || m.busy(a.ID) {
			continue
		}
		if len(mailPortsOf(movePublicPorts(a.Current))) > 0 {
			targets = append(targets, target{a.ID, a.Domain, a.Current})
		}
	}
	m.mu.Unlock()
	for _, t := range targets {
		if ctx.Err() != nil {
			return
		}
		m.probeReleaseMail(ctx, t.id, t.domain, t.release)
	}
}
