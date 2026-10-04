package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRepositoryAndWebhookAPI(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer private-token" {
			t.Error("missing credential")
		}
		switch r.URL.Path {
		case "/user/repos":
			if r.URL.Query().Get("page") != "2" {
				t.Error("pagination missing")
			}
			w.Write([]byte(`[{"full_name":"acme/private","private":true,"default_branch":"main"}]`))
		case "/repos/acme/private/hooks", "/repos/acme/private/hooks/42":
			var body struct {
				Events []string          `json:"events"`
				Config map[string]string `json:"config"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Config["insecure_ssl"] != "0" || body.Config["secret"] != "secret" || len(body.Events) != 1 || body.Events[0] != "push" {
				t.Error("invalid hook configuration")
			}
			if r.URL.Path == "/repos/acme/private/hooks/42" && r.Method != "PATCH" {
				t.Error("hook not updated")
			}
			w.Write([]byte(`{"id":42}`))
		default:
			t.Error(r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	c := Client{API: s.URL}
	repos, err := c.Repositories(context.Background(), "private-token", 2)
	if err != nil || len(repos) != 1 || !repos[0].Private {
		t.Fatal(repos, err)
	}
	for _, id := range []int64{0, 42} {
		got, err := c.Hook(context.Background(), "private-token", "acme/private", "https://deploy.example.com/hooks/github/app", "secret", id)
		if err != nil || got != 42 {
			t.Fatal(got, err)
		}
	}
	if calls != 3 {
		t.Fatal(calls)
	}
}
