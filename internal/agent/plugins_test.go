package agent

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
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/plugin"
)

const pluginScript = `#!/bin/sh
request=$(cat)
case "$1" in
 describe) printf '{"ok":true,"messages":["example %s"]}' "$PLUGIN_BROKEN"; [ ! -f broken ] || { printf '{"ok":false,"messages":["cannot start"]}' > /dev/null; exit 0; };;
 status) printf '{"ok":true,"output":{"state":"%s","flags":%s}}' "$(pwd)" "$(printf '%s' "$request" | grep -c cpu_flags)";;
 check) printf '{"ok":true,"checks":[{"id":"disk","title":"Disk","ok":true,"detail":"enough"}]}';;
 provision) printf '{"ok":true}';;
esac
`

func pluginRelease(t *testing.T, key ed25519.PrivateKey, version, script string) (manifest string, signature json.RawMessage, archive []byte) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: plugin.Program, Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg})
	tw.Write([]byte(script))
	tw.Close()
	gz.Close()
	archive = buf.Bytes()
	sum := sha256.Sum256(archive)
	raw, _ := json.Marshal(plugin.Manifest{Contract: plugin.Contract, Name: "example", Version: version, Provides: "example", Summary: "An example service.",
		Hooks: []string{"describe", "status", "check", "provision"}, Environment: []string{"EXAMPLE_URL"},
		Archives: []plugin.Archive{{Arch: runtime.GOARCH, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(archive))}}})
	signature, _ = json.Marshal(plugin.Sign(key, raw))
	return string(raw), signature, archive
}

