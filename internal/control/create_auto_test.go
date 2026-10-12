package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateAutoDeployRequiresVerifiedInstallationBeforeAgentChanges(t *testing.T) {
	calls := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{}`))
	}))
	defer remote.Close()
	c, err := New(Config{PublicURL: "https://deploy.example.com", User: "operator@example.com", Password: "fixture-password-16", DataDir: t.TempDir(), Key: []byte(strings.Repeat("k", 32)), Servers: []Server{{ID: "one", URL: remote.URL, Token: strings.Repeat("a", 32)}}})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"id":"portal","server_id":"one","repository":"acme/portal","branch":"master","domain":"portal.example.com","auto_deploy":true}`
	w := httptest.NewRecorder()
	c.create(w, httptest.NewRequest("POST", "/", strings.NewReader(body)))
	if w.Code != 400 || calls != 0 || len(c.data.Apps) != 0 {
		t.Fatal("unverified auto-deploy caused creation side effects", w.Code, calls, w.Body)
	}
	w = httptest.NewRecorder()
	c.create(w, httptest.NewRequest("POST", "/", strings.NewReader(strings.Replace(body, `"auto_deploy":true`, `"auto_deploy":false`, 1))))
	if w.Code != 201 || calls != 1 || c.data.Apps["portal"].AutoDeploy {
		t.Fatal("manual public app creation was not preserved", w.Code, calls, w.Body)
	}
}
