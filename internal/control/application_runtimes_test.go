package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestLegacyGoSpecDoesNotAcquireUnsupportedRuntimeFields(t *testing.T) {
	raw, err := json.Marshal(agent.App{ID: "old-app", Repository: "org/app", Branch: "main", Domain: "app.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	json.Unmarshal(raw, &fields)
	for _, key := range []string{"runtime", "astro", "build"} {
		if _, exists := fields[key]; exists {
			t.Fatal("legacy Go request contains new fields", key, string(raw))
		}
	}
}

func TestAstroRuntimeRequiresAgentCapabilityBeforeAppCreation(t *testing.T) {
	calls := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	c := &Control{client: server.Client(), registry: []Server{{ID: "old", URL: server.URL, Token: strings.Repeat("s", 32)}}}
	r := httptest.NewRequest("POST", "/", nil)
	if err := c.verifyApplicationRuntime(r, "old", agent.App{Runtime: "astro"}); err == nil {
		t.Fatal("old agent accepted an unsupported runtime")
	}
	if len(calls) != 1 || calls[0] != "GET /v1/application-runtimes" {
		t.Fatal("capability check sent a host mutation", calls)
	}
	if err := c.verifyApplicationRuntime(r, "old", agent.App{}); err != nil {
		t.Fatal("legacy Go behavior changed", err)
	}
}
