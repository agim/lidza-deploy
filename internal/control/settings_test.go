package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSettingsIncludesControlApplicationMetadata(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"environment":[],"backup_before_deploy":true}`))
	}))
	defer remote.Close()
	c := &Control{client: remote.Client(), registry: []Server{{ID: "local", URL: remote.URL, Token: strings.Repeat("a", 32)}}, data: saved{Apps: map[string]Application{"portal": {ID: "portal", ServerID: "local", Repository: "acme/portal", Branch: "main", Domain: "portal.example.com", GitHubInstallation: 42}}}}
	r := httptest.NewRequest(http.MethodGet, "/api/control/apps/portal/settings", nil)
	r.SetPathValue("id", "portal")
	w := httptest.NewRecorder()
	c.settings(w, r)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var got struct {
		ID                 string `json:"id"`
		ServerID           string `json:"server_id"`
		Repository         string `json:"repository"`
		Branch             string `json:"branch"`
		Domain             string `json:"domain"`
		GitHubInstallation int64  `json:"github_installation"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "portal" || got.ServerID != "local" || got.Repository != "acme/portal" || got.Branch != "main" || got.Domain != "portal.example.com" || got.GitHubInstallation != 42 {
		t.Fatalf("incomplete settings metadata: %+v", got)
	}
}

func TestSettingsForwardsV2CapabilityAndPublicSelection(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 32) {
			t.Error("agent request authentication missing")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"stack_manifest_versions":[1,2],"runtime_selection":{"version":1,"revision":"draft-7","values":{"AI_RUNTIME":"local"}},"environment":[{"key":"DEPLOY_RUNTIME_TOKEN","kind":"credential","preview":"abcd…","length":48}]}`))
	}))
	defer remote.Close()
	c := &Control{client: remote.Client(), registry: []Server{{ID: "local", URL: remote.URL, Token: strings.Repeat("a", 32)}}, data: saved{Apps: map[string]Application{"portal": {ID: "portal", ServerID: "local", Branch: "main", Domain: "portal.example.com"}}}}
	r := httptest.NewRequest("GET", "/api/control/apps/portal/settings", nil)
	r.SetPathValue("id", "portal")
	w := httptest.NewRecorder()
	c.settings(w, r)
	var got struct {
		Versions  []int `json:"stack_manifest_versions"`
		Selection struct {
			Version  int               `json:"version"`
			Revision string            `json:"revision"`
			Values   map[string]string `json:"values"`
		} `json:"runtime_selection"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(got.Versions) != 2 || got.Versions[1] != 2 || got.Selection.Version != 1 || got.Selection.Revision != "draft-7" || got.Selection.Values["AI_RUNTIME"] != "local" {
		t.Fatal("v2 contract metadata lost", w.Body.String())
	}
}
