package control

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestApplicationAPIReturnsStableOrder(t *testing.T) {
	c := &Control{data: saved{Apps: map[string]Application{}}}
	for _, id := range []string{"portal", "agim-dev", "third"} {
		c.data.Apps[id] = Application{ID: id}
	}
	for i := 0; i < 30; i++ {
		w := httptest.NewRecorder()
		c.apps(w, httptest.NewRequest("GET", "/api/control/apps", nil))
		var list []Application
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		for j, id := range []string{"agim-dev", "portal", "third"} {
			if list[j].ID != id {
				t.Fatalf("poll %d: order changed: %#v", i, list)
			}
		}
	}
}
