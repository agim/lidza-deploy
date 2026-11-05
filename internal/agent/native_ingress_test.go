package agent

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
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
)

func TestNativeIngressPreservesRequestsAndUpgradesAcrossPublishRollbackAndManagerStop(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	var finished sync.Once
	defer finished.Do(func() { close(finish) })
	backend := func(label string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/slow" {
				close(started)
				<-finish
			}
			if r.URL.Path != "/socket" {
				io.WriteString(w, label)
				return
			}
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			accept := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(accept[:]) + "\r\n\r\n")
			rw.Flush()
			for {
				// Bounded RFC6455 text-frame fixture: masked client frames and
				// unmasked server frames, not an arbitrary HTTP upgrade echo.
				var header [2]byte
				if _, err := io.ReadFull(rw, header[:]); err != nil {
					return
				}
				if header[0] != 0x81 || header[1]&0x80 == 0 || header[1]&0x7f > 125 {
					return
				}
				var mask [4]byte
				if _, err := io.ReadFull(rw, mask[:]); err != nil {
					return
				}
				payload := make([]byte, int(header[1]&0x7f))
				if _, err := io.ReadFull(rw, payload); err != nil {
					return
				}
				for i := range payload {
					payload[i] ^= mask[i%4]
				}
				response := []byte(label + ":" + string(payload))
				rw.Write([]byte{0x81, byte(len(response))})
				rw.Write(response)
				if rw.Flush() != nil {
					return
				}
			}
		}))
	}
	first, second := backend("old"), backend("new")
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)
	port := func(url string) string { _, p, _ := net.SplitHostPort(strings.TrimPrefix(url, "http://")); return p }
	writer := testManager(t, &fakeRuntime{})
	a := NativeApplication{ID: "rails-example", Name: "example", Runtime: "rails", Domain: "example.test", AdditionalDomains: []string{"www.example.test"}, Services: []string{"example-server.service", "example-worker.service"}}
	disk := diskState{Apps: map[string]App{}, NativeApps: map[string]NativeApplication{a.ID: a}, NativeIngress: map[string]NativeIngressRoute{a.ID: {ReleaseID: "old", Port: port(first.URL)}}}
	saveIngressFixture(t, writer, disk)
	before, _ := os.ReadFile(writer.path())
	cfg := ingressConfig(writer)
	cfg.ReservedDomains = []string{a.Domain, "www.example.test"}
	cfg.NativeIngressTargets = map[string][]string{a.ID: {port(first.URL), port(second.URL)}}
	reader, err := NewIngressReader(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	proxy := httptest.NewServer(Proxy(reader))
	t.Cleanup(proxy.Close)
	edgeURL := nativeIngressTestEdge(t, proxy.URL, a.Domain)
	request := func(path, host string) (int, string, error) {
		req, _ := http.NewRequest("GET", edgeURL+path, nil)
		req.Host = host
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			return 0, "", e
		}
		defer res.Body.Close()
		body, e := io.ReadAll(res.Body)
		return res.StatusCode, string(body), e
	}
	for _, host := range []string{a.Domain, "www.example.test"} {
		if code, body, e := request("/", host); e != nil || code != 200 || body != "old" {
			t.Fatal(code, body, e)
		}
	}
	open := func() (net.Conn, *bufio.Reader) {
		conn, e := net.DialTimeout("tcp", strings.TrimPrefix(edgeURL, "http://"), time.Second)
		if e != nil {
			t.Fatal(e)
		}
		conn.SetDeadline(time.Now().Add(15 * time.Second))
		io.WriteString(conn, "GET /socket HTTP/1.1\r\nHost: "+a.Domain+"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
		rw := bufio.NewReader(conn)
		res, e := http.ReadResponse(rw, nil)
		if e != nil || res.StatusCode != 101 || res.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
			t.Fatal("upgrade", e)
		}
		return conn, rw
	}
	echo := func(conn net.Conn, rw *bufio.Reader, label string) {
		payload := []byte("ping")
		mask := [4]byte{1, 2, 3, 4}
		frame := append([]byte{0x81, 0x84}, mask[:]...)
		for i, b := range payload {
			frame = append(frame, b^mask[i%4])
		}
		if _, e := conn.Write(frame); e != nil {
			t.Fatal(e)
		}
		var header [2]byte
		if _, e := io.ReadFull(rw, header[:]); e != nil || header[0] != 0x81 || header[1]&0x80 != 0 || header[1] > 125 {
			t.Fatal("invalid server websocket frame", header, e)
		}
		body := make([]byte, int(header[1]))
		_, e := io.ReadFull(rw, body)
		if e != nil || string(body) != label+":ping" {
			t.Fatal("websocket changed or closed", string(body), e)
		}
	}
	oldConn, oldRW := open()
	defer oldConn.Close()
	echo(oldConn, oldRW, "old")
	slow := make(chan string, 1)
	go func() {
		_, body, e := request("/slow", a.Domain)
		if e != nil {
			body = e.Error()
		}
		slow <- body
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("long request did not start")
	}
	writer.Close()
	if err := reader.save(); err == nil {
		t.Fatal("reader acquired write access")
	}
	after, _ := os.ReadFile(writer.path())
	if string(before) != string(after) {
		t.Fatal("reader changed sealed inventory")
	}
	disk.NativeIngress[a.ID] = NativeIngressRoute{ReleaseID: "new", Port: port(second.URL)}
	saveIngressFixture(t, writer, disk)
	awaitIngress(t, func() bool { code, body, e := request("/", a.Domain); return e == nil && code == 200 && body == "new" })
	echo(oldConn, oldRW, "old")
	finished.Do(func() { close(finish) })
	select {
	case body := <-slow:
		if body != "old" {
			t.Fatal(body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long request interrupted")
	}
	newConn, newRW := open()
	defer newConn.Close()
	echo(newConn, newRW, "new")
	disk.NativeIngress[a.ID] = NativeIngressRoute{ReleaseID: "rollback", Port: port(first.URL)}
	saveIngressFixture(t, writer, disk)
	awaitIngress(t, func() bool { _, body, e := request("/", a.Domain); return e == nil && body == "old" })
	echo(oldConn, oldRW, "old")
	echo(newConn, newRW, "new")
	// An unapproved snapshot target must fail closed for fresh requests while
	// established streams continue using their original backend connections.
	disk.NativeIngress[a.ID] = NativeIngressRoute{ReleaseID: "bad", Port: "1"}
	saveIngressFixture(t, writer, disk)
	awaitIngress(t, func() bool { return !reader.ingressAvailable() })
	if code, _, _ := request("/", a.Domain); code != 503 {
		t.Fatal("invalid target accepted", code)
	}
	echo(oldConn, oldRW, "old")
	echo(newConn, newRW, "new")
	disk.NativeIngress[a.ID] = NativeIngressRoute{ReleaseID: "recovered", Port: port(first.URL)}
	saveIngressFixture(t, writer, disk)
	awaitIngress(t, func() bool { return reader.ingressAvailable() })
	if code, _, _ := request("/", "unregistered.test"); code != 503 {
		t.Fatal("unregistered host accepted", code)
	}
}

// Optional real edge acceptance. It starts a disposable local Caddy once and
// never reloads it; native route updates must preserve its established streams.
func nativeIngressTestEdge(t *testing.T, upstream, host string) string {
	t.Helper()
	binary := os.Getenv("LIDZA_TEST_CADDY")
	if binary == "" {
		return upstream
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("Caddy acceptance requires an absolute trusted binary path")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	handler := map[string]any{"handler": "reverse_proxy", "upstreams": []any{map[string]any{"dial": strings.TrimPrefix(upstream, "http://")}}}
	server := map[string]any{"listen": []string{address}, "routes": []any{map[string]any{"handle": []any{handler}}}}
	config := map[string]any{
		"admin": map[string]any{"disabled": true, "config": map[string]any{"persist": false}},
		"apps":  map[string]any{"http": map[string]any{"servers": map[string]any{"native": server}}},
	}
	raw, _ := json.Marshal(config)
	file := filepath.Join(t.TempDir(), "caddy.json")
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binary, "run", "--config", file)
	log, err := os.Create(filepath.Join(t.TempDir(), "caddy.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		cancel()
		log.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); cmd.Wait(); log.Close() })
	url := "http://" + address
	awaitIngress(t, func() bool {
		req, _ := http.NewRequest("GET", url, nil)
		req.Host = host
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == 200
	})
	t.Log("real stock Caddy edge enabled; no config reload during publish/rollback")
	return url
}