func TestPluginIsInstalledOnlyFromAKeyTheServerTrustsAndRunsReadOnlyHooks(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	mux := http.NewServeMux()
	m.pluginRoutes(mux)
	do := func(method, path, contentType string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	jsonBody := func(v any) []byte { raw, _ := json.Marshal(v); return raw }
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	manifest, signature, archive := pluginRelease(t, private, "1.0", pluginScript)
	stage := jsonBody(map[string]any{"manifest": manifest, "signature": signature})

	// The plugin's key is not trusted yet: nothing is accepted. (A build that
	// pins update keys trusts those, but not this one.)
	if w := do("POST", "/v1/plugins/manifest", "application/json", stage); w.Code != 409 || !(strings.Contains(w.Body.String(), "trusts no plugin signing key") || strings.Contains(w.Body.String(), "does not trust")) {
		t.Fatalf("untrusted server: %d %s", w.Code, w.Body)
	}
	if w := do("PUT", "/v1/plugins/keys", "application/json", jsonBody(map[string]any{"keys": []string{"not-a-key"}})); w.Code != 400 {
		t.Fatalf("invalid key: %d", w.Code)
	}
	if w := do("PUT", "/v1/plugins/keys", "application/json", jsonBody(map[string]any{"keys": []string{hex.EncodeToString(public)}})); w.Code != 200 || !strings.Contains(w.Body.String(), plugin.KeyID(public)) {
		t.Fatalf("trust key: %d %s", w.Code, w.Body)
	}
	// The archive is refused before its manifest was verified.
	if w := do("PUT", "/v1/plugins/example/archive", "application/octet-stream", archive); w.Code != 409 || !strings.Contains(w.Body.String(), "manifest first") {
		t.Fatalf("archive without manifest: %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/v1/plugins/manifest", "application/json", stage); w.Code != 200 || !strings.Contains(w.Body.String(), `"version":"1.0"`) {
		t.Fatalf("manifest: %d %s", w.Code, w.Body)
	}
	// A different archive than the one that was signed is not installed.
	if w := do("PUT", "/v1/plugins/example/archive", "application/octet-stream", append(append([]byte{}, archive...), 'x')); w.Code != 409 || !strings.Contains(w.Body.String(), "does not match its signed manifest") {
		t.Fatalf("tampered archive: %d %s", w.Code, w.Body)
	}
	if w := do("PUT", "/v1/plugins/example/archive", "application/octet-stream", archive); w.Code != 201 {
		t.Fatalf("install: %d %s", w.Code, w.Body)
	}
	var view PluginsView
	json.Unmarshal(do("GET", "/v1/plugins", "", nil).Body.Bytes(), &view)
	if len(view.Plugins) != 1 || view.Plugins[0].Version != "1.0" || view.Plugins[0].Provides != "example" || view.Plugins[0].KeyID != plugin.KeyID(public) || view.Arch != runtime.GOARCH {
		t.Fatalf("view %+v", view)
	}
	// Read-only hooks run on request, in the plugin's own state directory, with the server's facts.
	w := do("POST", "/v1/plugins/example/hooks/status", "application/json", []byte(`{}`))
	var status struct {
		Answer struct {
			OK     bool
			Output struct {
				State string
				Flags int
			}
		}
	}
	json.Unmarshal(w.Body.Bytes(), &status)
	if w.Code != 200 || !status.Answer.OK || status.Answer.Output.State != m.pluginState("example") {
		t.Fatalf("status hook: %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/v1/plugins/example/hooks/check", "application/json", []byte(`{"postgres_bytes":1}`)); w.Code != 200 || !strings.Contains(w.Body.String(), `"detail":"enough"`) {
		t.Fatalf("check hook: %d %s", w.Code, w.Body)
	}
	// Hooks that change the server are not run on request.
	if w := do("POST", "/v1/plugins/example/hooks/provision", "application/json", []byte(`{}`)); w.Code != 400 {
		t.Fatalf("a changing hook ran on request: %d %s", w.Code, w.Body)
	}
	// A newer version replaces the old program.
	manifest2, signature2, archive2 := pluginRelease(t, private, "1.1", pluginScript)
	do("POST", "/v1/plugins/manifest", "application/json", jsonBody(map[string]any{"manifest": manifest2, "signature": signature2}))
	if w := do("PUT", "/v1/plugins/example/archive", "application/octet-stream", archive2); w.Code != 201 {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(plugin.Dir(m.pluginRoot(), "example", "1.0")); !os.IsNotExist(err) {
		t.Fatal("the replaced version was kept")
	}
	// A plugin that cannot describe itself is not kept; the installed one stays.
	manifest3, signature3, archive3 := pluginRelease(t, private, "1.2", "#!/bin/sh\ncat >/dev/null\necho 'not json'\n")
	do("POST", "/v1/plugins/manifest", "application/json", jsonBody(map[string]any{"manifest": manifest3, "signature": signature3}))
	if w := do("PUT", "/v1/plugins/example/archive", "application/octet-stream", archive3); w.Code != 409 || !strings.Contains(w.Body.String(), "did not answer with JSON") {
		t.Fatalf("broken plugin: %d %s", w.Code, w.Body)
	}
	json.Unmarshal(do("GET", "/v1/plugins", "", nil).Body.Bytes(), &view)
	if len(view.Plugins) != 1 || view.Plugins[0].Version != "1.1" {
		t.Fatalf("after a broken update: %+v", view.Plugins)
	}
	if _, err := os.Stat(plugin.Dir(m.pluginRoot(), "example", "1.2")); !os.IsNotExist(err) {
		t.Fatal("a plugin that could not describe itself was left installed")
	}
	// Removing keeps the state directory: it may hold what the plugin manages.
	os.WriteFile(filepath.Join(m.pluginState("example"), "data"), []byte("kept"), 0o600)
	if w := do("DELETE", "/v1/plugins/example", "", nil); w.Code != 200 {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(m.pluginRoot(), "example")); !os.IsNotExist(err) {
		t.Fatal("the removed plugin's program stayed")
	}
	if raw, _ := os.ReadFile(filepath.Join(m.pluginState("example"), "data")); string(raw) != "kept" {
		t.Fatal("the plugin's state was deleted with it")
	}
	if w := do("POST", "/v1/plugins/example/hooks/status", "application/json", []byte(`{}`)); w.Code != 409 {
		t.Fatalf("hook of a removed plugin: %d", w.Code)
	}
	_ = fmt.Sprint
}

// servicePlugin provisions a "search" service: it remembers the applications
// it serves in its state directory, answers environment, a record and a
// secret, and expects the record and the secret back when it releases.
const servicePlugin = `#!/bin/sh
request=$(cat)
field() { printf '%s' "$request" | sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"; }
app=$(field app)
case "$1" in
 describe) printf '{"ok":true}';;
 check) if [ -f full ]; then printf '{"ok":false,"checks":[{"id":"disk","title":"Search disk","ok":false,"detail":"the index does not fit"}]}'; else printf '{"ok":true,"checks":[{"id":"disk","title":"Search disk","ok":true,"detail":"room for the index"}]}'; fi;;
 provision)
  [ ! -f refuse ] || { printf '{"ok":false,"messages":["the search server is not running"]}'; exit 0; }
  echo "$app" >> served
  extra=''; [ ! -f rogue ] || extra=',"PATH":"/tmp"'
  printf '{"ok":true,"output":{"env":{"SEARCH_URL":"http://127.0.0.1:7700/%s"%s},"record":{"index":"%s"},"secrets":{"key":"key-of-%s"}}}' "$app" "$extra" "$app" "$app";;
 release)
  printf '%s' "$request" | grep -q "\"index\":\"$app\"" && printf '%s' "$request" | grep -q "\"key\":\"key-of-$app\"" || { printf '{"ok":false,"messages":["record or secret missing"]}'; exit 0; }
  echo "$app" >> released; printf '{"ok":true}';;
esac
`

func TestAnApplicationGetsAServiceFromAPluginAndGivesItBack(t *testing.T) {
	m, _ := newHostDataFixture(t)
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	if err := m.setPluginKeys([]string{hex.EncodeToString(public)}); err != nil {
		t.Fatal(err)
	}
	install := func(version string, hooks []string) {
		t.Helper()
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: plugin.Program, Mode: 0o755, Size: int64(len(servicePlugin)), Typeflag: tar.TypeReg})
		tw.Write([]byte(servicePlugin))
		tw.Close()
		gz.Close()
		sum := sha256.Sum256(buf.Bytes())
		raw, _ := json.Marshal(plugin.Manifest{Contract: plugin.Contract, Name: "meili", Version: version, Provides: "search", Hooks: hooks, Environment: []string{"SEARCH_URL"},
			Archives: []plugin.Archive{{Arch: runtime.GOARCH, SHA256: hex.EncodeToString(sum[:]), Size: int64(buf.Len())}}})
		signature, _ := json.Marshal(plugin.Sign(private, raw))
		if _, err := m.stagePluginManifest(string(raw), signature); err != nil {
			t.Fatal(err)
		}
		if _, err := m.installPlugin(context.Background(), "meili", bytes.NewReader(buf.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	account := NativeAppAccount{User: "app-shop", UID: 1001, GID: 1001, Root: "/srv/apps/shop"}
	request := NativeDataRequest{Plugins: []NativePluginService{{Service: "search", Options: json.RawMessage(`{"languages":["en"]}`)}}}

	// Nothing on this server provides the service yet.
	if _, err := provisionNativeData(ctx, m, "shop", account, request); err == nil || !strings.Contains(err.Error(), `no plugin that provides "search"`) {
		t.Fatalf("missing plugin: %v", err)
	}
	if checks := m.pluginServiceChecks(ctx, "shop", request.Plugins, nil); len(checks) != 1 || checks[0].OK || !strings.Contains(checks[0].Detail, "install it on the server first") {
		t.Fatalf("rehearsal without the plugin: %+v", checks)
	}
	install("1.0", []string{"describe", "check", "provision", "release"})
	state := m.pluginState("meili")
	if checks := m.pluginServiceChecks(ctx, "shop", request.Plugins, nil); len(checks) != 1 || !checks[0].OK || checks[0].ID != "plugin-search-disk" {
		t.Fatalf("rehearsal: %+v", checks)
	}
	os.WriteFile(filepath.Join(state, "full"), nil, 0o600)
	if checks := m.pluginServiceChecks(ctx, "shop", request.Plugins, nil); len(checks) != 1 || checks[0].OK || checks[0].Detail != "the index does not fit" {
		t.Fatalf("failing rehearsal: %+v", checks)
	}
	os.Remove(filepath.Join(state, "full"))

	// The plugin's refusal is the application's error, and nothing is recorded.
	os.WriteFile(filepath.Join(state, "refuse"), nil, 0o600)
	if _, err := provisionNativeData(ctx, m, "shop", account, request); err == nil || !strings.Contains(err.Error(), "the search server is not running") {
		t.Fatalf("refused provision: %v", err)
	}
	os.Remove(filepath.Join(state, "refuse"))
	// A variable the signed manifest does not declare never reaches the application.
	os.WriteFile(filepath.Join(state, "rogue"), nil, 0o600)
	if _, err := provisionNativeData(ctx, m, "shop", account, request); err == nil || !strings.Contains(err.Error(), "set PATH, which its manifest does not declare") {
		t.Fatalf("undeclared variable: %v", err)
	}
	os.Remove(filepath.Join(state, "rogue"))
	if _, recorded := m.hostDataRecord("shop"); recorded {
		t.Fatal("a failed provision left a record")
	}

	grant, err := provisionNativeData(ctx, m, "shop", account, request)
	if err != nil || grant.Env["SEARCH_URL"] != "http://127.0.0.1:7700/shop" || len(grant.Env) != 1 {
		t.Fatalf("provision: %v %v", err, grant.Env)
	}
	record, _ := m.hostDataRecord("shop")
	entry := record.Plugins["search"]
	if entry.Plugin != "meili" || entry.Version != "1.0" || string(entry.Record) != `{"index":"shop"}` || len(entry.Env) != 1 {
		t.Fatalf("record: %+v", entry)
	}
	// The secret is sealed in the agent's state, not kept in the clear.
	if entry.Secrets == "" || strings.Contains(entry.Secrets, "key-of-shop") {
		t.Fatalf("secret not sealed: %q", entry.Secrets)
	}
	if secrets, err := m.pluginSecrets(entry.Secrets); err != nil || secrets["key"] != "key-of-shop" {
		t.Fatalf("unsealed secrets: %v %v", secrets, err)
	}
	// Removing the application gives the service back, with its record and secret.
	if err := releaseNativeData(ctx, m, "shop"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(state, "released")); string(raw) != "shop\n" {
		t.Fatalf("released: %q", raw)
	}
	if _, recorded := m.hostDataRecord("shop"); recorded {
		t.Fatal("the record stayed after the release")
	}
	// Requests are bounded before anything runs.
	if err := validateNativePluginServices([]NativePluginService{{Service: "search"}, {Service: "search"}}); err == nil {
		t.Fatal("a service requested twice was accepted")
	}
	if err := validateNativePluginServices([]NativePluginService{{Service: "Bad Name"}}); err == nil {
		t.Fatal("an invalid service name was accepted")
	}
}

// The example plugin, built from source with the plugin kit, goes through the
// agent exactly as a real one would: signed, installed, checked, provisioned
// for an application and released. This is the proof that the kit and the
// agent speak the same contract.
func TestTheExamplePluginBuiltWithTheKitWorksThroughTheAgent(t *testing.T) {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the Go toolchain is not on PATH")
	}
	program := filepath.Join(t.TempDir(), plugin.Program)
	build := exec.Command(goBinary, "build", "-o", program, "github.com/agim/lidza-deploy/examples/plugin-example")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the example plugin: %v\n%s", err, out)
	}
	binary, err := os.ReadFile(program)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: plugin.Program, Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg})
	tw.Write(binary)
	tw.Close()
	gz.Close()
	m, _ := newHostDataFixture(t)
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	if err := m.setPluginKeys([]string{hex.EncodeToString(public)}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	raw, _ := json.Marshal(plugin.Manifest{Contract: plugin.Contract, Name: "notes", Version: "1.0", Provides: "notes", Summary: "Keeps a notes directory per application.",
		Hooks: []string{"describe", "check", "status", "provision", "release", "backup", "restore"}, Environment: []string{"NOTES_DIR"},
		Archives: []plugin.Archive{{Arch: runtime.GOARCH, SHA256: hex.EncodeToString(sum[:]), Size: int64(buf.Len())}}})
	signature, _ := json.Marshal(plugin.Sign(private, raw))
	ctx := context.Background()
	if _, err := m.stagePluginManifest(string(raw), signature); err != nil {
		t.Fatal(err)
	}
	if _, err := m.installPlugin(ctx, "notes", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("install: %v", err)
	}
	services := []NativePluginService{{Service: "notes"}}
	if checks := m.pluginServiceChecks(ctx, "shop", services, nil); len(checks) != 1 || !checks[0].OK || checks[0].Title != "Notes disk" {
		t.Fatalf("check: %+v", checks)
	}
	grant, err := provisionNativeData(ctx, m, "shop", NativeAppAccount{User: "app-shop", UID: 1001, GID: 1001, Root: "/srv/apps/shop"}, NativeDataRequest{Plugins: services})
	dir := grant.Env["NOTES_DIR"]
	if err != nil || dir != filepath.Join(m.pluginState("notes"), "notes", "shop") {
		t.Fatalf("provision: %v %v", err, grant.Env)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("the provisioned directory: %v", err)
	}
	result, err := m.runPluginHook(ctx, "notes", "status", nil)
	if err != nil || !strings.Contains(string(result.Answer.Output), `"applications":1`) {
		t.Fatalf("status: %v %s", err, result.Answer.Output)
	}
	// Its notes are backed up and restored through the agent's backup pipeline.
	if len(grant.Databases) != 1 {
		t.Fatalf("no protected resource: %+v", grant)
	}
	note := filepath.Join(dir, "week", "monday.txt")
	os.MkdirAll(filepath.Dir(note), 0o750)
	os.WriteFile(note, []byte("ship it"), 0o640)
	m.mu.Lock()
	d := m.data.Databases[grant.Databases[0]]
	m.mu.Unlock()
	backup, err := m.dumpDatabase(ctx, d, nil)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	os.WriteFile(note, []byte("overwritten"), 0o640)
	os.WriteFile(filepath.Join(dir, "later.txt"), []byte("new"), 0o640)
	if err := m.restorePluginBackup(ctx, "shop", grant.Databases[0], backup); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if raw, _ := os.ReadFile(note); string(raw) != "ship it" {
		t.Fatalf("restored note: %q", raw)
	}
	if _, err := os.Stat(filepath.Join(dir, "later.txt")); !os.IsNotExist(err) {
		t.Fatal("a file written after the backup survived the restore")
	}
	if err := releaseNativeData(ctx, m, "shop"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the released directory stayed")
	}
}

// hostPlugin manages a pretend server: it is "down" until repaired, and
// records its maintenance runs.
const hostPlugin = `#!/bin/sh
cat >/dev/null
case "$1" in
 describe) printf '{"ok":true}';;
 status) if [ -f up ]; then printf '{"ok":true,"output":{"version":"7.1","running":true}}'; else printf '{"ok":false,"messages":["the queue server is stopped"]}'; fi;;
 check) if [ -f up ]; then printf '{"ok":true,"checks":[{"id":"server","title":"Queue server","ok":true,"detail":"running"}]}'; else printf '{"ok":false,"checks":[{"id":"server","title":"Queue server","ok":false,"repairable":true,"detail":"its service failed"}]}'; fi;;
 repair) [ ! -f broken ] || { printf '{"ok":false,"messages":["the data directory is missing"]}'; exit 0; }; : > up; printf '{"ok":true,"messages":["started the queue server"]}';;
 maintain) echo run >> maintained; printf '{"ok":true,"messages":["dropped 3 old segments"]}';;
esac
`

func TestPluginsReportStatusCheckAndRepairTheServerAndRunMaintenance(t *testing.T) {
	m, _ := newHostDataFixture(t)
	m.ctx = context.Background()
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	if err := m.setPluginKeys([]string{hex.EncodeToString(public)}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: plugin.Program, Mode: 0o755, Size: int64(len(hostPlugin)), Typeflag: tar.TypeReg})
	tw.Write([]byte(hostPlugin))
	tw.Close()
	gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	raw, _ := json.Marshal(plugin.Manifest{Contract: plugin.Contract, Name: "queue", Version: "1.0", Provides: "queue", Hooks: []string{"describe", "status", "check", "repair", "maintain"},
		Archives: []plugin.Archive{{Arch: runtime.GOARCH, SHA256: hex.EncodeToString(sum[:]), Size: int64(buf.Len())}}})
	signature, _ := json.Marshal(plugin.Sign(private, raw))
	ctx := context.Background()
	if _, err := m.stagePluginManifest(string(raw), signature); err != nil {
		t.Fatal(err)
	}
	if _, err := m.installPlugin(ctx, "queue", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	state := m.pluginState("queue")
	find := func(checks []HostReadinessCheck, id string) *HostReadinessCheck {
		for _, c := range checks {
			if c.ID == id {
				return &c
			}
		}
		return nil
	}
	// The plugin's own check is part of the server's readiness.
	report := m.hostReadiness(ctx)
	if c := find(report.Checks, "plugin-queue-server"); c == nil || c.OK || !c.Repairable || c.Title != "Queue server" || c.Detail != "its service failed" || report.OK {
		t.Fatalf("readiness: %+v", report.Checks)
	}
	// A repair that fails says why; nothing is claimed.
	os.WriteFile(filepath.Join(state, "broken"), nil, 0o600)
	if after := m.repairHost(ctx); !slices.ContainsFunc(after.Failed, func(s string) bool { return strings.Contains(s, "the data directory is missing") }) {
		t.Fatalf("failed repair: %+v", after)
	}
	os.Remove(filepath.Join(state, "broken"))
	after := m.repairHost(ctx)
	if !slices.ContainsFunc(after.Repaired, func(s string) bool { return s == "queue: started the queue server" }) {
		t.Fatalf("repair: %+v", after)
	}
	if c := find(after.Checks, "plugin-queue-server"); c == nil || !c.OK {
		t.Fatalf("readiness after repair: %+v", after.Checks)
	}
	// Status and maintenance run in the background tick; health reports the last status.
	now := time.Now()
	m.pluginHousekeeping(now)
	health := m.pluginHealth()
	if len(health) != 1 || !health[0].OK || !strings.Contains(string(health[0].Status), `"version":"7.1"`) || health[0].Provides != "queue" {
		t.Fatalf("health: %+v", health)
	}
	// Maintenance is hourly, however often status is refreshed.
	m.pluginHousekeeping(now.Add(10 * time.Minute))
	m.pluginHousekeeping(now.Add(61 * time.Minute))
	if raw, _ := os.ReadFile(filepath.Join(state, "maintained")); string(raw) != "run\nrun\n" {
		t.Fatalf("maintenance runs: %q", raw)
	}
	// A removed plugin leaves no status behind.
	if err := m.removePlugin("queue"); err != nil {
		t.Fatal(err)
	}
	m.pluginHousekeeping(now.Add(2 * time.Hour))
	if len(m.pluginHealth()) != 0 {
		t.Fatalf("health of a removed plugin: %+v", m.pluginHealth())
	}
}

// With the ClickHouse plugin installed, the agent asks it instead of using
// its own ClickHouse rules: for an application's rehearsal and for the
// server's readiness.
func TestTheClickHousePluginReplacesTheBuiltInChecksWhereItIsInstalled(t *testing.T) {
	m, f := newHostDataFixture(t)
	m.cfg.Native = &NativeHostConfig{Root: t.TempDir(), User: "deploy", AppsRoot: filepath.Join(f.root, "srv/apps")}
	savedEUID, savedDisk, savedForbidden, savedListen := hostNativeEUID, preflightDisk, nativeForbiddenAppsRoots, nativePortListenable
	t.Cleanup(func() {
		hostNativeEUID, preflightDisk, nativeForbiddenAppsRoots, nativePortListenable = savedEUID, savedDisk, savedForbidden, savedListen
	})
	hostNativeEUID = func() int { return 0 }
	preflightDisk = func(string) (DiskHealth, error) {
		return DiskHealth{Total: 100 << 30, Available: 90 << 30, UsedPercent: 10}, nil
	}
	nativeForbiddenAppsRoots = nil
	nativePortListenable = func(int) bool { return true }
	// A ClickHouse server is installed here and its service failed.
	os.MkdirAll(filepath.Join(f.root, "usr/bin"), 0o755)
	os.WriteFile(filepath.Join(f.root, "usr/bin/clickhouse-server"), nil, 0o755)
	os.WriteFile(filepath.Join(f.root, "usr/bin/apt-get"), nil, 0o755)
	previous := nativeHostDataHost
	t.Cleanup(func() { nativeHostDataHost = previous })
	nativeHostDataHost = func(*Manager) nativeHostData {
		return nativeHostData{root: f.root, run: func(ctx context.Context, stdin, name string, args ...string) (string, error) {
			if name == "/usr/bin/systemctl" && args[0] == "show" {
				return "failed\n", nil
			}
			return f.run(ctx, stdin, name, args...)
		}}
	}
	request := NativePreflightRequest{ClickHouseBytes: 5 << 30, Request: NativeCreateRequest{ID: "shop", Runtime: "go", Repository: "acme/shop", Branch: "main", Domain: "shop.example.com", HealthPath: "/readyz",
		Processes: []NativeProcessSpec{{Name: "web", Command: "./shop", Web: true}}, Data: NativeDataRequest{ClickHouse: true, ClickHouseVersion: "26.8"}}}
	ctx := context.Background()
	ids := func(checks []HostReadinessCheck) string {
		out := []string{}
		for _, c := range checks {
			out = append(out, c.ID)
		}
		return strings.Join(out, " ")
	}
	// Without the plugin the built-in checks answer.
	if got := ids(m.nativePreflight(ctx, request).Checks); !strings.Contains(got, " clickhouse ") && !strings.HasSuffix(got, " clickhouse") || strings.Contains(got, "plugin-") {
		t.Fatalf("built-in rehearsal: %s", got)
	}
	if got := ids(m.hostReadiness(ctx).Checks); !strings.Contains(got, "clickhouse") || strings.Contains(got, "plugin-") {
		t.Fatalf("built-in readiness: %s", got)
	}
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	if err := m.setPluginKeys([]string{hex.EncodeToString(public)}); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
request=$(cat)
case "$1" in
 describe) printf '{"ok":true}';;
 check) series=$(printf '%s' "$request" | sed -n 's/.*"series":"\([^"]*\)".*/\1/p'); printf '{"ok":true,"checks":[{"id":"processor","title":"Processor","ok":true,"caution":true,"detail":"asked for %s"},{"id":"server","title":"ClickHouse server","ok":false,"repairable":true,"detail":"its service failed"}]}' "$series";;
 repair) printf '{"ok":true,"messages":["started the ClickHouse server"]}';;
