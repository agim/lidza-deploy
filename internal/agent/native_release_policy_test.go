package agent

import "testing"

func TestNativeReleasePolicyToolchainChoice(t *testing.T) {
	for _, c := range []struct {
		policy  NativeReleasePolicy
		managed bool
	}{
		{NativeReleasePolicy{Runtime: "rails"}, false},
		{NativeReleasePolicy{Runtime: "go"}, true},
		{NativeReleasePolicy{Runtime: "rails", User: "app-shop"}, true},
		{NativeReleasePolicy{Runtime: "rails", User: "app-shop", Toolchains: "account"}, false},
		{NativeReleasePolicy{Runtime: "rails", Toolchains: "managed"}, true},
	} {
		if got := c.policy.ManagedToolchains(); got != c.managed {
			t.Errorf("%+v: managed toolchains %v, want %v", c.policy, got, c.managed)
		}
	}
	p := NativeReleasePolicy{Enabled: true, Root: "/home/deploy/shop", Repository: "acme/shop", Branch: "main", Runtime: "rails", Units: []string{"shop-web.service"}, Toolchains: "rbenv"}
	if err := p.Validate(); err == nil {
		t.Fatal("unknown toolchain choice accepted")
	}
	p.Toolchains = "account"
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}