func TestNativeIngressNeedsExplicitConfigurationAndPublishedRoute(t *testing.T) {
	for _, scenario := range []string{"unconfigured", "unpublished", "astro", "identity", "conflicting-host", "wildcard", "invalid-service", "loop", "manager-role", "duplicate-ports", "empty-port"} {
		t.Run(scenario, func(t *testing.T) {
			writer := testManager(t, &fakeRuntime{})
			a := NativeApplication{ID: "rails-example", Name: "example", Runtime: "rails", Domain: "example.test", Services: []string{"example-server.service"}}
			disk := diskState{Apps: map[string]App{}, NativeApps: map[string]NativeApplication{a.ID: a}, NativeIngress: map[string]NativeIngressRoute{a.ID: {ReleaseID: "old", Port: "9150"}}}
			cfg := ingressConfig(writer)
			cfg.NativeIngressTargets = map[string][]string{a.ID: {"9150"}}
			switch scenario {
			case "unconfigured":
				cfg.NativeIngressTargets = nil
			case "unpublished":
				disk.NativeIngress = nil
			case "astro":
				a.Runtime = "astro"
			case "identity":
				a.ID = "other"
			case "conflicting-host":
				other := testApp("other")
				other.Domain = a.Domain
				disk.Apps[other.ID] = other
			case "wildcard":
				a.Domain = "*.example.test"
			case "invalid-service":
				a.Services = []string{"../other"}
			case "loop":
				cfg.NativeIngressTargets[a.ID] = []string{"18081"}
			case "manager-role":
				cfg.IngressOnly = false
			case "duplicate-ports":
				cfg.NativeIngressTargets[a.ID] = []string{"9150", "9150"}
			case "empty-port":
				cfg.NativeIngressTargets[a.ID] = []string{""}
			}
			disk.NativeApps["rails-example"] = a
			saveIngressFixture(t, writer, disk)
			reader, err := NewIngressReader(context.Background(), cfg)
			if scenario == "unconfigured" || scenario == "unpublished" {
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				if _, found := reader.nativeIngressApplication(a.Domain); found {
					t.Fatal("discovery became a live route")
				}
				return
			}
			if err == nil {
				reader.Close()
				t.Fatal("invalid native route/role accepted")
			}
		})
	}
}