esac
`
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: plugin.Program, Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg})
	tw.Write([]byte(script))
	tw.Close()
	gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	raw, _ := json.Marshal(plugin.Manifest{Contract: plugin.Contract, Name: "clickhouse", Version: "0.1", Provides: "clickhouse", Hooks: []string{"describe", "check", "repair"},
		Archives: []plugin.Archive{{Arch: runtime.GOARCH, SHA256: hex.EncodeToString(sum[:]), Size: int64(buf.Len())}}})
	signature, _ := json.Marshal(plugin.Sign(private, raw))
	if _, err := m.stagePluginManifest(string(raw), signature); err != nil {
		t.Fatal(err)
	}
	if _, err := m.installPlugin(ctx, "clickhouse", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	// The rehearsal asks the plugin, with the series the application comes from.
	report := m.nativePreflight(ctx, request)
	got := ids(report.Checks)
	if !strings.Contains(got, "plugin-clickhouse-processor") || !strings.Contains(got, "plugin-clickhouse-server") || strings.Contains(got+" ", " clickhouse ") || strings.Contains(got, "clickhouse-disk") {
		t.Fatalf("rehearsal with the plugin: %s", got)
	}
	for _, c := range report.Checks {
		if c.ID == "plugin-clickhouse-processor" && (c.Detail != "asked for 26.8" || !c.Caution) {
			t.Fatalf("the plugin was not given the series: %+v", c)
		}
	}
	// Readiness and repair are the plugin's too: the built-in ClickHouse entries are gone.
	readiness := m.hostReadiness(ctx)
	got = ids(readiness.Checks)
	if !strings.Contains(got, "plugin-clickhouse-server") || strings.Contains(" "+got+" ", " clickhouse ") {
		t.Fatalf("readiness with the plugin: %s", got)
	}
	f.mu.Lock()
	f.commands = nil
	f.mu.Unlock()
	repaired := m.repairHost(ctx)
	if !slices.Contains(repaired.Repaired, "clickhouse: started the ClickHouse server") {
		t.Fatalf("repair with the plugin: %+v", repaired)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, command := range f.commands {
		if strings.Contains(command, "systemctl start clickhouse") || strings.Contains(command, "purge") {
			t.Fatalf("the agent repaired ClickHouse itself although the plugin owns it: %s", command)
		}
	}
}

// A plugin that can back its service up gets a protected resource for the
// application: its backups go through the same pipeline as a database's,
// and a restore hands the plugin the files it wrote.
func TestAPluginServiceIsBackedUpAndRestoredThroughItsPlugin(t *testing.T) {
	m, _ := newHostDataFixture(t)
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	if err := m.setPluginKeys([]string{hex.EncodeToString(public)}); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
request=$(cat)
work=$(printf '%s' "$request" | sed -n 's/.*"work_dir":"\([^"]*\)".*/\1/p')
case "$1" in
 describe|release) printf '{"ok":true}';;
 provision) printf '{"ok":true,"output":{"env":{"SEARCH_URL":"http://127.0.0.1:7700/shop"},"record":{"index":"shop"}}}';;
 backup) if [ -e refuse ]; then printf '{"ok":false,"messages":["the index is locked"]}'; exit 0; fi
   mkdir -p "$work/data"; cp index.txt "$work/data/index.txt"; printf '{"ok":true}';;
 restore) cp "$work/data/index.txt" index.txt; printf '{"ok":true}';;
esac
`
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: plugin.Program, Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg})
	tw.Write([]byte(script))
	tw.Close()
	gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	raw, _ := json.Marshal(plugin.Manifest{Contract: plugin.Contract, Name: "meili", Version: "1.0", Provides: "search", Environment: []string{"SEARCH_URL"},
		Hooks:    []string{"describe", "provision", "release", "backup", "restore"},
		Archives: []plugin.Archive{{Arch: runtime.GOARCH, SHA256: hex.EncodeToString(sum[:]), Size: int64(buf.Len())}}})
	signature, _ := json.Marshal(plugin.Sign(private, raw))
	if _, err := m.stagePluginManifest(string(raw), signature); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := m.installPlugin(ctx, "meili", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	state := m.pluginState("meili")
	os.WriteFile(filepath.Join(state, "index.txt"), []byte("first"), 0o600)
	account := NativeAppAccount{User: "app-shop", UID: 1001, GID: 1001, Root: "/srv/apps/shop"}
	grant, err := provisionNativeData(ctx, m, "shop", account, NativeDataRequest{Plugins: []NativePluginService{{Service: "search"}}})
	if err != nil || len(grant.Databases) != 1 {
		t.Fatalf("provision: %v %+v", err, grant)
	}
	resource := grant.Databases[0]
	record, _ := m.hostDataRecord("shop")
	if record.Plugins["search"].Resource != resource || !strings.HasPrefix(resource, "shop-pl") {
		t.Fatalf("resource: %q %+v", resource, record.Plugins["search"])
	}
	m.mu.Lock()
	policy, d := m.data.NativeDatabases[resource].Policy, m.data.Databases[resource]
	inUse := m.nativeDatabaseInUseLocked(resource)
	m.mu.Unlock()
	if policy.PluginService != "search" || policy.AppID != "shop" || !d.Ready || d.Backup.Hours != 24 || !inUse || strings.Contains(d.URL, "@") {
		t.Fatalf("protected resource: %+v %+v in use %v", policy, d, inUse)
	}
	// The service appears among the server's databases under the name of what the plugin provides.
	found := false
	for _, view := range m.databaseViews() {
		found = found || view.AppID == resource && view.Engine == "search"
	}
	if !found {
		t.Fatal("the service is not listed with its engine")
	}
	// A refusal of the plugin fails the backup and records nothing.
	os.WriteFile(filepath.Join(state, "refuse"), nil, 0o600)
	if _, err := m.dumpDatabase(ctx, d, nil); err == nil || !strings.Contains(err.Error(), "the index is locked") {
		t.Fatalf("refused backup: %v", err)
	}
	os.Remove(filepath.Join(state, "refuse"))
	backup, err := m.dumpDatabase(ctx, d, nil)
	if err != nil || backup.Size == 0 || len(backup.SHA256) != 64 {
		t.Fatalf("backup: %v %+v", err, backup)
	}
	if left, _ := filepath.Glob(filepath.Join(m.backupDir(resource), ".plugin-*")); len(left) != 0 {
		t.Fatalf("the plugin's work directory stayed: %v", left)
	}
	// The data changes; the restore brings the backup's files back through the plugin.
	os.WriteFile(filepath.Join(state, "index.txt"), []byte("second"), 0o600)
	if err := m.restorePluginBackup(ctx, "shop", resource, backup); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(state, "index.txt")); string(raw) != "first" {
		t.Fatalf("restored: %q", raw)
	}
	// A damaged backup is refused before the plugin sees it.
	damaged := backup
	damaged.SHA256 = strings.Repeat("0", 64)
	if err := m.restorePluginBackup(ctx, "shop", resource, damaged); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("damaged backup: %v", err)
	}
	// It cannot be attached to another application or restored as PostgreSQL.
	m.mu.Lock()
	m.data.Apps = map[string]App{"other": {}}
	bindErr := m.bindDatabaseLocked("other", "DATABASE_URL", resource)
	m.mu.Unlock()
	if bindErr == nil {
		t.Fatal("a plugin's service was attached to another application")
	}
	// Removing the application releases the service; the resource is no longer bound.
	if err := releaseNativeData(ctx, m, "shop"); err != nil {
		t.Fatalf("release: %v", err)
	}
	m.mu.Lock()
	_, bound := m.data.NativeDatabases[resource]
	m.mu.Unlock()
	if _, _, ok := m.pluginResourceApp(resource); ok || bound {
		t.Fatal("the resource stayed bound after the release")
	}
	// Archives with anything but files and directories are refused.
	var bad bytes.Buffer
	btw := tar.NewWriter(&bad)
	btw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg})
	btw.Write([]byte("x"))
	btw.Close()
	if err := untarPluginDirectory(&bad, t.TempDir()); err == nil {
		t.Fatal("a path outside the directory was unpacked")
	}
}
