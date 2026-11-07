package control

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/buildinfo"
	"github.com/agim/lidza/pkg/credentials"
)

const panelTestToken = "0123456789abcdef0123456789abcdef"

// panelAgentFake is a hosting agent for control panel moves.
type panelAgentFake struct {
	mu       sync.Mutex
	srv      *httptest.Server
	status   agent.ControlHostStatus
	host     agent.MoveHost
	calls    []string
	files    []byte
	database []byte
	applied  []map[string]any
	routes   []map[string]any
	cert     *agent.ControlCertificate
	placed   *agent.ControlCertificate
	reach    map[string]bool
	failNext map[string]int
}

func newPanelAgent(t *testing.T, machine string) *panelAgentFake {
	f := &panelAgentFake{reach: map[string]bool{}, failNext: map[string]int{}}
	f.status = agent.ControlHostStatus{Supported: true, Version: buildinfo.Version, Arch: "amd64", MachineID: machine, Listen: agent.ControlListen, ListenFree: true, BundleStaged: true, Ingress: "caddy", IngressDetail: "a route through the Caddy admin API", CertificateStorage: "/var/lib/caddy/.local/share/caddy", Docker: true}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	f.status.AgentURL = "http://127.0.0.1:9092"
	t.Cleanup(f.srv.Close)
	return f
}

func (f *panelAgentFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, call)
	if f.failNext[call] > 0 {
		f.failNext[call]--
		agent.Fail(w, 502, errors.New("transient failure"))
		return
	}
	body, _ := io.ReadAll(r.Body)
	switch call {
	case "GET /v1/host/control":
		agent.JSON(w, 200, f.status)
	case "GET /v1/move/host":
		agent.JSON(w, 200, f.host)
	case "POST /v1/host/control/probe":
		var in struct{ URL string }
		json.Unmarshal(body, &in)
		if f.reach[in.URL] {
			agent.JSON(w, 200, map[string]any{"reachable": true})
		} else {
			agent.JSON(w, 200, map[string]any{"reachable": false, "detail": "no answer"})
		}
	case "PUT /v1/host/control/files":
		f.files = body
		tr := tar.NewReader(bytes.NewReader(body))
		n := 0
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			if h.Typeflag == tar.TypeReg {
				n++
			}
		}
		agent.JSON(w, 200, map[string]int{"files": n})
	case "POST /v1/host/control/apply":
		var in map[string]any
		json.Unmarshal(body, &in)
		f.applied = append(f.applied, in)
		phase := "active"
		if in["standby"] == true {
			phase = "standby"
		}
		f.status.Installed, f.status.Owned = true, true
		f.status.State = &agent.ControlHostState{Hostname: in["hostname"].(string), Phase: phase}
		agent.JSON(w, 200, f.status)
	case "PUT /v1/host/control/database":
		f.database = body
		agent.JSON(w, 200, map[string]string{"status": "restored"})
	case "GET /v1/host/control/certificate":
		if f.cert == nil {
			agent.Fail(w, 404, errors.New("no certificate"))
			return
		}
		cert := *f.cert
		if r.URL.Query().Get("check") == "1" {
			cert.Cert, cert.Key = "", ""
		}
		agent.JSON(w, 200, cert)
	case "PUT /v1/host/control/certificate":
		var in agent.ControlCertificate
		json.Unmarshal(body, &in)
		f.placed = &in
		agent.JSON(w, 200, map[string]string{"storage": f.status.CertificateStorage, "issuer": in.Issuer})
	case "PUT /v1/host/control/route":
		var in map[string]any
		json.Unmarshal(body, &in)
		f.routes = append(f.routes, in)
		agent.JSON(w, 200, map[string]string{"mode": "caddy", "detail": "a route through the Caddy admin API"})
	case "POST /v1/host/control/retire":
		agent.JSON(w, 200, map[string]any{"notes": []string{"The control panel unit is stopped and disabled."}})
	case "DELETE /v1/host/control":
		f.status.Installed, f.status.Owned, f.status.State = false, false, nil
		agent.JSON(w, 200, map[string]string{"status": "removed"})
	default:
		http.NotFound(w, r)
	}
}

