package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func controlTar(t *testing.T, entries map[string]string, extra ...*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for name, body := range entries {
		if strings.HasSuffix(name, "/") {
			tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o700})
			continue
		}
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	for _, h := range extra {
		tw.WriteHeader(h)
	}
	tw.Close()
	return b.Bytes()
}

func TestControlEnvRendersStandbyAndHostValues(t *testing.T) {
	raw, err := renderControlEnv(map[string]string{"PUBLIC_URL": "https://deploy.example.com", "CONTROL_USER": `op"er\ator@example.com`, "LIDZA_ADDR": "0.0.0.0:80", ControlStandbyEnv: "0"}, true)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{`PUBLIC_URL="https://deploy.example.com"`, `CONTROL_USER="op\"er\\ator@example.com"`, `LIDZA_ADDR="127.0.0.1:3000"`, `CONTROL_DATA_DIR="/var/lib/lidza-control"`, `CONTROL_STANDBY="1"`} {
		if !strings.Contains(text, want+"\n") {
			t.Fatalf("env lacks %s:\n%s", want, text)
		}
	}
	active, _ := renderControlEnv(map[string]string{"PUBLIC_URL": "https://deploy.example.com"}, false)
	if strings.Contains(string(active), ControlStandbyEnv) {
		t.Fatalf("active env still standing by:\n%s", active)
	}
	if _, err := renderControlEnv(map[string]string{"BAD": "a\nINJECTED=1"}, false); err == nil {
		t.Fatal("a newline in a value was accepted")
	}
	if _, err := renderControlEnv(map[string]string{"lower": "x"}, false); err == nil {
		t.Fatal("an invalid name was accepted")
	}
}

