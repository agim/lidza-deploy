package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStoppedApplicationDropsPushAndScheduledDispatch(t *testing.T) {
	calls := []string{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/settings") {
			w.Write([]byte(`{"stopped":true}`))
			return
		}
		t.Error("stopped app reached deploy or task execution", r.URL.Path)
		w.WriteHeader(500)
	}))
	defer remote.Close()
	c := &Control{client: remote.Client(), cfg: Config{PublicURL: "http://127.0.0.1:3000"}, registry: []Server{{ID: "local", URL: remote.URL}}, data: saved{Apps: map[string]Application{"portal": {ID: "portal", ServerID: "local", AutoDeploy: true, Generation: "one"}}}}
	push, _ := json.Marshal(pushJob{AppID: "portal", Generation: "one", Delivery: "push"})
	if err := c.dispatchPush(context.Background(), push); err != nil {
		t.Fatal(err)
	}
	task, _ := json.Marshal(taskJob{AppID: "portal", ServerID: "local", Generation: "one", TaskID: "job", Key: "due"})
	if err := c.dispatchTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatal(calls)
	}
}