func (f *panelAgentFake) called(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

type panelFixture struct {
	c        *Control
	source   *panelAgentFake
	dest     *panelAgentFake
	remote   *panelAgentFake
	resolver *moveResolverFake
	probe    *moveProbeFake
	dumped   int
}

func newPanelFixture(t *testing.T) *panelFixture {
	t.Helper()
	fx := &panelFixture{source: newPanelAgent(t, "machine-old"), dest: newPanelAgent(t, "machine-new"), remote: newPanelAgent(t, "machine-remote"), resolver: &moveResolverFake{answers: map[string][]string{}}, probe: &moveProbeFake{}}
	fx.dest.host = agent.MoveHost{IPv4: moveTestTargetIP}
	fx.source.cert = &agent.ControlCertificate{Host: "deploy.example.com", Issuer: "acme-v02.api.letsencrypt.org-directory", Name: "deploy.example.com", Cert: "Y2VydA==", Key: "a2V5"}
	dir := t.TempDir()
	key := []byte(strings.Repeat("k", 32))
	cfg := Config{PublicURL: "https://deploy.example.com", User: "operator@example.com", Key: key, DataDir: dir, Servers: []Server{
		{ID: "old", Name: "Old host", URL: fx.source.srv.URL, Token: panelTestToken},
		{ID: "combined", Name: "Combined", URL: fx.dest.srv.URL, Token: panelTestToken},
		{ID: "edge", Name: "Edge", URL: "https://edge-agent.example.net", Token: panelTestToken},
	}}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The panel move loop must end with the test (move_work.go).
	t.Cleanup(c.stopMoveWork)
	c.panelMachineID = func() string { return "machine-old" }
	c.panelPollInterval, c.panelRetryDelays = time.Millisecond, []time.Duration{time.Millisecond}
	c.moveResolver, c.moveProbe = fx.resolver, fx.probe.probe
	c.panelDatabase = func(context.Context) (bool, int64, error) { return true, 4096, nil }
	c.panelDumpFixture = func(_ context.Context, w io.Writer) error {
		fx.dumped++
		_, err := io.WriteString(w, "PGDMP panel database")
		return err
	}
	fx.dest.reach["https://edge-agent.example.net"] = true
	c.data.Apps["shop"] = Application{ID: "shop", ServerID: "combined", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com"}
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "config"), 0o700)
	os.WriteFile(filepath.Join(dir, "config", "master.key"), []byte(strings.Repeat("ab", 32)), 0o600)
	os.WriteFile(filepath.Join(dir, "setup-complete.json"), []byte("true"), 0o600)
	fx.resolver.set("deploy.example.com", moveTestSourceIP)
	fx.c = c
	return fx
}

func waitPanel(t *testing.T, c *Control, states ...string) PanelMove {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		move, ok := c.panelMove()
		c.mu.Lock()
		running := c.panelRunning
		c.mu.Unlock()
		// The DNS wait keeps its runner polling.
		if ok && slices.Contains(states, move.State) && (!running || move.State == panelMoveDNS) {
			return move
		}
		time.Sleep(2 * time.Millisecond)
	}
	move, _ := c.panelMove()
	t.Fatalf("panel move stayed %s (%s), want %v: %+v", move.State, move.Error, states, move.Steps)
	return move
}

func panelPost(t *testing.T, c *Control, action string, body any) (int, string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	path := "/api/control/panel-move"
	if action != "" {
		path += "/" + action
	}
	req := httptest.NewRequest("POST", path, bytes.NewReader(raw))
	w := httptest.NewRecorder()
	if action == "" {
		c.panelMoveStart(w, req)
	} else {
		req.SetPathValue("action", action)
		c.panelMoveAction(w, req)
	}
	return w.Code, w.Body.String()
}

