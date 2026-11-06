package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Tests never reach the developer machine's own web server: the default
// ingress probes see no admin API and no web ports unless a test stubs them.
func init() {
	// Managed error pages go to a private scratch directory, readable as
	// on a host (error_pages.go).
	pages, err := os.MkdirTemp("", "lidza-pages-")
	if err != nil {
		panic(err)
	}
	if err := os.Chmod(pages, 0o755); err != nil {
		panic(err)
	}
	errorPagesDir = filepath.Join(pages, "pages")
	caddyProcessUser = func() string { return "caddy" }
	hostIngressListeners = func() []int { return nil }
	hostIngressAdminGet = func(ctx context.Context, admin string) ([]byte, error) {
		if admin == hostIngressDefaultAdmin {
			return nil, errors.New("connection refused")
		}
		return caddyAdminConfig(ctx, admin)
	}
}

// fakeCaddyAdmin is a small model of Caddy's admin API over a JSON
// configuration tree: GET/POST/PUT/PATCH/DELETE on /config/... paths and on
// /id/<@id> nodes, with Caddy's semantics (POST appends to arrays and
// creates missing keys, PUT inserts into arrays, PATCH replaces an existing
// value, DELETE removes).
type fakeCaddyAdmin struct {
	t      *testing.T
	mu     sync.Mutex
	config map[string]any
	calls  []string
	srv    *httptest.Server
}

