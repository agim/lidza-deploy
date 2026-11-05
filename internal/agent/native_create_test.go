package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

// nativeCreateFixture is an agent with native management and fakes for the
// account database, systemd and port probing.
type nativeCreateFixture struct {
	m        *Manager
	mux      *http.ServeMux
	appsRoot string
	units    string
	mu       sync.Mutex
	commands []string
	accounts map[string]bool
	states   map[string]string // unit -> ActiveState/SubState/UnitFileState
}

func newNativeCreateFixture(t *testing.T) *nativeCreateFixture {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("the fixture uses the test user's own uid for application accounts")
	}
	m := testManager(t, &fakeRuntime{})
	base := t.TempDir()
	f := &nativeCreateFixture{m: m, appsRoot: filepath.Join(base, "apps"), units: t.TempDir(), accounts: map[string]bool{},
		states: map[string]string{}}
	legacy := filepath.Join(base, "legacy")
	if err := os.Mkdir(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	m.cfg.Native = &NativeHostConfig{Root: legacy, User: "agim", AppsRoot: f.appsRoot}
	restore := []func(){}
	set := func(undo func()) { restore = append(restore, undo) }
	oldLookup, oldUnits, oldForbidden, oldListen := nativeLookupUser, nativeSystemUnitDir, nativeForbiddenAppsRoots, nativePortListenable
	set(func() {
		nativeLookupUser, nativeSystemUnitDir, nativeForbiddenAppsRoots, nativePortListenable = oldLookup, oldUnits, oldForbidden, oldListen
	})
	t.Cleanup(func() {
		for _, undo := range restore {
			undo()
		}
	})
	nativeSystemUnitDir = f.units
	nativeForbiddenAppsRoots = nil
	nativePortListenable = func(port int) bool { return port != 4101 }
	nativeLookupUser = func(name string) (*user.User, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.accounts[name] {
			return nil, user.UnknownUserError(name)
		}
		id := strings.TrimPrefix(name, nativeAppUserPrefix)
		return &user.User{Username: name, Uid: strconv.Itoa(os.Getuid()), Gid: strconv.Itoa(os.Getgid()), HomeDir: filepath.Join(f.appsRoot, id)}, nil
	}
	m.nativeCommand = func(_ context.Context, args []string) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.commands = append(f.commands, strings.Join(args, " "))
		switch {
		case args[0] == "useradd":
			f.accounts[args[len(args)-1]] = true
		case args[0] == "userdel":
			delete(f.accounts, args[len(args)-1])
		case len(args) > 2 && args[0] == "systemctl" && args[1] == "show":
			var out []string
			for _, unit := range args[2:] {
				if strings.HasPrefix(unit, "--") {
					continue
				}
				state := f.states[unit]
				if state == "" {
					state = "inactive/dead/enabled"
				}
				parts := strings.Split(state, "/")
				out = append(out, "Id="+unit+"\nActiveState="+parts[0]+"\nSubState="+parts[1]+"\nUnitFileState="+parts[2])
			}
			return strings.Join(out, "\n\n"), nil
		}
		return "", nil
	}
	f.mux = http.NewServeMux()
	m.nativeRoutes(f.mux)
	return f
}

func (f *nativeCreateFixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

func (f *nativeCreateFixture) ran(command string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.commands, func(c string) bool { return strings.HasPrefix(c, command) })
}

