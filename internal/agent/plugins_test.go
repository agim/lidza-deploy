package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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

	// No key is trusted yet: nothing is accepted.
	if w := do("POST", "/v1/plugins/manifest", "application/json", stage); w.Code != 409 || !strings.Contains(w.Body.String(), "trusts no plugin signing key") {
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