func TestPanelServersRewriteForDestination(t *testing.T) {
	data := saved{Apps: map[string]Application{"shop": {ID: "shop", ServerID: "combined"}}, Servers: []Server{
		{ID: "old", URL: "http://127.0.0.1:9090", Token: panelTestToken},
		{ID: "combined", URL: "http://127.0.0.1:19092", Token: panelTestToken},
		{ID: "edge", URL: "http://127.0.0.1:19093", Token: panelTestToken},
	}, Updates: UpdateSettings{Targets: []UpdateTarget{{ServerID: "old", Local: true}, {ServerID: "combined"}}}, PanelMove: &PanelMove{ID: "m", ReadOnly: true}}
	plan := PanelMovePlan{SourceName: "Old host", Servers: []PanelMoveServer{
		{ID: "combined", Role: "destination"},
		{ID: "old", Name: "Old host", Role: "source", Retire: true},
		{ID: "edge", Role: "remote", NewURL: "https://edge-agent.example.net"},
	}}
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	out, err := panelRewriteState(data, panelRewrite{Target: "combined", TargetURL: "http://127.0.0.1:9092", Plan: plan, Hostname: "deploy.example.com", At: at})
	if err != nil {
		t.Fatal(err)
	}
	want := []Server{{ID: "combined", URL: "http://127.0.0.1:9092", Token: panelTestToken}, {ID: "edge", URL: "https://edge-agent.example.net", Token: panelTestToken}}
	if !slices.Equal(out.Servers, want) {
		t.Fatalf("servers %+v", out.Servers)
	}
	if out.PanelMove != nil || out.PanelArrival == nil || out.PanelArrival.From != "Old host" || len(out.PanelArrival.Retired) != 1 || out.PanelArrival.Retired[0].ID != "old" {
		t.Fatalf("arrival %+v move %+v", out.PanelArrival, out.PanelMove)
	}
	if len(out.Updates.Targets) != 1 || out.Updates.Targets[0].ServerID != "combined" {
		t.Fatalf("update targets %+v", out.Updates.Targets)
	}
	if out.Apps["shop"].ServerID != "combined" {
		t.Fatal("application lost its server")
	}
	// The input is not changed and a retired source with applications is refused.
	if data.Servers[1].URL != "http://127.0.0.1:19092" {
		t.Fatal("the input state changed")
	}
	data.Apps["legacy"] = Application{ID: "legacy", ServerID: "old"}
	if _, err := panelRewriteState(data, panelRewrite{Target: "combined", TargetURL: "http://127.0.0.1:9092", Plan: plan}); err == nil {
		t.Fatal("a retired server holding an application was dropped")
	}
	// A kept source gets its reachable URL.
	plan.Servers[1] = PanelMoveServer{ID: "old", Role: "source", NewURL: "https://old-agent.example.com"}
	out, err = panelRewriteState(data, panelRewrite{Target: "combined", TargetURL: "http://127.0.0.1:9092", Plan: plan})
	if err != nil || out.Servers[0].URL != "https://old-agent.example.com" {
		t.Fatalf("kept source %+v %v", out.Servers, err)
	}
}

