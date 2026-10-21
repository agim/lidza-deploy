package agent

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func fixtureStackV2() StackManifest {
	m := fixtureStackManifest()
	m.Version = 2
	m.GeneratedSecrets = append(m.GeneratedSecrets, "DEPLOY_RUNTIME_TOKEN")
	m.RuntimeSelection = &StackRuntimeSelection{Selectors: map[string][]string{"AI_RUNTIME": {"remote", "local"}}, Initial: map[string]string{"AI_RUNTIME": "remote"}, Path: "/.well-known/deploy-runtime", TokenKey: "DEPLOY_RUNTIME_TOKEN"}
	m.Services = append(m.Services, StackService{Name: "matrix", Role: "auxiliary", Policy: "init", Build: &StackBuild{Dockerfile: "Dockerfile"}, Environment: map[string]string{"MATRIX_DATABASE_URL": "${DATABASE_URL}"}, DatabaseBindings: []string{"DATABASE_URL"}, HealthCommand: []string{"/fixture", "--health"}}, StackService{Name: "local-ai", Role: "auxiliary", Policy: "hardened", Image: "example/model@sha256:" + strings.Repeat("f", 64), EnabledWhen: &StackConfigurationCondition{Name: "AI_RUNTIME", Equals: "local"}, HealthCommand: []string{"/model-health"}})
	resources := StackProfileResources{Services: map[string]StackBudget{}, Database: StackBudget{256, "0.25"}}
	for _, s := range m.Services {
		resources.Services[s.Name] = StackBudget{128, "0.1"}
	}
	m.ProfileResources = map[string]StackProfileResources{"minimum": resources}
	return m
}
func v2App() App {
	a := testApp("one")
	a.RuntimeProfile = "minimum"
	a.Env["LIDZA_MASTER_KEY"] = strings.Repeat("1", 64)
	a.Env["DATABASE_URL"] = "postgres://app:password@db/app?sslmode=disable"
	a.Env["DEPLOY_RUNTIME_TOKEN"] = "stable-token"
	a.Bindings = map[string]string{"DATABASE_URL": "one"}
	a.BindingNetworks = map[string]string{"DATABASE_URL": "lidza-db-one"}
	return a
}
func cloneManifest(m StackManifest) StackManifest {
	raw, _ := json.Marshal(m)
	var out StackManifest
	_ = json.Unmarshal(raw, &out)
	return out
}
func TestStackV2StrictSchema(t *testing.T) {
	good := fixtureStackV2()
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*StackManifest){
		"v1-fields":              func(m *StackManifest) { m.Version = 1 },
		"missing-profile":        func(m *StackManifest) { m.ProfileResources = nil },
		"missing-service-budget": func(m *StackManifest) { delete(m.ProfileResources["minimum"].Services, "matrix") },
		"bad-cpu":                func(m *StackManifest) { m.ProfileResources["minimum"].Services["matrix"] = StackBudget{128, "NaN"} },
		"wrong-role-policy":      func(m *StackManifest) { m.Services[3].Policy = "office" },
		"conditional-app": func(m *StackManifest) {
			m.Services[0].EnabledWhen = &StackConfigurationCondition{Name: "AI_RUNTIME", Equals: "local"}
		},
		"conditional-build": func(m *StackManifest) {
			m.Services[4].Build = &StackBuild{Dockerfile: "Dockerfile"}
			m.Services[4].Image = ""
		},
		"secret-selector": func(m *StackManifest) {
			m.RuntimeSelection.Selectors = map[string][]string{"API_TOKEN": {"local"}}
			m.RuntimeSelection.Initial = map[string]string{"API_TOKEN": "local"}
		},
		"invalid-initial":   func(m *StackManifest) { m.RuntimeSelection.Initial["AI_RUNTIME"] = "maybe" },
		"missing-token":     func(m *StackManifest) { m.RuntimeSelection.TokenKey = "UNGENERATED_TOKEN" },
		"public-path":       func(m *StackManifest) { m.RuntimeSelection.Path = "https://evil.example/control" },
		"shared-net-db":     func(m *StackManifest) { m.Services[1].DatabaseBindings = []string{"DATABASE_URL"} },
		"unknown-condition": func(m *StackManifest) { m.Services[4].EnabledWhen.Equals = "unknown" },
		"duplicate-binding": func(m *StackManifest) { m.Services[3].DatabaseBindings = []string{"DATABASE_URL", "DATABASE_URL"} },
	} {
		t.Run(name, func(t *testing.T) {
			m := cloneManifest(good)
			mutate(&m)
			if err := m.validate(); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}
func TestStackV2SelectedBudgetsAndIsolation(t *testing.T) {
	m := fixtureStackV2()
	a := v2App()
	remote := &StackSelectionSnapshot{Version: 1, Revision: "remote", Values: map[string]string{"AI_RUNTIME": "remote"}}
	local := cloneStackSelection(remote)
	local.Values["AI_RUNTIME"] = "local"
	if len(m.selectedServices(remote)) != 4 || len(m.selectedServices(local)) != 5 {
		t.Fatal("wrong conditional topology")
	}
	if err := m.validatePlan(a, local); err != nil {
		t.Fatal(err)
	}
	m.ProfileResources["minimum"].Services["local-ai"] = StackBudget{4000, "0.1"}
	if err := m.validatePlan(a, remote); err != nil {
		t.Fatal("disabled service charged", err)
	}
	if err := m.validatePlan(a, local); err == nil {
		t.Fatal("over-budget local plan accepted")
	}
	a.DatabaseBudgets = []StackBudget{{1024, "1"}}
	if err := m.validatePlan(a, remote); err == nil || !strings.Contains(err.Error(), "sizing is preserved") {
		t.Fatal("existing database silently retuned", err)
	}
	a.DatabaseBudgets = nil
	delete(a.Bindings, "DATABASE_URL")
	if err := m.validatePlan(a, remote); err == nil {
		t.Fatal("missing DB attachment accepted")
	}
	args := strings.Join(stackPolicyArgs(m.Services[3]), " ")
	if !strings.Contains(args, "--security-opt no-new-privileges") || strings.Contains(args, "NET_BIND") || strings.Contains(args, "SYS_ADMIN") {
		t.Fatal("aux init policy broadened", args)
	}
}
func selectionServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, App, StackManifest) {
	t.Helper()
	m := fixtureStackV2()
	a := v2App()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	a.Current = &Release{ID: "current", Port: port, Stack: &StackRelease{Manifest: m, Selection: &StackSelectionSnapshot{Version: 1, Revision: "active-remote", Values: map[string]string{"AI_RUNTIME": "remote"}}}}
	return server, a, m
}
func TestStackV2SavedDesiredSelectionAndRevisionFences(t *testing.T) {
	var revision atomic.Value
	revision.Store("saved-local")
	_, a, m := selectionServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer stable-token" {
			t.Error("missing private endpoint authentication")
		}
		_ = json.NewEncoder(w).Encode(StackSelectionSnapshot{Version: 1, Revision: revision.Load().(string), Values: map[string]string{"AI_RUNTIME": "local"}})
	})
	planned, err := (&Docker{}).prepareStackPlan(context.Background(), a, m)
	if err != nil {
		t.Fatal(err)
	}
	if planned.StackSelection.Values["AI_RUNTIME"] != "local" {
		t.Fatal("saved desired configuration was ignored")
	}
	if a.Env["AI_RUNTIME"] != "" {
		t.Fatal("operator environment mutated")
	}
	if err := verifyStackSelection(context.Background(), planned, m); err != nil {
		t.Fatal(err)
	}
	revision.Store("published-remote")
	if err := verifyStackSelection(context.Background(), planned, m); err == nil {
		t.Fatal("stale revision accepted")
	}
	a.Env["AI_RUNTIME"] = "remote"
	if _, err = resolveStackSelection(context.Background(), a, m); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatal("operator conflict ignored", err)
	}
}
func TestStackV2SelectionResponseFailClosed(t *testing.T) {
	for name, body := range map[string]string{"extra-fields": `{"version":1,"revision":"a","values":{"AI_RUNTIME":"remote"},"token":"do-not-return"}`, "unknown-enum": `{"version":1,"revision":"a","values":{"AI_RUNTIME":"other"}}`, "missing-key": `{"version":1,"revision":"a","values":{}}`, "extra-key": `{"version":1,"revision":"a","values":{"AI_RUNTIME":"remote","COMMAND":"evil"}}`, "oversize": strings.Repeat("x", 16385), "two-json": `{"version":1,"revision":"a","values":{"AI_RUNTIME":"remote"}} {}`} {
		t.Run(name, func(t *testing.T) {
			_, a, m := selectionServer(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
			if _, err := readStackSelection(context.Background(), a, m.RuntimeSelection); err == nil || strings.Contains(err.Error(), "do-not-return") {
				t.Fatal("invalid/unbounded or unsanitized response", err)
			}
		})
	}
	var calls atomic.Int32
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer external.Close()
	_, a, m := selectionServer(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, external.URL, 302) })
	if _, err := readStackSelection(context.Background(), a, m.RuntimeSelection); err == nil || calls.Load() != 0 {
		t.Fatal("redirect followed", err)
	}
}
func TestStackV2RecordedStartAndRollbackCompatibility(t *testing.T) {
	_, a, m := selectionServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(StackSelectionSnapshot{Version: 1, Revision: "new", Values: map[string]string{"AI_RUNTIME": "local"}})
	})
	a.UseRecordedSelection = true
	got, err := resolveStackSelection(context.Background(), a, m)
	if err != nil || got.Values["AI_RUNTIME"] != "remote" {
		t.Fatal("explicit start did not retain recorded topology", err)
	}
	a.UseRecordedSelection = false
	a.Previous = &Release{Stack: &StackRelease{Manifest: m, Selection: cloneStackSelection(got)}}
	if err = validateRollbackSelection(context.Background(), a); err == nil {
		t.Fatal("incompatible rollback accepted")
	}
	a.Previous.Stack.Selection.Values["AI_RUNTIME"] = "local"
	if err = validateRollbackSelection(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}
