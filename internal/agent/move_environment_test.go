package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// stubInspectEnvironment answers docker inspect for one container and one
// image; anything else is unknown.
func stubInspectEnvironment(t *testing.T, container, image string, containerEnv, imageEnv []string) {
	t.Helper()
	old := moveInspectEnvironment
	t.Cleanup(func() { moveInspectEnvironment = old })
	moveInspectEnvironment = func(_ context.Context, kind, name string) (map[string]string, error) {
		switch {
		case kind == "container" && name == container && containerEnv != nil:
			return parseEnvironmentEntries(containerEnv), nil
		case kind == "image" && name == image && imageEnv != nil:
			return parseEnvironmentEntries(imageEnv), nil
		}
		return nil, errors.New("No such object: " + name)
	}
}

var (
	testImageEnv = []string{"PATH=/usr/local/bin:/usr/bin", "LIDZA_MODE=production", "LANG=C.UTF-8", "GOSU_VERSION=1.17", "TZ=UTC", "FEATURE_FLAGS=default", "GOLANG_VERSION=1.25"}
	// The running container: image ENV under the env file (settings and
	// manifest defaults) and the agent's own --env flags.
	testContainerEnv = append(slices.Clone(testImageEnv), "HOME=/root", "HOSTNAME=4f2a", "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt", "LIDZA_ADDR=0.0.0.0:3000", "PORT=3000",
		"APP_SECRET=runtime-secret", "DB_POOL=20", "APP_URL=https://shop.example.com", "FEATURE_FLAGS=a,b", "DATABASE_URL=postgres://app:pw@lidza-db-shop/app", "CACHE_URL=redis://:pw@cache:6379",
		"CLICKHOUSE_URL=http://ch:8123", "STORAGE_DIR=/data", "DEPLOY_GATEWAY_KEY=gateway", "AUTH_OWNER_CLAIM_DIR=/run/claim")
)

func TestContainerEffectiveEnvironmentNamesSourcesAndDropsRuntimeKeys(t *testing.T) {
	stubInspectEnvironment(t, "lidza-shop-r1", "ghcr.io/acme/shop:abc", testContainerEnv, testImageEnv)
	m := testManager(t, &fakeRuntime{})
	a := testApp("shop")
	a.Current = &Release{ID: "r1", Container: "lidza-shop-r1", Image: "ghcr.io/acme/shop:abc"}
	env := m.containerEffectiveEnvironment(context.Background(), a)
	want := map[string][2]string{
		"APP_SECRET":    {"runtime-secret", EnvironmentSourcePanel},
		"DB_POOL":       {"20", EnvironmentSourceDefaults},
		"APP_URL":       {"https://shop.example.com", EnvironmentSourceDefaults},
		"FEATURE_FLAGS": {"a,b", EnvironmentSourceDefaults}, // the deploy overrode the image's value
		"LIDZA_MODE":    {"production", EnvironmentSourceImage},
		"TZ":            {"UTC", EnvironmentSourceImage},
	}
	for key, expected := range want {
		if env.Values[key] != expected[0] || env.Sources[key] != expected[1] {
			t.Fatalf("%s = %q (%s), want %q (%s)", key, env.Values[key], env.Sources[key], expected[0], expected[1])
		}
	}
	if len(env.Values) != len(want) {
		t.Fatalf("exported %v", sortedKeys(env.Values))
	}
	for _, excluded := range []string{"PATH", "HOME", "HOSTNAME", "SSL_CERT_FILE", "LANG", "GOSU_VERSION", "GOLANG_VERSION", "LIDZA_ADDR", "PORT", "DATABASE_URL", "CACHE_URL", "CLICKHOUSE_URL", "STORAGE_DIR", "DEPLOY_GATEWAY_KEY", "AUTH_OWNER_CLAIM_DIR"} {
		if _, present := env.Values[excluded]; present {
			t.Fatalf("%s is owned by the host or the destination", excluded)
		}
	}
	// A setting the application itself makes is kept even when the key is
	// otherwise host-owned, and a setting newer than the container counts.
	a.Env["LANG"] = "de_DE.UTF-8"
	a.Env["NEW_SETTING"] = "fresh"
	env = m.containerEffectiveEnvironment(context.Background(), a)
	if env.Values["LANG"] != "de_DE.UTF-8" || env.Sources["LANG"] != EnvironmentSourcePanel || env.Values["NEW_SETTING"] != "fresh" || env.Sources["NEW_SETTING"] != EnvironmentSourcePanel {
		t.Fatalf("application settings: %v %v", env.Values, env.Sources)
	}
}

