package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/smtp"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestDockerPublicTLSStackLifecycle(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for actual public certificate stack lifecycle")
	}
	t.Setenv("TEST_STACK_PUBLIC_TLS", "1")
	TestDockerStackV2Lifecycle(t)
}
func assertDockerPublicTLS(t *testing.T, m *Manager, id string) {
	t.Helper()
	r := m.Current(id)
	if r == nil || r.Stack == nil {
		t.Fatal("missing TLS stack")
	}
	found := false
	for _, c := range r.Stack.Containers {
		if c.PublicTLS == nil {
			continue
		}
		found = true
		if m.serviceTLS.status(*c.PublicTLS).Status != "ready" {
			t.Fatal("certificate not ready after lifecycle transition")
		}
		raw, e := command(context.Background(), "", nil, "docker", "inspect", "--format", `{{range .Mounts}}{{if eq .Destination "/var/lib/lidza-public-tls"}}{{.RW}}{{end}}{{end}}`, c.Container)
		if e != nil || raw != "false" {
			t.Fatal("service certificate volume is not read-only", raw, e)
		}
		target := filepath.Join(t.TempDir(), "cert.pem")
		if _, e = command(context.Background(), "", nil, "docker", "cp", c.Container+":"+publicTLSMount+"/current/fullchain.pem", target); e != nil {
			t.Fatal(e)
		}
		block, _ := pem.Decode(mustReadTLS(t, target))
		if block == nil {
			t.Fatal("container cannot read public certificate")
		}
		leaf, e := x509.ParseCertificate(block.Bytes)
		if e != nil || leaf.VerifyHostname(c.PublicTLS.Hostname) != nil {
			t.Fatal("container certificate hostname mismatch", e)
		}
	}
	if !found {
		t.Fatal("stack dropped opted-in TLS service")
	}
}

// An isolated protocol fixture: no app-specific Postfix behavior is assumed.
const publicTLSSMTPFixture = `package main
import("bufio";"crypto/tls";"crypto/x509";"encoding/pem";"fmt";"net";"os";"strings";"time")
func pair()(*tls.Certificate,error){b,e:=os.ReadFile(os.Getenv("DEPLOY_PUBLIC_TLS_BUNDLE_FILE"));if e!=nil{return nil,e};p,e:=tls.X509KeyPair(b,b);if e!=nil{return nil,e};block,_:=pem.Decode(b);if block==nil{return nil,fmt.Errorf("missing leaf")};leaf,e:=x509.ParseCertificate(block.Bytes);if e!=nil{return nil,e};if e=leaf.VerifyHostname(os.Getenv("DEPLOY_PUBLIC_TLS_HOSTNAME"));e!=nil{return nil,e};if time.Now().After(leaf.NotAfter){return nil,fmt.Errorf("expired")};return &p,nil}
func main(){if len(os.Args)>1 {if e:=os.WriteFile("/var/lib/lidza-public-tls/write-probe",[]byte("bad"),0600);e!=nil{return};os.Exit(3)};ln,e:=net.Listen("tcp",":2525");if e!=nil{panic(e)};for{c,e:=ln.Accept();if e!=nil{return};go serve(c)}}
func serve(c net.Conn){defer c.Close();c.SetDeadline(time.Now().Add(10*time.Second));fmt.Fprint(c,"220 isolated fixture\r\n");r:=bufio.NewReader(c);for{line,e:=r.ReadString('\n');if e!=nil{return};switch{case strings.HasPrefix(line,"EHLO"):if _,e:=pair();e==nil{fmt.Fprint(c,"250-fixture\r\n250 STARTTLS\r\n")}else{fmt.Fprint(c,"250 fixture\r\n")};case strings.HasPrefix(line,"STARTTLS"):p,e:=pair();if e!=nil{fmt.Fprint(c,"454 TLS unavailable\r\n");continue};fmt.Fprint(c,"220 TLS ready\r\n");s:=tls.Server(c,&tls.Config{MinVersion:tls.VersionTLS13,Certificates:[]tls.Certificate{*p}});if s.Handshake()!=nil{return};c=s;r=bufio.NewReader(c);case strings.HasPrefix(line,"QUIT"):fmt.Fprint(c,"221 bye\r\n");return;default:fmt.Fprint(c,"500 unsupported\r\n")}}}
`

