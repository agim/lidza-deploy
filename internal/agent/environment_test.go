package agent

import (
	"strings"
	"testing"
)

func TestEnvironmentAddressValuesRequireAbsoluteURLs(t *testing.T) {
	for _, test := range []struct {
		key   string
		value string
		valid bool
	}{
		{key: "LLM_BASE_URL", value: "https://api.example.com/v1", valid: true},
		{key: "DATABASE_URL", value: "postgres://user:password@database.example.com/app", valid: true},
		{key: "CACHE_URL", value: "rediss://:password@cache.example.com:6379/0", valid: true},
		{key: "LLM_BASE_URL", value: "operator@example.com", valid: false},
		{key: "APP_URL", value: "example.com", valid: false},
		{key: "SUPPORT_EMAIL", value: "operator@example.com", valid: true},
	} {
		t.Run(test.key+"/"+test.value, func(t *testing.T) {
			a := testApp("address-test")
			a.Env[test.key] = test.value
			err := a.Validate()
			if test.valid && err != nil {
				t.Fatal(err)
			}
			if !test.valid && (err == nil || !strings.Contains(err.Error(), test.key)) {
				t.Fatalf("expected a field-specific URL error, got %v", err)
			}
		})
	}
}

func TestInvalidAddressPatchDoesNotMutateSavedEnvironment(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("address-patch")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	bad := "operator@example.com"
	err := m.PatchSettings(a.ID, SettingsPatch{Branch: a.Branch, Domain: a.Domain, EnvChanges: map[string]*string{"LLM_BASE_URL": &bad}})
	if err == nil {
		t.Fatal("accepted an email address as LLM_BASE_URL")
	}
	m.mu.Lock()
	_, saved := m.data.Apps[a.ID].Env["LLM_BASE_URL"]
	m.mu.Unlock()
	if saved {
		t.Fatal("rejected address was persisted")
	}
}
