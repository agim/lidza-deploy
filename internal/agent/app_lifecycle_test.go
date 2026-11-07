package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// lifecycleCrontab is a crontab per user for the lifecycle tests.
type lifecycleCrontab struct {
	mu   sync.Mutex
	tabs map[string]string
}

func (f *lifecycleCrontab) command(_ context.Context, stdin string, args []string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	user := args[1]
	if args[len(args)-1] == "-l" {
		if f.tabs[user] == "" {
			return "", "no crontab for " + user, errLifecycleNoCrontab
		}
		return f.tabs[user], "", nil
	}
	f.tabs[user] = stdin
	return "", "", nil
}

type lifecycleNoCrontab struct{}

func (lifecycleNoCrontab) Error() string { return "exit status 1" }

var errLifecycleNoCrontab = lifecycleNoCrontab{}

func TestContainerLifecycleStopsStartsAndRestarts(t *testing.T) {
	rt := &stateRuntime{}
	m := testManager(t, rt)
	a := testApp("portal")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a = m.data.Apps["portal"]
	a.Current = &Release{ID: "current", Container: "web", Port: "12345"}
	m.data.Apps["portal"] = a
	m.mu.Unlock()
	ctx := context.Background()
	result, err := m.AppLifecycle(ctx, "portal", "stop", AppLifecycleRequest{By: "op"})
	if err != nil || result.Status != "stopped" {
		t.Fatalf("stop: %+v %v", result, err)
	}
	if len(rt.calls) == 0 || rt.calls[0][0] != "stop" {
		t.Fatalf("containers not stopped: %v", rt.calls)
	}
	w := httptest.NewRecorder()
	Proxy(m).ServeHTTP(w, httptest.NewRequest("GET", "http://"+a.Domain+"/", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "This application is stopped") {
		t.Fatalf("stopped page: %d %s", w.Code, w.Body)
	}
	if _, err := m.AppLifecycle(ctx, "portal", "restart", AppLifecycleRequest{}); err == nil || !strings.Contains(err.Error(), "start it instead") {
		t.Fatalf("a stopped application restarted: %v", err)
	}
	result, err = m.AppLifecycle(ctx, "portal", "start", AppLifecycleRequest{})
	if err != nil || result.Status != "starting" {
		t.Fatalf("start: %+v %v", result, err)
	}
	queued := slices.ContainsFunc(m.Deployments(), func(d Deployment) bool { return d.AppID == "portal" && d.Kind == "start" })
	if !queued {
		t.Fatal("start queued no deployment")
	}
	if _, err := m.AppLifecycle(ctx, "portal", "stop", AppLifecycleRequest{}); err == nil || !strings.Contains(err.Error(), "wait") {
		// The start's deployment runs: a stop waits for it.
		t.Fatalf("stop during the start's deployment: %v", err)
	}
	if _, err := m.AppLifecycle(ctx, "missing", "stop", AppLifecycleRequest{}); err != errLifecycleNotFound {
		t.Fatalf("missing app: %v", err)
	}
}

func TestNativeCreatedLifecyclePersistsStopAndPausesSchedule(t *testing.T) {
	f := newNativeCreateFixture(t)
	m := f.m
	tab := &lifecycleCrontab{tabs: map[string]string{}}
	m.nativeCrontabCommand = tab.command
	body := `{"id":"shop","repository":"acme/shop","branch":"main","runtime":"go","domain":"shop.example.com","data":{},"memory_mib":512}`
	if w := f.do(t, "POST", "/v1/native/apps", body); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	unit := "lidza-app-shop-web.service"
	f.states[unit] = "active/running/enabled"
	tab.tabs[nativeAppUser("shop")] = "MAILTO=\"\"\n\n# Begin Whenever generated tasks for: shop\n0 3 * * * run nightly\n# End Whenever generated tasks for: shop\n"
	ctx := context.Background()

	result, err := m.AppLifecycle(ctx, "shop", "stop", AppLifecycleRequest{By: "op"})
	if err != nil || result.Status != "stopped" || !slices.Equal(result.Units, []string{unit}) {
		t.Fatalf("stop: %+v %v", result, err)
	}
	if !f.ran("systemctl disable --now --quiet " + unit) {
		t.Fatalf("unit not disabled: %v", f.commands)
	}
	if got := tab.tabs[nativeAppUser("shop")]; strings.Contains(got, "nightly") || !strings.Contains(got, "MAILTO") {
		t.Fatalf("schedule not paused, or other lines lost: %q", got)
	}
	state, stopped := m.nativeStopped("shop")
	if !stopped || state.By != "op" || !strings.Contains(state.Schedule, "nightly") {
		t.Fatalf("state %+v", state)
	}
	// Views, settings and health report it; deploys and restarts are refused.
	views := m.nativeViews()
	if len(views) != 1 || !views[0].Stopped {
		t.Fatalf("view %+v", views)
	}
	if settings, err := m.nativeSettings("shop"); err != nil || !settings.Stopped {
		t.Fatalf("settings stopped=%v %v", settings.Stopped, err)
	}
	for _, health := range m.nativeApplicationHealth(ctx) {
		if health.AppID == "shop" && health.State != "stopped" {
			t.Fatalf("health %+v", health)
		}
	}
	if _, err := m.enqueueNativeRelease("shop", nativeReleaseRequest{}, false); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("deployed a stopped application: %v", err)
	}
	if _, err := m.AppLifecycle(ctx, "shop", "restart", AppLifecycleRequest{}); err == nil {
		t.Fatal("restarted a stopped application")
	}
	w := httptest.NewRecorder()
	Proxy(m).ServeHTTP(w, httptest.NewRequest("GET", "http://shop.example.com/", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "This application is stopped") {
		t.Fatalf("stopped page: %d %s", w.Code, w.Body)
	}
	// Stopping again finishes nothing new and keeps which units ran.
	f.states[unit] = "inactive/dead/disabled"
	if result, err := m.AppLifecycle(ctx, "shop", "stop", AppLifecycleRequest{}); err != nil || !slices.Equal(result.Units, []string{unit}) {
		t.Fatalf("second stop: %+v %v", result, err)
	}

	result, err = m.AppLifecycle(ctx, "shop", "start", AppLifecycleRequest{})
	if err != nil || result.Status != "starting" {
		t.Fatalf("start: %+v %v", result, err)
	}
	if !f.ran("systemctl enable --now --no-block --quiet " + unit) {
		t.Fatalf("unit not started: %v", f.commands)
	}
	if got := tab.tabs[nativeAppUser("shop")]; !strings.Contains(got, "nightly") || !strings.Contains(got, "MAILTO") {
		t.Fatalf("schedule not restored: %q", got)
	}
	if _, stopped := m.nativeStopped("shop"); stopped {
		t.Fatal("still stopped")
	}
	f.states[unit] = "active/running/enabled"
	if result, err := m.AppLifecycle(ctx, "shop", "restart", AppLifecycleRequest{}); err != nil || !slices.Equal(result.Units, []string{unit}) {
		t.Fatalf("restart: %+v %v", result, err)
	}
	if !f.ran("systemctl restart --no-block " + unit) {
		t.Fatalf("unit not restarted: %v", f.commands)
	}
}

// adoptedLifecycleFixture is an adopted Rails application on the shared
// deployment user's manager: its server runs, its worker is kept disabled.
func adoptedLifecycleFixture(t *testing.T) (*Manager, *[][]string) {
	t.Helper()
	m, _ := nativeFixture(t)
	if _, err := m.nativeRegistration(NativeApplication{ID: "my-rails", Name: "MyRails", Domain: "rails.example.com", Repository: "acme/rails"}); err != nil {
		t.Fatal(err)
	}
	calls := &[][]string{}
	var mu sync.Mutex
	m.nativeCommand = func(_ context.Context, args []string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		*calls = append(*calls, slices.Clone(args))
		if len(args) > 2 && args[1] == "--user" && args[2] == "show" {
			return "Id=myrails-server.service\nActiveState=active\nSubState=running\nUnitFileState=enabled\n\nId=myrails-worker.service\nActiveState=inactive\nSubState=dead\nUnitFileState=disabled", nil
		}
		return "", nil
	}
	return m, calls
}

func ranCommand(calls *[][]string, want ...string) bool {
	return slices.ContainsFunc(*calls, func(args []string) bool { return slices.Equal(args, want) })
}

func TestNativeAdoptedLifecycleNeedsServiceControlAndKeepsDisabledUnits(t *testing.T) {
	m, calls := adoptedLifecycleFixture(t)
	ctx := context.Background()
	a, _ := m.nativeApp("my-rails")
	if capability := m.nativeManagementCapabilities(a); capability.Lifecycle || capability.LifecycleReason == "" || !capability.Remove || !capability.Adopted {
		t.Fatalf("capabilities without service control: %+v", capability)
	}
	if _, err := m.AppLifecycle(ctx, "my-rails", "stop", AppLifecycleRequest{}); err == nil || len(*calls) != 0 {
		t.Fatalf("stopped without service control: %v %v", err, *calls)
	}
	m.cfg.Native.ServiceControl = true
	if capability := m.nativeManagementCapabilities(a); !capability.Lifecycle || !capability.Remove {
		t.Fatalf("capabilities with service control: %+v", capability)
	}
	result, err := m.AppLifecycle(ctx, "my-rails", "stop", AppLifecycleRequest{})
	if err != nil || !slices.Equal(result.Units, []string{"myrails-server.service"}) {
		t.Fatalf("stop: %+v %v", result, err)
	}
	for _, unit := range []string{"myrails-server.service", "myrails-worker.service"} {
		if !ranCommand(calls, "systemctl", "--user", "disable", "--now", "--quiet", unit) {
			t.Fatalf("%s not disabled: %v", unit, *calls)
		}
	}
	*calls = nil
	if _, err := m.AppLifecycle(ctx, "my-rails", "start", AppLifecycleRequest{}); err != nil {
		t.Fatal(err)
	}
	if !ranCommand(calls, "systemctl", "--user", "enable", "--now", "--no-block", "--quiet", "myrails-server.service") {
		t.Fatalf("server not started: %v", *calls)
	}
	if slices.ContainsFunc(*calls, func(args []string) bool { return slices.Contains(args, "myrails-worker.service") }) {
		t.Fatalf("the disabled worker was started: %v", *calls)
	}
	*calls = nil
	if result, err := m.AppLifecycle(ctx, "my-rails", "restart", AppLifecycleRequest{}); err != nil || !slices.Equal(result.Units, []string{"myrails-server.service"}) {
		t.Fatalf("restart: %+v %v", result, err)
	}
	if slices.ContainsFunc(*calls, func(args []string) bool {
		return slices.Contains(args, "restart") && slices.Contains(args, "myrails-worker.service")
	}) {
		t.Fatalf("restart touched the disabled worker: %v", *calls)
	}
}

func TestStoppedCaddyRouteServesTheStoppedPage(t *testing.T) {
	raw, err := renderStoppedCaddyRoute("shop", []string{"shop.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"@id":"lidza-stopped-shop"`, `"static_response"`, `"status_code":503`, "This application is stopped"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("route lacks %s: %s", want, raw)
		}
	}
	if _, err := renderStoppedCaddyRoute("shop", nil); err == nil {
		t.Fatal("a route without hostnames")
	}
}
