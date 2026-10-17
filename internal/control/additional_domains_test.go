package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestAdditionalDomainSettingsReserveNamesAcrossServersAndPersist(t *testing.T) {
	calls := 0
	var forwarded agent.SettingsPatch
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if err := json.NewDecoder(r.Body).Decode(&forwarded); err != nil {
			t.Error(err)
		}
		w.Write([]byte(`{"status":"saved"}`))
	}))
	defer remote.Close()
	c := &Control{cfg: Config{DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32))}, client: remote.Client(), registry: []Server{{ID: "local", URL: remote.URL, Token: strings.Repeat("a", 32)}}, data: saved{Apps: map[string]Application{
		"portal": {ID: "portal", ServerID: "local", Repository: "acme/portal", Branch: "main", Domain: "portal.example.com", AdditionalDomains: []string{"saved.example.com"}},
		"other":  {ID: "other", ServerID: "remote", Domain: "other.example.com", AdditionalDomains: []string{"reserved.example.com"}},
	}}}
	send := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PATCH", "/", strings.NewReader(body))
		r.SetPathValue("id", "portal")
		w := httptest.NewRecorder()
		c.updateSettings(w, r)
		return w
	}
	for _, name := range []string{"other.example.com", "reserved.example.com"} {
		w := send(`{"branch":"main","domain":"portal.example.com","additional_domains":["` + name + `"]}`)
		if w.Code != 409 || calls != 0 {
			t.Fatal("global assignment collision reached agent", w.Code, w.Body.String(), calls)
		}
	}
	w := send(`{"branch":"main","domain":"portal.example.com","additional_domains":["new.example.org"]}`)
	if w.Code != 200 || forwarded.AdditionalDomains == nil || !slices.Equal(*forwarded.AdditionalDomains, []string{"new.example.org"}) || !slices.Equal(c.data.Apps["portal"].AdditionalDomains, []string{"new.example.org"}) {
		t.Fatal("alias save lost metadata", w.Code, w.Body.String())
	}
	w = send(`{"branch":"main","domain":"portal.example.com","env_changes":{}}`)
	if w.Code != 200 || !slices.Equal(c.data.Apps["portal"].AdditionalDomains, []string{"new.example.org"}) {
		t.Fatal("environment save removed aliases", w.Code)
	}
	w = send(`{"branch":"main","domain":"portal.example.com","additional_domains":[]}`)
	if w.Code != 200 || len(c.data.Apps["portal"].AdditionalDomains) != 0 || forwarded.AdditionalDomains == nil {
		t.Fatal("explicit clear failed", w.Code)
	}
}