func TestControlFilesUnpackOnlyRelativeRegularFiles(t *testing.T) {
	stage := t.TempDir()
	body := controlTar(t, map[string]string{"config/": "", "config/master.key": "k", "control.enc.json": `"sealed"`, "setup-complete.json": "true"})
	count, err := receiveControlFiles(bytes.NewReader(body), stage)
	if err != nil || count != 3 {
		t.Fatalf("count %d err %v", count, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(stage, "data", "config", "master.key")); string(raw) != "k" {
		t.Fatalf("key %q", raw)
	}
	info, _ := os.Stat(filepath.Join(stage, "data", "control.enc.json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	for name, bad := range map[string][]byte{
		"escape":  controlTar(t, map[string]string{"../outside": "x"}),
		"symlink": controlTar(t, nil, &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/shadow"}),
	} {
		if _, err := receiveControlFiles(bytes.NewReader(bad), stage); err == nil {
			t.Fatalf("%s accepted", name)
		}
		// A refused stream leaves the previously received data in place.
		if _, err := os.Stat(filepath.Join(stage, "data", "control.enc.json")); err != nil {
			t.Fatalf("%s removed the earlier data: %v", name, err)
		}
	}
}

func TestControlExtractsPanelBinaryFromBundle(t *testing.T) {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	gz.Write(controlTar(t, map[string]string{"./bin/lidza-control": "#!panel", "./bin/lidza-agent": "#!agent", "./deploy/lidza-control.service": "[Unit]\n# lidza-control\n"}))
	gz.Close()
	dir := t.TempDir()
	archive := filepath.Join(dir, "bundle.tar.gz")
	os.WriteFile(archive, b.Bytes(), 0o600)
	stage := filepath.Join(dir, "stage")
	if err := extractControlBinary(archive, stage); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(stage, "bin", "lidza-control")); string(raw) != "#!panel" {
		t.Fatalf("binary %q", raw)
	}
	if _, err := os.Stat(filepath.Join(stage, "bin", "lidza-agent")); err == nil {
		t.Fatal("the agent binary was extracted too")
	}
	var empty bytes.Buffer
	gz = gzip.NewWriter(&empty)
	gz.Write(controlTar(t, map[string]string{"./bin/lidza-agent": "#!agent"}))
	gz.Close()
	os.WriteFile(archive, empty.Bytes(), 0o600)
	if err := extractControlBinary(archive, stage); err == nil {
		t.Fatal("a bundle without the panel was accepted")
	}
}

func TestCaddyStorageRootResolution(t *testing.T) {
	old := caddyUserHome
	caddyUserHome = func(name string) string { return "/var/lib/" + name }
	t.Cleanup(func() { caddyUserHome = old })
	cases := []struct {
		storage map[string]any
		env     map[string]string
		want    string
	}{
		{map[string]any{"module": "file_system", "root": "/srv/caddy-data"}, nil, "/srv/caddy-data"},
		{map[string]any{"module": "redis"}, map[string]string{"HOME": "/root"}, ""},
		{nil, map[string]string{"XDG_DATA_HOME": "/data", "HOME": "/root"}, "/data/caddy"},
		{nil, map[string]string{"HOME": "/home/web"}, "/home/web/.local/share/caddy"},
		{nil, nil, "/var/lib/caddy/.local/share/caddy"},
	}
	for i, tc := range cases {
		if got := caddyStorageRoot(tc.storage, tc.env, "caddy"); got != tc.want {
			t.Fatalf("case %d: %q, want %q", i, got, tc.want)
		}
	}
}

func TestCaddyCertificateCopyPaths(t *testing.T) {
	root := t.TempDir()
	issuer := "acme-v02.api.letsencrypt.org-directory"
	exact := filepath.Join(root, "certificates", issuer, "deploy.example.com")
	os.MkdirAll(exact, 0o700)
	os.WriteFile(filepath.Join(exact, "deploy.example.com.crt"), []byte("-----BEGIN CERTIFICATE-----\nexact\n"), 0o600)
	os.WriteFile(filepath.Join(exact, "deploy.example.com.key"), []byte("-----BEGIN EC PRIVATE KEY-----\n"), 0o600)
	os.WriteFile(filepath.Join(exact, "deploy.example.com.json"), []byte(`{"sans":["deploy.example.com"]}`), 0o600)
	wild := filepath.Join(root, "certificates", issuer, "wildcard_.example.org")
	os.MkdirAll(wild, 0o700)
	os.WriteFile(filepath.Join(wild, "wildcard_.example.org.crt"), []byte("-----BEGIN CERTIFICATE-----\nwild\n"), 0o600)
	os.WriteFile(filepath.Join(wild, "wildcard_.example.org.key"), []byte("-----BEGIN PRIVATE KEY-----\n"), 0o600)

	cert, err := findCaddyCertificate(root, "deploy.example.com")
	if err != nil || cert == nil || cert.Issuer != issuer || cert.Name != "deploy.example.com" || cert.Meta == "" {
		t.Fatalf("exact: %+v %v", cert, err)
	}
	wc, _ := findCaddyCertificate(root, "panel.example.org")
	if wc == nil || wc.Name != "wildcard_.example.org" {
		t.Fatalf("wildcard: %+v", wc)
	}
	if none, _ := findCaddyCertificate(root, "other.example.net"); none != nil {
		t.Fatalf("found %+v", none)
	}
	if _, err := findCaddyCertificate(root, "../../etc"); err == nil {
		t.Fatal("a path was accepted as hostname")
	}

	stage := t.TempDir()
	if err := stageControlCertificate(stage, "/var/lib/caddy/.local/share/caddy", *cert); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(stage, "cert", issuer, "deploy.example.com")
	for _, name := range []string{"deploy.example.com.crt", "deploy.example.com.key", "deploy.example.com.json"} {
		if _, err := os.Stat(filepath.Join(staged, name)); err != nil {
			t.Fatalf("staged %s: %v", name, err)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(stage, "cert-storage")); strings.TrimSpace(string(raw)) != "/var/lib/caddy/.local/share/caddy" {
		t.Fatalf("storage %q", raw)
	}
	bad := *cert
	bad.Name = "../../root"
	if err := stageControlCertificate(stage, "/x/caddy", bad); err == nil {
		t.Fatal("an escaping storage name was accepted")
	}
	bad = *cert
	bad.Key = base64.StdEncoding.EncodeToString([]byte("not a key"))
	if err := stageControlCertificate(stage, "/x/caddy", bad); err == nil {
		t.Fatal("a non-PEM key was accepted")
	}
}

func TestControlRoutePublishJSONThroughCaddyAdmin(t *testing.T) {
	admin := newFakeCaddyAdmin(t, operatorCaddyConfig())
	m := testManager(t, &fakeRuntime{})
	m.data.HostIngress = &HostIngressState{Caddy: &NativeCaddyConfig{Admin: admin.URL(), Server: "https"}}
	os.MkdirAll(m.controlDir(), 0o700)
	mode, err := m.publishControlRoute(context.Background(), "deploy.example.com", false)
	if err != nil || mode != "caddy" {
		t.Fatalf("mode %q err %v", mode, err)
	}
	route, _ := admin.at("/config/apps/http/servers/https/routes/0").(map[string]any)
	if route["@id"] != controlRouteID {
		t.Fatalf("route %v", route)
	}
	raw, _ := json.Marshal(route)
	if !jsonEqual(raw, renderControlRoute("deploy.example.com", ControlListen)) {
		t.Fatalf("route JSON %s", raw)
	}
	for _, want := range []string{`"dial":"127.0.0.1:3000"`, `"host":["deploy.example.com"]`, `"/metrics"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("route lacks %s: %s", want, raw)
		}
	}
	if subjects := admin.strings("/config/apps/tls/automation/policies/0/subjects"); len(subjects) != 1 {
		t.Fatalf("subjects changed without tls: %v", subjects)
	}
	admin.reset()
	if _, err := m.publishControlRoute(context.Background(), "deploy.example.com", true); err != nil {
		t.Fatal(err)
	}
	if admin.called("PATCH /id/"+controlRouteID) != 0 {
		t.Fatal("an identical route was replaced")
	}
	subjects := admin.strings("/config/apps/tls/automation/policies/0/subjects")
	if len(subjects) != 2 || subjects[1] != "deploy.example.com" {
		t.Fatalf("subjects %v", subjects)
	}
}

func TestControlCaddyfileSiteBlock(t *testing.T) {
	original := "{\n\temail ops@example.com\n}\n\ndeploy.example.com {\n\t@internal path /metrics\n\trespond @internal 404\n\treverse_proxy 127.0.0.1:3000 {\n\t\tflush_interval -1\n\t}\n}\n\nshop.example.com {\n\treverse_proxy 127.0.0.1:8081\n}\n"
	next := controlCaddyfileSite(original, "deploy.example.com", "\trespond \"moved\" 503\n")
	if strings.Contains(next, "flush_interval") || !strings.Contains(next, "shop.example.com {") || !strings.Contains(next, "{\n\temail ops@example.com\n}") {
		t.Fatalf("replace:\n%s", next)
	}
	if !strings.Contains(next, controlCaddyfileBegin+"\ndeploy.example.com {\n\trespond \"moved\" 503\n}\n"+controlCaddyfileEnd) {
		t.Fatalf("marked block missing:\n%s", next)
	}
	again := controlCaddyfileSite(next, "deploy.example.com", controlProxyBody(ControlListen))
	if strings.Count(again, controlCaddyfileBegin) != 1 || strings.Contains(again, "moved") || !strings.Contains(again, "reverse_proxy 127.0.0.1:3000") {
		t.Fatalf("second edit:\n%s", again)
	}
	removed := controlCaddyfileSite(again, "deploy.example.com", "")
	if strings.Contains(removed, "deploy.example.com") || !strings.Contains(removed, "shop.example.com") {
		t.Fatalf("removal:\n%s", removed)
	}
}

// fakeControlHelper answers queued operations like the root helper does.
func fakeControlHelper(t *testing.T, m *Manager, outcome func(action string) (string, string)) *[]string {
	t.Helper()
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper")
	os.WriteFile(helper, []byte("#!/bin/bash\n# "+controlHelperMarker+"\n"), 0o700)
	oldPath, oldPoll := controlHelperPath, controlHelperPoll
	controlHelperPath, controlHelperPoll = helper, time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	seen := []string{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		request := filepath.Join(m.cfg.DataDir, "upgrade", "request")
		for ctx.Err() == nil {
			raw, err := os.ReadFile(request)
			if err == nil {
				os.Remove(request)
				action := strings.TrimSpace(string(raw))
				mu.Lock()
				seen = append(seen, action)
				mu.Unlock()
				state, message := outcome(action)
				status, _ := json.Marshal(ControlHelperStatus{Action: action, State: state, Message: message})
				os.WriteFile(filepath.Join(m.controlDir(), "status.json"), status, 0o600)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	t.Cleanup(func() { cancel(); <-done; controlHelperPath, controlHelperPoll = oldPath, oldPoll })
	return &seen
}

func TestControlHelperQueuesFixedOperations(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	seen := fakeControlHelper(t, m, func(action string) (string, string) {
		if action == controlActionStop {
			return "failed", "The control panel here has not handed over; it was not stopped."
		}
		return "succeeded", ""
	})
	ctx := context.Background()
	if err := m.controlHelper(ctx, controlActionCert); err != nil {
		t.Fatal(err)
	}
	if err := m.controlHelper(ctx, controlActionStop); err == nil || !strings.Contains(err.Error(), "not handed over") {
		t.Fatalf("helper failure: %v", err)
	}
	if len(*seen) != 2 || (*seen)[0] != controlActionCert {
		t.Fatalf("queued %v", *seen)
	}
	controlHelperPath = filepath.Join(t.TempDir(), "old-helper")
	os.WriteFile(controlHelperPath, []byte("#!/bin/bash\n"), 0o700)
	if err := m.controlHelper(ctx, controlActionApply); err == nil || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("old helper: %v", err)
	}
}

func TestControlRemoveWithdrawsRouteAndAsksHelper(t *testing.T) {
	admin := newFakeCaddyAdmin(t, operatorCaddyConfig())
	m := testManager(t, &fakeRuntime{})
	m.data.HostIngress = &HostIngressState{Caddy: &NativeCaddyConfig{Admin: admin.URL(), Server: "https"}}
	seen := fakeControlHelper(t, m, func(string) (string, string) { return "succeeded", "" })
	m.saveControlState(&ControlHostState{Hostname: "deploy.example.com", Listen: ControlListen, Phase: "standby"})
	ctx := context.Background()
	mode, err := m.publishControlRoute(ctx, "deploy.example.com", true)
	if err != nil || mode != "caddy" {
		t.Fatal(mode, err)
	}
	s := m.controlState()
	s.Route, s.TLSSubject = mode, true
	m.saveControlState(s)
	if err := m.removeControl(ctx); err != nil {
		t.Fatal(err)
	}
	if found, _ := findCaddyID(admin.snapshot(), controlRouteID, nil); found != nil {
		t.Fatal("the route stayed")
	}
	if subjects := admin.strings("/config/apps/tls/automation/policies/0/subjects"); len(subjects) != 1 || subjects[0] != "ops.example.com" {
		t.Fatalf("subjects %v", subjects)
	}
	if len(*seen) != 1 || (*seen)[0] != controlActionRemove {
		t.Fatalf("helper %v", *seen)
	}
	if _, err := os.Stat(m.controlDir()); !os.IsNotExist(err) {
		t.Fatal("the stage stayed")
	}
}

func TestControlPanelProxiedByAgentIngress(t *testing.T) {
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "panel "+r.Host+" "+r.URL.Path)
	}))
	defer panel.Close()
	m := testManager(t, &fakeRuntime{})
	u, _ := url.Parse(panel.URL)
	m.saveControlState(&ControlHostState{Hostname: "deploy.example.com", Listen: u.Host, Phase: "standby", Route: "agent"})
	proxy := Proxy(m)
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, httptest.NewRequest("GET", "http://deploy.example.com/console", nil))
	if w.Body.String() != "panel deploy.example.com /console" {
		t.Fatalf("proxied %d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	proxy.ServeHTTP(w, httptest.NewRequest("GET", "http://deploy.example.com/metrics", nil))
	if w.Code != 404 {
		t.Fatalf("internal path %d", w.Code)
	}
	if !m.controlDomainAllowed("deploy.example.com") || m.controlDomainAllowed("other.example.com") {
		t.Fatal("on-demand TLS permission")
	}
}

func TestControlDatabaseRestoreSequence(t *testing.T) {
	id, name := ControlDatabaseContainer(ControlDataDir)
	var calls []string
	var restored []byte
	old := controlDocker
	controlDocker = func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "inspect" {
			return []byte(id + "\n"), nil
		}
		if stdin != nil && strings.Contains(strings.Join(args, " "), "pg_restore") {
			restored, _ = io.ReadAll(stdin)
		}
		return nil, nil
	}
	t.Cleanup(func() { controlDocker = old })
	if err := restoreControlDatabase(context.Background(), strings.NewReader("PGDMP archive")); err != nil {
		t.Fatal(err)
	}
	if string(restored) != "PGDMP archive" || len(calls) != 4 || !strings.HasPrefix(calls[2], "exec -i "+name+" psql") || !strings.Contains(calls[3], "pg_restore -U lidza -d lidza_control --no-owner --single-transaction") {
		t.Fatalf("calls %v restored %q", calls, restored)
	}
	controlDocker = func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		return []byte("someone-else"), nil
	}
	if err := restoreControlDatabase(context.Background(), strings.NewReader("x")); err == nil {
		t.Fatal("a foreign container was restored into")
	}
}

func TestControlAgentURLForTheMovedInPanel(t *testing.T) {
	for listen, want := range map[string]string{"127.0.0.1:9092": "http://127.0.0.1:9092", ":9090": "http://127.0.0.1:9090", "0.0.0.0:9090": "http://127.0.0.1:9090", "[::]:9090": "http://127.0.0.1:9090"} {
		if got := controlAgentURL(listen); got != want {
			t.Fatalf("%s: %s", listen, got)
		}
	}
}