func newFakeCaddyAdmin(t *testing.T, config map[string]any) *fakeCaddyAdmin {
	t.Helper()
	f := &fakeCaddyAdmin{t: t, config: config}
	if f.config == nil {
		f.config = map[string]any{}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCaddyAdmin) URL() string { return f.srv.URL }

// snapshot returns a JSON-normalized copy of the configuration.
func (f *fakeCaddyAdmin) snapshot() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := json.Marshal(f.config)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// at returns the value at a /config path of the current configuration.
func (f *fakeCaddyAdmin) at(path string) any {
	node := any(f.snapshot())
	for _, segment := range strings.Split(strings.Trim(strings.TrimPrefix(path, "/config"), "/"), "/") {
		switch typed := node.(type) {
		case map[string]any:
			node = typed[segment]
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(typed) {
				return nil
			}
			node = typed[index]
		default:
			return nil
		}
	}
	return node
}

// strings returns the string array at a /config path.
func (f *fakeCaddyAdmin) strings(path string) []string {
	list, _ := f.at(path).([]any)
	out := []string{}
	for _, v := range list {
		out = append(out, v.(string))
	}
	return out
}

func (f *fakeCaddyAdmin) called(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, call := range f.calls {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeCaddyAdmin) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func findCaddyID(node any, id string, path []string) ([]string, bool) {
	switch typed := node.(type) {
	case map[string]any:
		if typed["@id"] == id {
			return path, true
		}
		for key, child := range typed {
			if found, ok := findCaddyID(child, id, append(append([]string{}, path...), key)); ok {
				return found, true
			}
		}
	case []any:
		for i, child := range typed {
			if found, ok := findCaddyID(child, id, append(append([]string{}, path...), strconv.Itoa(i))); ok {
				return found, true
			}
		}
	}
	return nil, false
}

func (f *fakeCaddyAdmin) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	var body any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
	}
	if r.URL.Path == "/load" {
		loaded, ok := body.(map[string]any)
		if r.Method != http.MethodPost || !ok {
			http.Error(w, "load needs a configuration object", 400)
			return
		}
		f.config = loaded
		return
	}
	var segments []string
	switch {
	case strings.HasPrefix(r.URL.Path, "/id/"):
		found, ok := findCaddyID(f.config, strings.TrimPrefix(r.URL.Path, "/id/"), nil)
		if !ok {
			http.Error(w, "unknown object id", 404)
			return
		}
		segments = found
	case strings.HasPrefix(r.URL.Path, "/config"):
		trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/config"), "/")
		if trimmed != "" {
			segments = strings.Split(trimmed, "/")
		}
	default:
		http.NotFound(w, r)
		return
	}
	if len(segments) == 0 {
		if r.Method != http.MethodGet {
			http.Error(w, "unsupported", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(f.config)
		return
	}
	// Walk to the parent, creating maps for writes.
	parent := any(f.config)
	for _, segment := range segments[:len(segments)-1] {
		switch typed := parent.(type) {
		case map[string]any:
			child, ok := typed[segment]
			if !ok {
				if r.Method == http.MethodGet || r.Method == http.MethodDelete || r.Method == http.MethodPatch {
					http.Error(w, "unknown path", 404)
					return
				}
				child = map[string]any{}
				typed[segment] = child
			}
			parent = child
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(typed) {
				http.Error(w, "unknown index", 404)
				return
			}
			parent = typed[index]
		default:
			http.Error(w, "unknown path", 404)
			return
		}
	}
	last := segments[len(segments)-1]
	switch typed := parent.(type) {
	case map[string]any:
		current, exists := typed[last]
		switch r.Method {
		case http.MethodGet:
			if !exists {
				http.Error(w, "unknown path", 404)
				return
			}
			_ = json.NewEncoder(w).Encode(current)
		case http.MethodPost:
			if list, isList := current.([]any); exists && isList {
				typed[last] = append(list, body)
			} else {
				typed[last] = body
			}
		case http.MethodPut:
			typed[last] = body
		case http.MethodPatch:
			if !exists {
				http.Error(w, "unknown path", 404)
				return
			}
			typed[last] = body
		case http.MethodDelete:
			if !exists {
				http.Error(w, "unknown path", 404)
				return
			}
			delete(typed, last)
		}
	case []any:
		index, err := strconv.Atoi(last)
		if err != nil || index < 0 || index > len(typed) || (r.Method != http.MethodPut && index == len(typed)) {
			http.Error(w, "unknown index", 404)
			return
		}
		// Arrays live inside their parent map; rewrite the parent's slot.
		grand := any(f.config)
		for _, segment := range segments[:len(segments)-2] {
			switch g := grand.(type) {
			case map[string]any:
				grand = g[segment]
			case []any:
				i, _ := strconv.Atoi(segment)
				grand = g[i]
			}
		}
		set := func(list []any) {
			key := segments[len(segments)-2]
			switch g := grand.(type) {
			case map[string]any:
				g[key] = list
			case []any:
				i, _ := strconv.Atoi(key)
				g[i] = list
			}
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(typed[index])
		case http.MethodPut:
			list := append([]any{}, typed[:index]...)
			list = append(list, body)
			set(append(list, typed[index:]...))
		case http.MethodPost, http.MethodPatch:
			typed[index] = body
		case http.MethodDelete:
			set(append(append([]any{}, typed[:index]...), typed[index+1:]...))
		}
	default:
		http.Error(w, "unknown path", 404)
	}
}

// Caddy configurations used by the tests.

// operatorCaddyConfig is an operator-managed Caddy: host-matched routes,
// an ACME policy restricted to its subjects.
func operatorCaddyConfig() map[string]any {
	return map[string]any{"apps": map[string]any{
		"http": map[string]any{"servers": map[string]any{
			"srv0": map[string]any{"listen": []any{":80"}, "routes": []any{}},
			"https": map[string]any{"listen": []any{":443"}, "routes": []any{
				map[string]any{"@id": "ops", "match": []any{map[string]any{"host": []any{"ops.example.com"}}}, "terminal": true,
					"handle": []any{map[string]any{"handler": "reverse_proxy", "upstreams": []any{map[string]any{"dial": "127.0.0.1:3000"}}}}},
			}},
		}},
		"tls": map[string]any{"automation": map[string]any{"policies": []any{
			map[string]any{"subjects": []any{"ops.example.com"}, "issuers": []any{map[string]any{"module": "acme", "email": "ops@example.com"}}},
		}}},
	}}
}

// installerCaddyConfig is the layout the agent installer writes: every
// hostname to the agent proxy, certificates on demand.
func installerCaddyConfig(upstream string) map[string]any {
	return map[string]any{"apps": map[string]any{
		"http": map[string]any{"servers": map[string]any{
			"srv0": map[string]any{"listen": []any{":443"}, "routes": []any{
				map[string]any{"handle": []any{map[string]any{"handler": "subroute", "routes": []any{
					map[string]any{"handle": []any{map[string]any{"handler": "reverse_proxy", "upstreams": []any{map[string]any{"dial": upstream}}}}},
				}}}},
			}},
			"srv1": map[string]any{"listen": []any{":80"}, "routes": []any{}},
		}},
		"tls": map[string]any{"automation": map[string]any{
			"on_demand": map[string]any{"permission": map[string]any{"module": "http", "endpoint": "http://127.0.0.1:9090/tls/allow"}},
			"policies":  []any{map[string]any{"on_demand": true}},
		}},
	}}
}