func TestStandbyRefusesTicksAndJobs(t *testing.T) {
	c := &Control{cfg: Config{Standby: true}}
	ran := 0
	tick := c.gatedJob(true, func(context.Context, json.RawMessage) error { ran++; return nil })
	job := c.gatedJob(false, func(context.Context, json.RawMessage) error { ran++; return nil })
	if err := tick(context.Background(), nil); err != nil || ran != 0 {
		t.Fatalf("standby tick ran (%d) or failed: %v", ran, err)
	}
	if err := job(context.Background(), nil); err == nil || ran != 0 {
		t.Fatalf("standby job ran (%d) or succeeded", ran)
	}
	if err := c.Start(context.Background(), nil); err != nil {
		t.Fatalf("standby start: %v", err)
	}
	c.cfg.Standby = false
	if err := tick(context.Background(), nil); err != nil || ran != 1 {
		t.Fatalf("active tick ran %d: %v", ran, err)
	}
	c.data.PanelMove = &PanelMove{ReadOnly: true}
	if err := tick(context.Background(), nil); err != nil || ran != 1 {
		t.Fatalf("read-only tick ran %d", ran)
	}
	for _, h := range []http.Handler{StandbyHandler(), MovedHandler("https://deploy.example.com")} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
		if w.Code != 200 {
			t.Fatalf("readyz %d", w.Code)
		}
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/api/control/apps", nil))
		if w.Code != 503 || !strings.Contains(w.Body.String(), "moving") {
			t.Fatalf("api %d %s", w.Code, w.Body.String())
		}
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/console", nil))
		if w.Code != 503 || !strings.Contains(w.Body.String(), "control panel") {
			t.Fatalf("page %d", w.Code)
		}
	}
	dir := t.TempDir()
	if _, moved := MovedTo(dir); moved {
		t.Fatal("moved without a marker")
	}
	os.WriteFile(filepath.Join(dir, agent.ControlMovedFile), []byte(`{"moved_to":"https://deploy.example.com"}`), 0o600)
	if to, moved := MovedTo(dir); !moved || to != "https://deploy.example.com" {
		t.Fatalf("marker %q %v", to, moved)
	}
}

func TestPanelReadOnlyMiddlewareRefusesEveryMutation(t *testing.T) {
	c := &Control{data: saved{PanelMove: &PanelMove{ReadOnly: true}}}
	checked := 0
	c.routes(func(pattern string, fn http.HandlerFunc) {
		method, path, _ := strings.Cut(pattern, " ")
		reached := false
		guarded := c.panelReadOnlyGuard(pattern, func(w http.ResponseWriter, r *http.Request) { reached = true })
		w := httptest.NewRecorder()
		guarded(w, httptest.NewRequest(method, strings.NewReplacer("{", "", "}", "").Replace(path), nil))
		mutating := method != "GET" && method != "HEAD"
		switch {
		case panelMoveRoute(pattern):
			if !reached {
				t.Fatalf("%s is the move's own route and was refused", pattern)
			}
		case mutating && (reached || w.Code != 503 || !strings.Contains(w.Body.String(), "the control panel is moving")):
			t.Fatalf("%s answered %d %s", pattern, w.Code, w.Body.String())
		case !mutating && !reached:
			t.Fatalf("%s is a read and was refused", pattern)
		}
		checked++
	})
	if checked < 50 {
		t.Fatalf("only %d routes checked", checked)
	}
	// Webhooks and agent reports outside the authenticated API too.
	h := c.Handler(http.NotFoundHandler())
	for _, path := range []string{"/hooks/github/shop", "/hooks/github-app", "/api/agent/errors", "/api/agent/checkout-token"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader("{}")))
		if w.Code != 503 || w.Header().Get("Retry-After") == "" {
			t.Fatalf("%s answered %d", path, w.Code)
		}
	}
	c.data.PanelMove.ReadOnly = false
	w := httptest.NewRecorder()
	c.panelReadOnlyGuard("POST /api/control/apps", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) })(w, httptest.NewRequest("POST", "/api/control/apps", nil))
	if w.Code != 201 {
		t.Fatalf("writable panel refused: %d", w.Code)
	}
}