func TestContainerEffectiveEnvironmentWithoutContainerUsesImageAndRuntimeDefaults(t *testing.T) {
	stubInspectEnvironment(t, "", "ghcr.io/acme/shop:abc", nil, testImageEnv)
	m := testManager(t, &fakeRuntime{})
	a := testApp("shop")
	a.Current = &Release{ID: "r1", Container: "lidza-shop-gone", Image: "ghcr.io/acme/shop:abc"}
	env := m.containerEffectiveEnvironment(context.Background(), a)
	if env.Values["LIDZA_MODE"] != "production" || env.Sources["LIDZA_MODE"] != EnvironmentSourceImage {
		t.Fatalf("image variable: %v %v", env.Values, env.Sources)
	}
	if env.Values["APP_URL"] != "https://shop.example.com" || env.Sources["APP_URL"] != EnvironmentSourceDefaults {
		t.Fatalf("agent default: %v %v", env.Values, env.Sources)
	}
	if env.Values["APP_SECRET"] != "runtime-secret" || env.Sources["APP_SECRET"] != EnvironmentSourcePanel {
		t.Fatalf("stored setting: %v %v", env.Values, env.Sources)
	}
	for _, excluded := range []string{"LIDZA_ADDR", "PATH", "LANG", "GOSU_VERSION"} {
		if _, present := env.Values[excluded]; present {
			t.Fatalf("%s exported", excluded)
		}
	}
	// Without any release: the stored environment and the agent's runtime
	// defaults, no image variables.
	a.Current = nil
	env = m.containerEffectiveEnvironment(context.Background(), a)
	if len(env.Values) != 3 || env.Values["APP_SECRET"] != "runtime-secret" || env.Sources["LIDZA_MODE"] != EnvironmentSourceDefaults || env.Sources["APP_URL"] != EnvironmentSourceDefaults {
		t.Fatalf("undeployed application: %v %v", env.Values, env.Sources)
	}
}

func TestMoveExportsTheEffectiveContainerEnvironment(t *testing.T) {
	stubInspectEnvironment(t, "lidza-shop-r1", "ghcr.io/acme/shop:abc", testContainerEnv, testImageEnv)
	m := testManager(t, &fakeRuntime{})
	if err := m.Upsert(testApp("shop")); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a := m.data.Apps["shop"]
	a.Current = &Release{ID: "r1", Commit: strings.Repeat("a", 40), Container: "lidza-shop-r1", Image: "ghcr.io/acme/shop:abc"}
	m.data.Apps["shop"] = a
	m.mu.Unlock()
	ctx := context.Background()
	env, err := m.moveEnvironment(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if env.Values["LIDZA_MODE"] != "production" || env.Sources["LIDZA_MODE"] != EnvironmentSourceImage || env.Values["DB_POOL"] != "20" || env.Sources["DB_POOL"] != EnvironmentSourceDefaults || env.Sources["APP_SECRET"] != EnvironmentSourcePanel {
		t.Fatalf("export: %v %v", env.Values, env.Sources)
	}
	if env.Kinds["APP_SECRET"] != EnvironmentCredential || env.Kinds["DB_POOL"] != EnvironmentSetting {
		t.Fatalf("kinds: %v", env.Kinds)
	}
	for _, excluded := range []string{"PATH", "DATABASE_URL", "CACHE_URL", "LIDZA_ADDR", "DEPLOY_GATEWAY_KEY"} {
		if _, present := env.Values[excluded]; present {
			t.Fatalf("%s exported", excluded)
		}
	}
	source, err := m.moveSource(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(source.EnvironmentKeys, "LIDZA_MODE") || !slices.Contains(source.EnvironmentKeys, "DB_POOL") || slices.Contains(source.EnvironmentKeys, "PATH") {
		t.Fatalf("plan keys: %v", source.EnvironmentKeys)
	}
	if source.EnvironmentSources["LIDZA_MODE"] != EnvironmentSourceImage || source.EnvironmentSources["APP_SECRET"] != EnvironmentSourcePanel {
		t.Fatalf("plan sources: %v", source.EnvironmentSources)
	}
	if raw := mustJSON(source); strings.Contains(string(raw), "runtime-secret") {
		t.Fatal("the plan carries a value")
	}
}
