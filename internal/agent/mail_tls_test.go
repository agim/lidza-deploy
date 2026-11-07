package agent

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

const mailTLSWarning25 = `Port 25/tcp receives mail, but no service declares public_tls: senders that require STARTTLS (Gmail, Microsoft) fail and defer mail. Declare "public_tls": {"hostname": "primary", "uid": <the mail server's uid>} on the service that terminates SMTP.`

func mailRelease(a App, id string, publicTLS bool, ports ...StackPort) *Release {
	manifest := fixtureStackManifest()
	manifest.Services[0].PublicPorts = ports
	mail := StackContainer{Container: stackContainerName(a, id, "mail"), Service: manifest.Services[1]}
	if publicTLS {
		mail.Service.PublicTLS = &StackPublicTLS{Hostname: "primary", UID: 101}
		mail.PublicTLS = serviceTLSBinding(a, mail.Service)
	}
	return &Release{ID: id, Commit: id, Stack: &StackRelease{Network: "lidza-stack-" + a.ID + "-" + id, Containers: []StackContainer{{Container: stackContainerName(a, id, "app"), Service: manifest.Services[0]}, mail}}}
}

func TestMailTLSStackManifestCheck(t *testing.T) {
	a := testApp("mailer")
	if got := releaseMailTLSWarning(mailRelease(a, "r1", false, StackPort{25, "tcp"}, StackPort{8443, "tcp"})); got != mailTLSWarning25 {
		t.Fatalf("warning: %q", got)
	}
	if got := releaseMailTLSWarning(mailRelease(a, "r1", false, StackPort{465, "tcp"}, StackPort{587, ""}, StackPort{25, "tcp"})); !strings.HasPrefix(got, "Ports 25/tcp, 465/tcp and 587/tcp receive mail, but no service declares public_tls") {
		t.Fatalf("warning: %q", got)
	}
	for name, r := range map[string]*Release{
		"public_tls declared": mailRelease(a, "r1", true, StackPort{25, "tcp"}),
		"no mail port":        mailRelease(a, "r1", false, StackPort{8443, "tcp"}),
		"udp only":            mailRelease(a, "r1", false, StackPort{25, "udp"}),
		"no stack":            {ID: "r1"},
		"no release":          nil,
	} {
		if got := releaseMailTLSWarning(r); got != "" {
			t.Fatalf("%s: %q", name, got)
		}
	}
	// The application view and the move source carry it.
	m := testManager(t, &fakeRuntime{})
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	stored := m.data.Apps[a.ID]
	stored.Current = mailRelease(a, "r1", false, StackPort{25, "tcp"})
	m.data.Apps[a.ID] = stored
	m.mu.Unlock()
	if apps := m.Apps(); len(apps) != 1 || len(apps[0].Warnings) != 1 || apps[0].Warnings[0] != mailTLSWarning25 {
		t.Fatalf("app warnings: %+v", apps)
	}
	source, err := m.moveSource(context.Background(), a.ID)
	if err != nil || source.PublicTLS == nil || *source.PublicTLS {
		t.Fatalf("move source public_tls: %v %v", source.PublicTLS, err)
	}
	m.mu.Lock()
	stored.Current = mailRelease(a, "r2", true, StackPort{25, "tcp"})
	m.data.Apps[a.ID] = stored
	m.mu.Unlock()
	if apps := m.Apps(); len(apps[0].Warnings) != 0 {
		t.Fatalf("app warnings: %v", apps[0].Warnings)
	}
	if source, _ = m.moveSource(context.Background(), a.ID); source.PublicTLS == nil || !*source.PublicTLS {
		t.Fatalf("move source public_tls: %v", source.PublicTLS)
	}
}

type fakeSMTPMode int

const (
	smtpNoStartTLS fakeSMTPMode = iota
	smtpWorkingTLS
	smtpBrokenTLS
	smtpRefusedTLS
)

func selfSignedMailCertificate(t *testing.T, host string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{raw}, PrivateKey: key}
}