func TestStackV2PrivateSelectionNotPubliclyProxied(t *testing.T) {
	var calls atomic.Int32
	_, a, mf := selectionServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.Write([]byte("private")) })
	manager := testManager(t, &fakeRuntime{})
	manager.data.Apps[a.ID] = a
	for _, p := range []string{mf.RuntimeSelection.Path, mf.RuntimeSelection.Path + "/", "/other/../" + strings.TrimPrefix(mf.RuntimeSelection.Path, "/")} {
		r := httptest.NewRequest("GET", "http://"+a.Domain+p, nil)
		w := httptest.NewRecorder()
		Proxy(manager).ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatal("private exchange exposed", p, w.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("private endpoint reached through public proxy")
	}
}
func TestStackV2PreparationPersistsStableSecretsAndPlan(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := v2App()
	a.Bindings = nil
	delete(a.Env, "DATABASE_URL")
	a.Env["MAIL_PROVIDER"] = "smtp"
	a.Env["LLM_PROVIDER"] = "saved-provider"
	mf := fixtureStackV2()
	mf.Services[3].DatabaseBindings = nil
	mf.Services[3].Environment = nil
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	first, err := m.prepareStack(context.Background(), a, &mf)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.prepareStack(context.Background(), a, &mf)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range mf.GeneratedSecrets {
		if first.Env[key] == "" || first.Env[key] != second.Env[key] {
			t.Fatal("generated identity rotated", key)
		}
	}
	if second.Env["MAIL_PROVIDER"] != "smtp" || second.Env["LLM_PROVIDER"] != "saved-provider" {
		t.Fatal("saved provider changed")
	}
	snapshot := &Manager{cfg: m.cfg, key: m.key}
	if err := snapshot.load(); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(snapshot.data.StackSelections[a.ID].Values, mf.RuntimeSelection.Initial) {
		t.Fatal("snapshot not persisted")
	}
	if snapshot.data.Apps[a.ID].Env["AI_RUNTIME"] != "" {
		t.Fatal("managed selector persisted as an operator override")
	}
}
func TestStackV2DisabledImageNeverPulledOrCreated(t *testing.T) {
	actions := filepath.Join(t.TempDir(), "actions")
	stackDockerFixture(t, `echo "$*" >> '`+actions+`'
case "$1" in
 info) echo linux/x86_64;;
 image) case "$*" in *Config.User*) echo 65532;; *) echo '{"os":"linux","architecture":"amd64"}';;esac;;
 network) case "$2" in create) exit 1;;esac;;
esac
`)
	a := v2App()
	m := fixtureStackV2()
	_, _ = (&Docker{Root: t.TempDir()}).buildStack(context.Background(), a, "candidate", t.TempDir(), "commit", &m)
	raw, _ := os.ReadFile(actions)
	if strings.Contains(string(raw), "example/model") || strings.Contains(string(raw), "local-ai") {
		t.Fatal("disabled service materialized", string(raw))
	}
	if !strings.Contains(string(raw), "--platform linux/amd64") {
		t.Fatal("native platform not selected", string(raw))
	}
}
