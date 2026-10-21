package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestDockerStackV2Lifecycle(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1 for isolated stack-v2 lifecycle acceptance")
	}
	t.Setenv("TEST_STACK_V2", "1")
	TestDockerStackProfilesLifecycle(t)
}
func realStackV2Manifest(t *testing.T, ctx context.Context, m StackManifest) StackManifest {
	t.Helper()
	raw, err := command(ctx, "", nil, "docker", "image", "inspect", "--format", `{{json .RepoDigests}}`, cacheImage)
	if err != nil {
		t.Fatal(err)
	}
	var pins []string
	if json.Unmarshal([]byte(raw), &pins) != nil || len(pins) == 0 {
		t.Fatal("cached Valkey fixture needs an immutable repository digest")
	}
	m.Version = 2
	m.GeneratedSecrets = append(m.GeneratedSecrets, "DEPLOY_RUNTIME_TOKEN")
	m.RuntimeSelection = &StackRuntimeSelection{Selectors: map[string][]string{"AI_RUNTIME": {"remote", "local"}}, Initial: map[string]string{"AI_RUNTIME": "remote"}, Path: "/.well-known/deploy-runtime", TokenKey: "DEPLOY_RUNTIME_TOKEN"}
	m.Services = append(m.Services, StackService{Name: "matrix", Role: "auxiliary", Policy: "init", Build: &StackBuild{Dockerfile: "Dockerfile"}, Environment: map[string]string{"FIXTURE_ROLE": "matrix", "MATRIX_DATABASE_URL": "${DATABASE_URL}"}, DatabaseBindings: []string{"DATABASE_URL"}, Volumes: []StackVolume{{Name: "matrix-state", Target: "/var/lib/matrix"}}, HealthCommand: []string{"/fixture", "--health"}}, StackService{Name: "local-ai", Role: "auxiliary", Policy: "hardened", Image: pins[0], EnabledWhen: &StackConfigurationCondition{Name: "AI_RUNTIME", Equals: "local"}, Command: []string{"valkey-server", "--save", "", "--appendonly", "no"}, HealthCommand: []string{"valkey-cli", "ping"}})
	m.ProfileResources = map[string]StackProfileResources{}
	for _, p := range stackProfiles {
		mailCPU, _ := budgetCPU(p.Mail)
		m.ProfileResources[p.ID] = StackProfileResources{Database: p.Database, Services: map[string]StackBudget{"app": p.App, "mail": {MemoryMiB: p.Mail.MemoryMiB - 256, CPUs: fmt.Sprintf("%.3f", float64(mailCPU-100)/1000)}, "office": p.Office, "matrix": {MemoryMiB: 64, CPUs: "0.05"}, "local-ai": {MemoryMiB: 128, CPUs: "0.05"}}}
	}
	return m
}
func exerciseStackV2Selection(t *testing.T, ctx context.Context, m *Manager, id, proxyURL, domain string, request func(string, string) string, deploy func(bool) Deployment, releases *[]*Release) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", proxyURL+"/.well-known/deploy-runtime", nil)
	req.Host = domain
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatal("runtime selection endpoint exposed through public proxy", response.StatusCode)
	}
	hasLocal := func() bool {
		for _, c := range m.Current(id).Stack.Containers {
			if c.Service.Name == "local-ai" {
				return true
			}
		}
		return false
	}
	if hasLocal() {
		t.Fatal("remote provider unnecessarily runs local backend")
	}
	if request("POST", "/fixture/runtime?desired=local") != "saved" {
		t.Fatal("save desired local")
	}
	local := deploy(true)
	if local.Status != "live" {
		t.Fatal("saved local selection reload", local.Error, local.Log)
	}
	*releases = append(*releases, m.Current(id))
	if !hasLocal() {
		t.Fatal("saved desired local did not enable backend before publication")
	}
	if request("POST", "/fixture/runtime?publish=true") != "saved" {
		t.Fatal("publish local")
	}
	if request("POST", "/fixture/runtime?desired=remote") != "saved" {
		t.Fatal("save desired remote")
	}
	held := deploy(true)
	if held.Status != "live" {
		t.Fatal("hold active local", held.Error, held.Log)
	}
	*releases = append(*releases, m.Current(id))
	if !hasLocal() {
		t.Fatal("remote draft stopped backend still needed by active local")
	}
	if request("POST", "/fixture/runtime?publish=true") != "saved" {
		t.Fatal("publish remote")
	}
	remote := deploy(true)
	if remote.Status != "live" {
		t.Fatal("remote selection reload", remote.Error, remote.Log)
	}
	*releases = append(*releases, m.Current(id))
	if hasLocal() {
		t.Fatal("published remote still starts local backend")
	}
	output, err := m.runtime.Logs(ctx, m.Current(id))
	if err != nil || !strings.Contains(output, "matrix") {
		t.Fatal("auxiliary runtime logs unavailable", err)
	}
	settings, err := m.Settings(id)
	if err != nil || settings.RuntimeSelection == nil || settings.RuntimeSelection.Values["AI_RUNTIME"] != "remote" {
		t.Fatal("committed selector metadata missing", err)
	}
	t.Log("Real v2 auxiliary DB health, active+saved-desired union, local activation, draft remote retention, remote deactivation, public endpoint isolation and inventory/log metadata passed; this is a platform fixture, not a Matrix/model capacity claim")
}