func TestPanelSnapshotCopiesRewrittenState(t *testing.T) {
	dir := t.TempDir()
	key := []byte(strings.Repeat("k", 32))
	for name, body := range map[string]string{"control.enc.json": `"old"`, "config/master.key": "hex", "config/credentials.yml.enc": "sealed", "setup-complete.json": "true", "servers.json": `[]`, "updates/v1/bundle.tar.gz": "big", "setup-token": "secret", ".postgres-env-1": "pw", ".state-123": "tmp", agent.ControlMovedFile: "{}"} {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700)
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)
	}
	state := saved{Apps: map[string]Application{}, Servers: []Server{{ID: "combined", URL: "http://127.0.0.1:9092", Token: panelTestToken}}}
	archive, files, err := panelSnapshot(dir, key, state)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Typeflag == tar.TypeReg {
			body, _ := io.ReadAll(tr)
			got[h.Name] = body
			if h.Mode != 0o600 {
				t.Fatalf("%s mode %o", h.Name, h.Mode)
			}
		}
	}
	names := []string{}
	for name := range got {
		names = append(names, name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"config/credentials.yml.enc", "config/master.key", "control.enc.json", "servers.json", "setup-complete.json"}) || files != 5 {
		t.Fatalf("files %v (%d)", names, files)
	}
	var sealed string
	json.Unmarshal(got["control.enc.json"], &sealed)
	plain, err := credentials.Decrypt(key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	var back saved
	json.Unmarshal(plain, &back)
	if len(back.Servers) != 1 || back.Servers[0].URL != "http://127.0.0.1:9092" {
		t.Fatalf("state %+v", back.Servers)
	}
	var seed []Server
	json.Unmarshal(got["servers.json"], &seed)
	if len(seed) != 1 || seed[0].Token != panelTestToken {
		t.Fatalf("seed %+v", seed)
	}
	if n, size, _ := panelDataSize(dir); n != 5 || size == 0 {
		t.Fatalf("size %d %d", n, size)
	}
}

func TestPanelEnvironmentForwardsOnlyPanelSettings(t *testing.T) {
	env := panelEnvironment([]string{"PUBLIC_URL=https://deploy.example.com", "CONTROL_USER=op@example.com", "CONTROL_SERVERS_FILE=/srv/panel/servers.json", "LIDZA_ADDR=127.0.0.1:3000", "CONTROL_DATA_DIR=/srv/panel", "LIDZA_TRUSTED_PROXIES=loopback", "PATH=/usr/bin", "HOME=/root", "AUTH_SECRET=s", "INVOCATION_ID=x", "CONTROL_STANDBY=1"}, "/srv/panel")
	want := map[string]string{"PUBLIC_URL": "https://deploy.example.com", "CONTROL_USER": "op@example.com", "CONTROL_SERVERS_FILE": "/var/lib/lidza-control/servers.json", "AUTH_SECRET": "s"}
	if len(env) != len(want) {
		t.Fatalf("env %v", env)
	}
	for k, v := range want {
		if env[k] != v {
			t.Fatalf("%s = %q", k, env[k])
		}
	}
}