// fakeSMTP serves one SMTP dialogue per connection and records the SNI the
// probe sent.
func fakeSMTP(t *testing.T, mode fakeSMTPMode) (string, chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	cert := selfSignedMailCertificate(t, "mailer.example.com")
	sni := make(chan string, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				r := bufio.NewReader(conn)
				_, _ = conn.Write([]byte("220 mailer.example.com ESMTP\r\n"))
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					switch verb := strings.ToUpper(strings.Fields(line + " x")[0]); verb {
					case "EHLO":
						if mode == smtpNoStartTLS {
							_, _ = conn.Write([]byte("250-mailer.example.com\r\n250-PIPELINING\r\n250 8BITMIME\r\n"))
						} else {
							_, _ = conn.Write([]byte("250-mailer.example.com\r\n250-PIPELINING\r\n250-STARTTLS\r\n250 8BITMIME\r\n"))
						}
					case "STARTTLS":
						switch mode {
						case smtpRefusedTLS:
							_, _ = conn.Write([]byte("454 4.7.0 TLS not available due to local problem\r\n"))
							continue
						case smtpBrokenTLS:
							// Postfix without a certificate: it accepts,
							// then aborts the handshake.
							_, _ = conn.Write([]byte("220 2.0.0 Ready to start TLS\r\n"))
							return
						}
						_, _ = conn.Write([]byte("220 2.0.0 Ready to start TLS\r\n"))
						server := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
							sni <- hello.ServerName
							return nil, nil
						}})
						if server.Handshake() != nil {
							return
						}
						_, _ = bufio.NewReader(server).ReadString('\n')
						return
					case "QUIT":
						_, _ = conn.Write([]byte("221 Bye\r\n"))
						return
					default:
						_, _ = conn.Write([]byte("502 unknown\r\n"))
					}
				}
			}(conn)
		}
	}()
	return listener.Addr().String(), sni
}

func TestSMTPProbeReportsSTARTTLSWithoutAWorkingCertificate(t *testing.T) {
	ctx := context.Background()
	addr, _ := fakeSMTP(t, smtpNoStartTLS)
	if got := probeSMTP(ctx, addr, "mailer.example.com", 25); got != "" {
		t.Fatalf("no STARTTLS: %q", got)
	}
	addr, sni := fakeSMTP(t, smtpWorkingTLS)
	if got := probeSMTP(ctx, addr, "mailer.example.com", 25); got != "" {
		t.Fatalf("self-signed certificate: %q", got)
	}
	if got := <-sni; got != "mailer.example.com" {
		t.Fatalf("SNI %q", got)
	}
	addr, _ = fakeSMTP(t, smtpBrokenTLS)
	got := probeSMTP(ctx, addr, "mailer.example.com", 25)
	t.Log(got)
	if !strings.HasPrefix(got, "Port 25/tcp: STARTTLS offered without a working certificate (TLS handshake for mailer.example.com failed: ") || !strings.Contains(got, "Gmail, Microsoft") {
		t.Fatalf("broken TLS: %q", got)
	}
	addr, _ = fakeSMTP(t, smtpRefusedTLS)
	if got := probeSMTP(ctx, addr, "mailer.example.com", 587); !strings.Contains(got, "Port 587/tcp: STARTTLS offered without a working certificate") || !strings.Contains(got, "STARTTLS answered 454") {
		t.Fatalf("refused TLS: %q", got)
	}
	// A server that never answers is bounded and not a TLS finding.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	started := time.Now()
	if got := probeSMTP(ctx, silent.Addr().String(), "mailer.example.com", 25); got != "" || time.Since(started) > 7*time.Second {
		t.Fatalf("silent server: %q after %s", got, time.Since(started))
	}
}

// mailRuntime deploys a stack release that publishes port 25 without
// public_tls.
type mailRuntime struct{ fakeRuntime }

func (f *mailRuntime) Deploy(ctx context.Context, a App, id, token string) (*Release, error) {
	return mailRelease(a, id, false, StackPort{25, "tcp"}), nil
}

func TestMailTLSDeploymentRecordsManifestAndProbeWarnings(t *testing.T) {
	addr, _ := fakeSMTP(t, smtpBrokenTLS)
	previous := mailProbeTarget
	mailProbeTarget = func(port int) string { return addr }
	t.Cleanup(func() { mailProbeTarget = previous })
	m := testManager(t, &mailRuntime{})
	a := testApp("mailer")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	d, err := m.Enqueue(a.ID, DeployRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitDeployment(t, m, d.ID); got.Status != "live" {
		t.Fatalf("a mail warning never fails the deployment: %+v", got)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var log string
		for _, item := range m.Deployments() {
			if item.ID == d.ID {
				log = item.Log
			}
		}
		apps := m.Apps()
		if strings.Contains(log, "warning\n"+mailTLSWarning25) && strings.Contains(log, "STARTTLS offered without a working certificate") && len(apps) == 1 && len(apps[0].Warnings) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("log %q warnings %v", log, apps[0].Warnings)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
