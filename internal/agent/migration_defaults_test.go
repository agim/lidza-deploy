package agent

import "testing"

func TestNewAppMigrationPolicy(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	for _, value := range []string{"", "false", "true"} {
		a := testApp("migrate" + map[string]string{"": "-default", "false": "-off", "true": "-on"}[value])
		if value != "" {
			a.Env["DB_MIGRATE"] = value
		}
		if err := m.Upsert(a); err != nil {
			t.Fatal(err)
		}
		want := value
		if want == "" {
			want = "true"
		}
		if got := m.data.Apps[a.ID].Env["DB_MIGRATE"]; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
		// A full update omitting the policy must retain the saved choice.
		if err := m.Upsert(testApp(a.ID)); err != nil {
			t.Fatal(err)
		}
		if got := m.data.Apps[a.ID].Env["DB_MIGRATE"]; got != want {
			t.Fatalf("update changed policy to %q", got)
		}
	}
	legacy := testApp("legacy")
	m.data.Apps[legacy.ID] = legacy
	if err := m.Upsert(legacy); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.data.Apps[legacy.ID].Env["DB_MIGRATE"]; ok {
		t.Fatal("existing app silently opted into migrations")
	}
}
