package agent

import "testing"

func TestAppsHaveStableOrder(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	for _, id := range []string{"portal", "agim-dev", "third"} {
		if err := m.Upsert(testApp(id)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 30; i++ {
		list := m.Apps()
		for j, id := range []string{"agim-dev", "portal", "third"} {
			if list[j].ID != id {
				t.Fatalf("poll %d: order changed: %#v", i, list)
			}
		}
	}
}
