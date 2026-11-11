package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/agentlink"
)

// linkedAgent is an agent API reached only through links.
func linkedAgent(t *testing.T, panelURL string) {
	t.Helper()
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+panelTestToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/host/control":
			agent.JSON(w, 200, agent.ControlHostStatus{MachineID: "machine-old", Links: true})
		case "POST /v1/apps/shop/restart":
			agent.JSON(w, 200, map[string]string{"status": "restarted"})
		default:
			http.NotFound(w, r)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go agentlink.Agent{URL: panelURL, Server: "old", Token: panelTestToken, Handler: api, Idle: 2}.Run(ctx)
}

func TestAgentLinkCarriesPanelCalls(t *testing.T) {
	dir := t.TempDir()
	c, err := New(Config{PublicURL: "https://deploy.example.com", User: "operator@example.com", Key: []byte(strings.Repeat("k", 32)), DataDir: dir, Servers: []Server{
		{ID: "old", Name: "Old host", URL: agentlink.URL("old"), Token: panelTestToken},
	}})
	if err != nil {
		t.Fatal(err)
	}
	panel := httptest.NewServer(c.Handler(http.NotFoundHandler()))
	t.Cleanup(panel.Close)
	linkedAgent(t, panel.URL)
	for deadline := time.Now().Add(3 * time.Second); c.links.Idle("old") < 2 && time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
	}
	r := httptest.NewRequest("GET", "/", nil)
	var status agent.ControlHostStatus
	if err := c.agentCall(r, "old", http.MethodGet, "/v1/host/control", nil, &status); err != nil || status.MachineID != "machine-old" {
		t.Fatalf("call through the link: %v %+v", err, status)
	}
	var out map[string]string
	if err := c.agentCall(r, "old", http.MethodPost, "/v1/apps/shop/restart", map[string]string{"confirm": "shop"}, &out); err != nil || out["status"] != "restarted" {
		t.Fatalf("post through the link: %v %v", err, out)
	}

	// A wrong token gets no link.
	req, _ := http.NewRequest("GET", panel.URL+agentlink.Path, nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 32))
	req.Header.Set(agentlink.ServerHeader, "old")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", agentlink.Protocol)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", res.StatusCode)
	}
}

func TestStandbyPanelProbesTheLink(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "servers.json")
	raw, _ := json.Marshal([]Server{{ID: "old", URL: agentlink.URL("old"), Token: panelTestToken}})
	os.WriteFile(file, raw, 0o600)
	standby := httptest.NewServer(StandbyHandler(file))
	t.Cleanup(standby.Close)
	probe := func() map[string]any {
		req, _ := http.NewRequest("POST", standby.URL+agentlink.ProbePath, nil)
		req.Header.Set("Authorization", "Bearer "+panelTestToken)
		req.Header.Set(agentlink.ServerHeader, "old")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	linkedAgent(t, standby.URL)
	var out map[string]any
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if out = probe(); out["reachable"] == true {
			break
		}
	}
	if out["reachable"] != true || out["machine_id"] != "machine-old" {
		t.Fatalf("probe %v", out)
	}
	// Everything else is the standby notice.
	res, _ := http.Get(standby.URL + "/")
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("notice %d", res.StatusCode)
	}
}

func TestPanelMoveFailsWhenTheLinkDoesNotAnswer(t *testing.T) {
	fx := newPanelFixture(t)
	fx.linkErr = errors.New("no open link")
	if code, body := panelPost(t, fx.c, "", PanelMoveInput{Target: "combined"}); code != 202 {
		t.Fatal(code, body)
	}
	move := waitPanel(t, fx.c, panelMoveFailed, panelMoveStandby)
	if move.State != panelMoveFailed || move.step(panelStepLink).Status != "failed" || !strings.Contains(move.Error, "no open link") {
		t.Fatalf("state %s error %q step %+v", move.State, move.Error, move.step(panelStepLink))
	}
}