func TestNativeCreateGoApplicationEndToEnd(t *testing.T) {
	f := newNativeCreateFixture(t)
	body := `{"id":"shop","repository":"acme/shop","branch":"main","runtime":"go","domain":"shop.example.com",
		"data":{},"env":{"API_TOKEN":"token-value","LIDZA_MASTER_KEY":"` + strings.Repeat("ab", 32) + `"},"memory_mib":512}`
	w := f.do(t, "POST", "/v1/native/apps", body)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var view App
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	unit := "lidza-app-shop-web.service"
	if view.Native == nil || view.Native.Port != "4100" || !slices.Equal(view.Native.Services, []string{unit}) || view.Runtime != "go" {
		t.Fatalf("view = %+v", view.Native)
	}
	root := filepath.Join(f.appsRoot, "shop")
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("root mode: %v %v", info, err)
	}
	envFile := filepath.Join(root, "shared", ".env")
	info, err := os.Stat(envFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("env file mode: %v %v", info, err)
	}
	env, _ := os.ReadFile(envFile)
	for _, line := range []string{"PORT=4100", "LIDZA_ADDR=127.0.0.1:4100", "API_TOKEN=token-value", "STORAGE_DIR=" + filepath.Join(root, "shared", "storage")} {
		if !strings.Contains(string(env), line+"\n") {
			t.Fatalf("env lacks %s:\n%s", line, env)
		}
	}
	rendered, err := os.ReadFile(filepath.Join(f.units, unit))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"User=app-shop", `ExecStart=/bin/sh -c "exec bin/shop"`, "MemoryMax=512M", "WorkingDirectory=" + filepath.Join(root, "current")} {
		if !strings.Contains(string(rendered), line+"\n") {
			t.Fatalf("unit lacks %s:\n%s", line, rendered)
		}
	}
	for _, command := range []string{"useradd --system --user-group --home-dir " + root, "systemctl daemon-reload", "systemctl enable --quiet " + unit} {
		if !f.ran(command) {
			t.Fatalf("missing %q in %v", command, f.commands)
		}
	}
	if f.ran("systemctl start") || f.ran("systemctl restart") {
		t.Fatal("creation must not start the units before the first release")
	}
	_, policy, err := f.m.nativeReleasePolicy("shop")
	if err != nil || policy.User != "app-shop" || !policy.SystemUnits() || policy.Port != "4100" || !slices.Equal(policy.EnvFiles, []string{envFile}) {
		t.Fatalf("policy = %+v, %v", policy, err)
	}
	if !f.m.nativeReleaseManaged("shop") {
		t.Fatal("created applications deploy through the release engine")
	}
	status := f.do(t, "GET", "/v1/native/apps/shop", "")
	if status.Code != 200 || !strings.Contains(status.Body.String(), `"service_control":true`) || !strings.Contains(status.Body.String(), `"release_managed":true`) {
		t.Fatalf("status: %d %s", status.Code, status.Body)
	}
	if again := f.do(t, "POST", "/v1/native/apps", body); again.Code != 409 {
		t.Fatalf("duplicate create: %d %s", again.Code, again.Body)
	}
	clash := strings.Replace(strings.Replace(body, `"shop"`, `"shop2"`, 1), "acme/shop", "acme/shop2", 1)
	if w := f.do(t, "POST", "/v1/native/apps", clash); w.Code != 409 || !strings.Contains(w.Body.String(), "domain") {
		t.Fatalf("domain clash: %d %s", w.Code, w.Body)
	}

	if a, _, ok := f.m.nativeCreatedHost("SHOP.example.com"); !ok || a.ID != "shop" {
		t.Fatal("the proxy must route the created application's domain")
	}

	removed := false
	oldRelease := releaseNativeData
	releaseNativeData = func(context.Context, *Manager, string) error { removed = true; return nil }
	t.Cleanup(func() { releaseNativeData = oldRelease })
	if w := f.do(t, "DELETE", "/v1/native/apps/shop?purge=1", ""); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if !removed || !f.ran("systemctl disable --now --quiet "+unit) || !f.ran("userdel app-shop") {
		t.Fatalf("delete commands: %v", f.commands)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("purge must delete the application root")
	}
	if _, err := os.Stat(filepath.Join(f.units, unit)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("delete must remove the unit file")
	}
	if _, found := f.m.nativeCreatedPolicy("shop"); found || f.m.nativeCreatedDomain("shop.example.com") {
		t.Fatal("delete must forget the application and its route")
	}
}