func TestPanelMovePlanChecksDestinationAndServers(t *testing.T) {
	fx := newPanelFixture(t)
	ctx := context.Background()
	fx.dest.reach = map[string]bool{}
	plan, err := fx.c.panelMovePlan(ctx, PanelMoveInput{Target: "combined"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Ready || plan.SourceServer != "old" || len(plan.Blockers) != 1 || !strings.Contains(plan.Blockers[0], "Edge") {
		t.Fatalf("plan %+v", plan.Blockers)
	}
	fx.dest.reach["https://edge-agent.example.org"] = true
	plan, _ = fx.c.panelMovePlan(ctx, PanelMoveInput{Target: "combined", ServerURLs: map[string]string{"edge": "https://edge-agent.example.org"}})
	if !plan.Ready || plan.Database != "managed" || !plan.CertificateOK || !slices.Equal(plan.Addresses, []string{moveTestTargetIP}) {
		t.Fatalf("plan %+v", plan)
	}
	for _, s := range plan.Servers {
		if s.ID == "old" && !s.Retire {
			t.Fatalf("source not retired: %+v", s)
		}
	}
	fx.dest.status.Version = "v0.0.1-private.1"
	fx.dest.status.ListenFree = false
	plan, _ = fx.c.panelMovePlan(ctx, PanelMoveInput{Target: "combined", ServerURLs: map[string]string{"edge": "https://edge-agent.example.org"}})
	if plan.Ready || len(plan.Blockers) != 2 {
		t.Fatalf("blockers %v", plan.Blockers)
	}
	if _, err := fx.c.panelMovePlan(ctx, PanelMoveInput{Target: "old"}); err == nil {
		t.Fatal("moving onto the panel's own server was planned")
	}
}

func TestPanelMoveRunsPrepareCutoverAndDNS(t *testing.T) {
	fx := newPanelFixture(t)
	c := fx.c
	fx.dest.failNext["PUT /v1/host/control/database"] = 1 // retried
	code, body := panelPost(t, c, "", PanelMoveInput{Target: "combined"})
	if code != 202 {
		t.Fatalf("start %d %s", code, body)
	}
	move := waitPanel(t, c, panelMoveStandby, panelMoveFailed)
	if move.State != panelMoveStandby || move.ReadOnly {
		t.Fatalf("prepare %s %s %+v", move.State, move.Error, move.Steps)
	}
	if len(fx.dest.applied) != 1 || fx.dest.applied[0]["standby"] != true || string(fx.dest.database) != "PGDMP panel database" || fx.dest.placed == nil || fx.dest.placed.Key != "a2V5" {
		t.Fatalf("destination applied %v db %q cert %+v", fx.dest.applied, fx.dest.database, fx.dest.placed)
	}
	if len(fx.dest.routes) != 1 || fx.dest.routes[0]["tls"] != true {
		t.Fatalf("route %v (a copied certificate lets Caddy manage it at once)", fx.dest.routes)
	}
	if move.step(panelStepDatabase).Attempts != 2 {
		t.Fatalf("database attempts %d", move.step(panelStepDatabase).Attempts)
	}
	// The copied state points at the destination's loopback agent.
	tr := tar.NewReader(bytes.NewReader(fx.dest.files))
	var sealed string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Name == "control.enc.json" {
			raw, _ := io.ReadAll(tr)
			json.Unmarshal(raw, &sealed)
		}
	}
	plain, err := credentials.Decrypt(c.cfg.Key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	var copied saved
	json.Unmarshal(plain, &copied)
	if copied.Servers[0].ID != "combined" || copied.Servers[0].URL != "http://127.0.0.1:9092" || copied.PanelMove != nil || copied.PanelArrival == nil {
		t.Fatalf("copied servers %+v arrival %+v", copied.Servers, copied.PanelArrival)
	}
	env := fx.dest.applied[0]["env"].(map[string]any)
	if _, ok := env["LIDZA_ADDR"]; ok {
		t.Fatal("the host address was forwarded")
	}

	if code, _ := panelPost(t, c, "cutover", nil); code != 200 {
		t.Fatalf("cutover %d", code)
	}
	move = waitPanel(t, c, panelMoveDNS, panelMoveFailed)
	if move.State != panelMoveDNS || !move.ReadOnly || move.FinalCopied == nil || fx.dumped != 3 { // prepare retried once, then the final copy
		t.Fatalf("cutover %s %s read-only %v dumps %d", move.State, move.Error, move.ReadOnly, fx.dumped)
	}
	if last := fx.dest.applied[len(fx.dest.applied)-1]; last["standby"] != false {
		t.Fatalf("destination not activated: %v", last)
	}
	if code, _ := panelPost(t, c, "abort", nil); code != 409 {
		t.Fatal("abort after the cutover was accepted")
	}
	// DNS still points to the old host: the move waits.
	c.panelCheckDNS(context.Background(), true)
	if move, _ = c.panelMove(); move.State != panelMoveDNS || len(move.DNS) != 1 || move.DNS[0].Ready {
		t.Fatalf("dns %+v", move.DNS)
	}
	// DNS moves; HTTPS answers: the panel hands over.
	fx.resolver.set("deploy.example.com", moveTestTargetIP)
	fx.probe.set(true)
	move = waitPanel(t, c, panelMoveDone)
	if !move.TLSReady || move.step(panelStepRetire).Status != "done" {
		t.Fatalf("done %+v", move.Steps)
	}
	if to, moved := MovedTo(c.cfg.DataDir); !moved || to != "https://deploy.example.com" {
		t.Fatalf("moved marker %q %v", to, moved)
	}
	deadline := time.Now().Add(2 * time.Second)
	for fx.source.called("POST /v1/host/control/retire") == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if fx.source.called("POST /v1/host/control/retire") != 1 {
		t.Fatal("the old host's agent was not asked to retire the panel")
	}
	items := c.panelMoveActivity(time.Now())
	if len(items) != 1 || items[0].Kind != "panel-move" || items[0].State != "succeeded" {
		t.Fatalf("activity %+v", items)
	}
}

func TestPanelMoveDNSRequestsCertificateOnlyOncePointed(t *testing.T) {
	fx := newPanelFixture(t)
	c := fx.c
	fx.source.cert = nil // nothing to copy: Caddy issues once DNS points
	if code, body := panelPost(t, c, "", PanelMoveInput{Target: "combined"}); code != 202 {
		t.Fatal(code, body)
	}
	move := waitPanel(t, c, panelMoveStandby, panelMoveFailed)
	if move.step(panelStepCert).Status != "skipped" || len(fx.dest.routes) != 1 || fx.dest.routes[0]["tls"] != false {
		t.Fatalf("cert %+v routes %v", move.step(panelStepCert), fx.dest.routes)
	}
	c.panelPollInterval = time.Hour // checks run only when asked
	panelPost(t, c, "cutover", nil)
	waitPanel(t, c, panelMoveDNS)
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if move, _ := c.panelMove(); move.DNSChecked != nil {
			break // the runner's first check ran; it now waits an hour
		}
	}
	c.panelCheckDNS(context.Background(), false)
	if len(fx.dest.routes) != 1 {
		t.Fatal("the certificate was requested before DNS pointed to the destination")
	}
	fx.resolver.set("deploy.example.com", moveTestTargetIP)
	c.panelCheckDNS(context.Background(), false)
	if len(fx.dest.routes) != 2 || fx.dest.routes[1]["tls"] != true {
		t.Fatalf("routes %v", fx.dest.routes)
	}
	if move, _ = c.panelMove(); move.State != panelMoveDNS || move.TLSReady {
		t.Fatalf("finished without HTTPS: %s", move.State)
	}
	fx.probe.set(true)
	if !c.panelCheckDNS(context.Background(), false) {
		t.Fatal("not finished with DNS and HTTPS ready")
	}
}

func TestPanelMoveAbortBeforeCutover(t *testing.T) {
	fx := newPanelFixture(t)
	c := fx.c
	if code, body := panelPost(t, c, "", PanelMoveInput{Target: "combined"}); code != 202 {
		t.Fatal(code, body)
	}
	waitPanel(t, c, panelMoveStandby)
	if code, body := panelPost(t, c, "abort", nil); code != 200 {
		t.Fatal(code, body)
	}
	move, _ := c.panelMove()
	if move.State != panelMoveAborted || move.ReadOnly || fx.dest.called("DELETE /v1/host/control") != 1 || fx.dest.status.Installed {
		t.Fatalf("abort %s read-only %v", move.State, move.ReadOnly)
	}
	if c.panelReadOnly() || !c.ticksAllowed() {
		t.Fatal("the panel did not return to normal")
	}
	// A new move can start after an abort.
	if code, body := panelPost(t, c, "", PanelMoveInput{Target: "combined"}); code != 202 {
		t.Fatal(code, body)
	}
	waitPanel(t, c, panelMoveStandby)
}
