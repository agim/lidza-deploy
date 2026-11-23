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
	"strings"
	"testing"

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
		Hooks: []string{"describe", "check", "status", "provision", "release"}, Environment: []string{"NOTES_DIR"},
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
	if err := releaseNativeData(ctx, m, "shop"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the released directory stayed")
	}
}