func TestNativeCreateUndoesEverythingWhenDataIsUnavailable(t *testing.T) {
	f := newNativeCreateFixture(t)
	old := provisionNativeData
	provisionNativeData = func(context.Context, *Manager, string, NativeAppAccount, NativeDataRequest) (NativeDataGrant, error) {
		return NativeDataGrant{}, errNativeCreateUnavailable
	}
	t.Cleanup(func() { provisionNativeData = old })
	w := f.do(t, "POST", "/v1/native/apps", `{"id":"blog","repository":"acme/blog","runtime":"rails","domain":"blog.example.com","data":{"postgres":true}}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "cannot provision databases") {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if !f.ran("userdel app-blog") {
		t.Fatalf("the account must be removed again: %v", f.commands)
	}
	if _, err := os.Stat(filepath.Join(f.appsRoot, "blog")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a root created for the failed application must be removed")
	}
	if _, found := f.m.nativeCreatedPolicy("blog"); found {
		t.Fatal("a failed create must not persist a policy")
	}
}

func TestNativeCreateRailsDefaultsAndStaticSites(t *testing.T) {
	f := newNativeCreateFixture(t)
	w := f.do(t, "POST", "/v1/native/apps", `{"id":"blog","repository":"acme/blog","runtime":"rails","domain":"blog.example.com","data":{"external_database_url":"postgres://u:p@db.internal/blog"},
		"processes":[{"name":"jobs","command":"bin/jobs"},{"name":"web","command":"bin/rails server -b 127.0.0.1 -p \"$PORT\"","web":true}]}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	policy, _ := f.m.nativeCreatedPolicy("blog")
	// The web process comes first in restart order.
	if !slices.Equal(policy.Units, []string{"lidza-app-blog-web.service", "lidza-app-blog-jobs.service"}) || policy.HealthPath != "/up" {
		t.Fatalf("policy = %+v", policy)
	}
	env, _ := os.ReadFile(policy.EnvFiles[0])
	for _, line := range []string{"RAILS_ENV=production", "RAILS_LOG_TO_STDOUT=true", "DATABASE_URL=postgres://u:p@db.internal/blog", "SECRET_KEY_BASE="} {
		if !strings.Contains(string(env), line) {
			t.Fatalf("env lacks %s:\n%s", line, env)
		}
	}
	w = f.do(t, "POST", "/v1/native/apps", `{"id":"site","repository":"acme/site","runtime":"static","domain":"site.example.com","data":{}}`)
	if w.Code != 201 {
		t.Fatalf("static create: %d %s", w.Code, w.Body)
	}
	site, _ := f.m.nativeCreatedPolicy("site")
	if len(site.Units) != 0 || site.Port != "" || site.Output != "dist" {
		t.Fatalf("static policy = %+v", site)
	}
	if blog, _ := f.m.nativeCreatedPolicy("blog"); blog.Port != "4100" {
		t.Fatalf("blog port = %s", blog.Port)
	}
	// 4101 is in use by another process on the host: the next is 4102.
	if w := f.do(t, "POST", "/v1/native/apps", `{"id":"api","repository":"acme/api","runtime":"node","domain":"api.example.com","data":{}}`); w.Code != 201 {
		t.Fatalf("node create: %d %s", w.Code, w.Body)
	}
	if api, _ := f.m.nativeCreatedPolicy("api"); api.Port != "4102" || api.HealthPath != "/" {
		t.Fatalf("api policy = %+v", api)
	}
	for name, body := range map[string]string{
		"two web":      `{"id":"x1","repository":"a/b","runtime":"node","domain":"x1.example.com","data":{},"processes":[{"name":"a","command":"a","web":true},{"name":"b","command":"b","web":true}]}`,
		"host key":     `{"id":"x2","repository":"a/b","runtime":"node","domain":"x2.example.com","data":{},"env":{"PORT":"1"}}`,
		"long id":      `{"id":"` + strings.Repeat("a", 29) + `","repository":"a/b","runtime":"node","domain":"x3.example.com","data":{}}`,
		"runtime":      `{"id":"x4","repository":"a/b","runtime":"php","domain":"x4.example.com","data":{}}`,
		"static procs": `{"id":"x5","repository":"a/b","runtime":"static","domain":"x5.example.com","data":{},"processes":[{"name":"web","command":"x","web":true}]}`,
	} {
		if w := f.do(t, "POST", "/v1/native/apps", body); w.Code != 409 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
}

func TestNativeCreateLeavesRegistrationRequestsAlone(t *testing.T) {
	f := newNativeCreateFixture(t)
	w := f.do(t, "POST", "/v1/native/apps", `{"id":"legacy","name":"Legacy","domain":"legacy.example.com","repository":"acme/legacy","branch":"main","health_path":"/up","services":[],"registered":"2026-01-01T00:00:00Z"}`)
	if w.Code == 201 || strings.Contains(w.Body.String(), "runtime must be") {
		t.Fatalf("registration was treated as a create: %d %s", w.Code, w.Body)
	}
	if _, found := f.m.nativeCreatedPolicy("legacy"); found {
		t.Fatal("registration must not create an application")
	}
}

type fakeSystemServices struct {
	states    map[string]release.ServiceState
	restarted []string
}

func (s *fakeSystemServices) State(_ context.Context, unit string) (release.ServiceState, error) {
	return s.states[unit], nil
}

func (s *fakeSystemServices) Restart(_ context.Context, unit string) error {
	s.restarted = append(s.restarted, unit)
	s.states[unit] = release.ServiceState{Active: "active", Sub: "running", UnitFile: s.states[unit].UnitFile}
	return nil
}

func TestNativeSystemRestartStartsEnabledUnitsAndKeepsStoppedOnes(t *testing.T) {
	services := &fakeSystemServices{states: map[string]release.ServiceState{
		"web.service":     {Active: "inactive", Sub: "dead", UnitFile: "enabled"},  // first release
		"jobs.service":    {Active: "active", Sub: "running", UnitFile: "enabled"}, // running
		"stopped.service": {Active: "inactive", Sub: "dead", UnitFile: "disabled"}, // stopped by the operator
		"kept.service":    {Active: "active", Sub: "running", UnitFile: "enabled"}, // preserved
	}}
	restarted, err := nativeSystemRestart(context.Background(), services, []string{"web.service", "jobs.service", "stopped.service", "kept.service"},
		[]string{"kept.service"}, time.Second, nil)
	if err != nil || !slices.Equal(restarted, []string{"web.service", "jobs.service"}) || !slices.Equal(services.restarted, restarted) {
		t.Fatalf("restarted %v (%v), err %v", restarted, services.restarted, err)
	}
}

func TestNativeCreatedPlanUsesSystemUnitsAndManagedToolchains(t *testing.T) {
	f := newNativeCreateFixture(t)
	if w := f.do(t, "POST", "/v1/native/apps", `{"id":"shop","repository":"acme/shop","runtime":"go","domain":"shop.example.com","data":{}}`); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	_, policy, err := f.m.nativeReleasePolicy("shop")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := f.m.nativeReleasePlan(context.Background(), "shop", policy, "d1", "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Services != nil || len(plan.Units) != 0 || plan.Health == nil || plan.Prepare == nil || !plan.SkipBackup {
		t.Fatalf("plan = services %v units %v health %v prepare %v skip %v", plan.Services, plan.Units, plan.Health != nil, plan.Prepare != nil, plan.SkipBackup)
	}
	if plan.Profile.Name != "go" {
		t.Fatalf("profile = %s", plan.Profile.Name)
	}
	path := ""
	for _, entry := range plan.Env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			path = value
		}
	}
	if !strings.HasPrefix(path, filepath.Join(policy.Root, ".lidza-build")+"/bin:") || !slices.Contains(plan.Env, "GOCACHE="+filepath.Join(policy.Root, ".cache", "go-build")) {
		t.Fatalf("build environment = %v", plan.Env)
	}
}
