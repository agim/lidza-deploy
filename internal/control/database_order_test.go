package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDatabaseFleetOrderAndUnavailableIdentity(t *testing.T) {
	offline := false
	z := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"app_id":"omega"},{"app_id":"alpha"}]`))
	}))
	defer z.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`[{"app_id":"zeta"},{"app_id":"beta"}]`))
	}))
	defer a.Close()
	c, err := New(Config{PublicURL: "http://localhost:3000", User: "operator@example.com", Password: "fixture-password-16", Key: []byte(strings.Repeat("k", 32)), DataDir: t.TempDir(), Servers: []Server{{ID: "z", Name: "Zulu", URL: z.URL, Token: strings.Repeat("z", 32)}, {ID: "a", Name: "Alpha", URL: a.URL, Token: strings.Repeat("a", 32)}}})
	if err != nil {
		t.Fatal(err)
	}
	call := func() struct {
		Databases   []fleetDatabase `json:"databases"`
		Unavailable []string        `json:"unavailable_server_ids"`
	} {
		w := httptest.NewRecorder()
		c.databases(w, httptest.NewRequest("GET", "/", nil))
		var result struct {
			Databases   []fleetDatabase `json:"databases"`
			Unavailable []string        `json:"unavailable_server_ids"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatal(w.Code, w.Body)
		}
		return result
	}
	for i := 0; i < 3; i++ {
		result := call()
		keys := []string{}
		for _, d := range result.Databases {
			keys = append(keys, d.ServerID+"/"+d.AppID)
		}
		if strings.Join(keys, ",") != "a/beta,a/zeta,z/alpha,z/omega" {
			t.Fatal(keys)
		}
	}
	offline = true
	result := call()
	if len(result.Unavailable) != 1 || result.Unavailable[0] != "a" || len(result.Databases) != 2 {
		t.Fatal(result)
	}
}
