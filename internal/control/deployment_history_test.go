package control

import (
	"encoding/json"
	"github.com/agim/lidza-deploy/internal/agent"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFleetHistoryAvoidsOversizedLogsAndRetainsIndividualDetail(t *testing.T) {
	log := strings.Repeat("x", 65536)
	var legacy atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/deployments/one" {
			agent.JSON(w, 200, agent.Deployment{ID: "one", AppID: "app", Log: log})
			return
		}
		rows := make([]agent.Deployment, 100)
		for i := range rows {
			rows[i] = agent.Deployment{ID: "one", AppID: "app", Status: "building"}
			if legacy.Load() || r.URL.Query().Get("logs") != "0" {
				rows[i].Log = log
			}
		}
		agent.JSON(w, 200, rows)
	}))
	defer remote.Close()
	c, err := New(Config{PublicURL: "http://127.0.0.1:3000", User: "operator@example.com", Key: []byte(strings.Repeat("k", 32)), DataDir: t.TempDir(), Servers: []Server{{ID: "local", URL: remote.URL, Token: strings.Repeat("a", 32)}}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c.deployments(w, httptest.NewRequest("GET", "/", nil))
	var list struct {
		Deployments []agent.Deployment `json:"deployments"`
		Unavailable []string           `json:"unavailable_servers"`
		Errors      map[string]string  `json:"server_errors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Deployments) != 100 || len(list.Unavailable) != 0 || w.Body.Len() > 100000 {
		t.Fatal("history disappeared", w.Body.String())
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.SetPathValue("id", "one")
	w = httptest.NewRecorder()
	c.deployment(w, req)
	var detail agent.Deployment
	json.Unmarshal(w.Body.Bytes(), &detail)
	if w.Code != 200 || detail.Log != log {
		t.Fatal("individual build log unavailable")
	}
	legacy.Store(true)
	w = httptest.NewRecorder()
	c.deployments(w, httptest.NewRequest("GET", "/", nil))
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Unavailable) != 1 || !strings.Contains(list.Errors["local"], "size limit") {
		t.Fatal("oversized old-agent response failed silently", w.Body.String())
	}
}
