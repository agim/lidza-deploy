package control

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApplicationProcessesAreReadOnlyAndScopedWithoutCommands(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("process observation attempted mutation", r.Method)
		}
		switch r.URL.Path {
		case "/v1/containers":
			io.WriteString(w, `{"containers":[{"name":"web","app_id":"app","role":"Web · current release","state":"running"},{"name":"worker","app_id":"app","role":"Worker · queue","state":"exited"},{"name":"other-private-container","app_id":"other","role":"Web · current release","state":"running"}]}`)
		case "/v1/tasks":
			io.WriteString(w, `[{"id":"queue","app_id":"app","container":"worker","command":["SECRET COMMAND"],"enabled":false}]`)
		default:
			t.Error("unexpected route", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()
	c := &Control{client: remote.Client(), registry: []Server{{ID: "host", URL: remote.URL}}, data: saved{Apps: map[string]Application{"app": {ID: "app", ServerID: "host"}}}}
	r := httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("id", "app")
	w := httptest.NewRecorder()
	c.applicationProcesses(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "SECRET") || strings.Contains(w.Body.String(), "other-private-container") || !strings.Contains(w.Body.String(), `"startup":"disabled"`) {
		t.Fatal("scoping, secret masking or startup semantics", w.Code, w.Body.String())
	}
}