func TestDockerPublicTLSSTARTTLSRenewal(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for real isolated STARTTLS acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f := newPublicTLSFixture(t)
	f.sync(t)
	directory := t.TempDir()
	source := filepath.Join(directory, "smtp.go")
	binary := filepath.Join(directory, "smtp-fixture")
	if e := os.WriteFile(source, []byte(publicTLSSMTPFixture), 0600); e != nil {
		t.Fatal(e)
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, source)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, e := build.CombinedOutput(); e != nil {
		t.Fatal(e, string(output))
	}
	name := "lidza-public-tls-test-" + newID()[:10]
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_, _ = command(cleanup, "", nil, "docker", "rm", "-f", name)
	})
	args := []string{"run", "-d", "--name", name, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "64m", "--cpus", "0.25", "--pids-limit", "32", "--user", strconv.Itoa(f.b.UID), "--publish", "127.0.0.1::2525", "--mount", f.c.mount(f.b), "--mount", "type=bind,src=" + binary + ",dst=/smtp-fixture,readonly", "--entrypoint", "/smtp-fixture"}
	for key, value := range f.b.environment() {
		args = append(args, "--env", key+"="+value)
	}
	args = append(args, databaseImage)
	if _, e := command(ctx, "", nil, "docker", args...); e != nil {
		t.Fatal(e)
	}
	port, e := command(ctx, "", nil, "docker", "port", name, "2525/tcp")
	if e != nil {
		t.Fatal(e)
	}
	connect := func() *smtp.Client {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			conn, e := net.DialTimeout("tcp", port, time.Second)
			if e == nil {
				conn.SetDeadline(time.Now().Add(10 * time.Second))
				client, e := smtp.NewClient(conn, f.b.Hostname)
				if e == nil {
					return client
				}
				conn.Close()
			}
			if time.Now().After(deadline) {
				t.Fatal("SMTP fixture unavailable", e)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	initial := connect()
	if ok, _ := initial.Extension("STARTTLS"); ok {
		t.Fatal("STARTTLS advertised before certificate provisioned")
	}
	initial.Close()
	readLeaf := func() string {
		t.Helper()
		client := connect()
		defer client.Close()
		if ok, _ := client.Extension("STARTTLS"); !ok {
			t.Fatal("STARTTLS not enabled after provisioning")
		}
		if e := client.StartTLS(&tls.Config{RootCAs: f.c.roots, ServerName: f.b.Hostname, MinVersion: tls.VersionTLS13}); e != nil {
			t.Fatal(e)
		}
		state, ok := client.TLSConnectionState()
		if !ok || state.Version != tls.VersionTLS13 {
			t.Fatal("TLS 1.3 handshake unavailable")
		}
		return certificateFingerprint(state.PeerCertificates[0])
	}
	cert, key := f.issue(t, []string{f.b.Hostname}, time.Now().Add(time.Hour))
	f.store(t, cert, key)
	f.sync(t)
	first := readLeaf()
	cert, key = f.issue(t, []string{f.b.Hostname}, time.Now().Add(2*time.Hour))
	f.store(t, cert, key)
	f.sync(t)
	second := readLeaf()
	if first == second {
		t.Fatal("live service did not observe renewed certificate")
	}
	if _, e = command(ctx, "", nil, "docker", "exec", name, "/smtp-fixture", "write-probe"); e != nil {
		t.Fatal("private volume was writable", e)
	}
	state, e := command(ctx, "", nil, "docker", "inspect", "--format", "{{.RestartCount}}", name)
	if e != nil || state != "0" {
		t.Fatal("renewal restarted service", state, e)
	}
	t.Log("Actual isolated Docker SMTP: pending suppresses STARTTLS; trusted TLS1.3 succeeds; renewal changes peer leaf without restart; volume is read-only")
}
