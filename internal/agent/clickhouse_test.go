package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func clickhouseManifest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lidza.json"), []byte(`{"name":"fixture","frontend":{"template":"htmx"},"deploy":{"env":{"CLICKHOUSE_MANAGED":"true"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestClickHouseAutomaticPersistenceAndRetirement(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("telemetry")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	m.clickhouseProvision = func(context.Context, ClickHouseResource) error { return nil }
	dir := clickhouseManifest(t)
	first, err := m.preflightRuntime(context.Background(), a, dir)
	if err != nil {
		t.Fatal(err)
	}
	c := m.data.ClickHouses[a.ID]
	if !c.Ready || len(c.Password) != 64 || first.Env["CLICKHOUSE_PASSWORD"] != c.Password || !slices.Contains(first.Networks, c.Network) {
		t.Fatal("missing private service attachment")
	}
	state, err := os.ReadFile(m.path())
	if err != nil || strings.Contains(string(state), c.Password) {
		t.Fatal("state is not encrypted")
	}
	settings, _ := m.Settings(a.ID)
	view, _ := json.Marshal(settings)
	if strings.Contains(string(view), c.Password) {
		t.Fatal("settings leaked password")
	}
	restarted, err := NewManager(context.Background(), m.cfg, &fakeRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.clickhouseProvision = m.clickhouseProvision
	second, err := restarted.preflightRuntime(context.Background(), restarted.data.Apps[a.ID], dir)
	if err != nil || second.Env["CLICKHOUSE_PASSWORD"] != c.Password || restarted.data.ClickHouses[a.ID] != c {
		t.Fatal("restart rotated credentials", err)
	}
	if err := m.PatchSettings(a.ID, SettingsPatch{Branch: a.Branch, Domain: a.Domain, EnvChanges: map[string]*string{"CLICKHOUSE_PASSWORD": nil}}); err == nil {
		t.Fatal("managed secret was deleted")
	}
	if err := m.Upsert(testApp(a.ID)); err != nil {
		t.Fatal(err)
	}
	if m.data.Apps[a.ID].Env["CLICKHOUSE_PASSWORD"] != c.Password {
		t.Fatal("upsert lost secret")
	}
	if err := m.Retire(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	if m.data.ClickHouses[a.ID].Network != "" || len(m.retainedViews()) != 1 || m.retainedViews()[0].ClickHouseVolume != c.Network+"-data" {
		t.Fatal("retirement lost retained data")
	}
	if err := m.Upsert(testApp(a.ID)); err != nil {
		t.Fatal(err)
	}
	third, err := m.preflightRuntime(context.Background(), m.data.Apps[a.ID], dir)
	if err != nil || third.Env["CLICKHOUSE_PASSWORD"] == c.Password || third.Env["CLICKHOUSE_URL"] == first.Env["CLICKHOUSE_URL"] {
		t.Fatal("recreated app inherited retired data", err)
	}
}

func TestClickHouseFailureRetryAndExternalConnection(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	a := testApp("retry")
	if err := m.Upsert(a); err != nil {
		t.Fatal(err)
	}
	dir := clickhouseManifest(t)
	m.clickhouseProvision = func(context.Context, ClickHouseResource) error { return errors.New("secret upstream diagnostic") }
	_, err := m.preflightRuntime(context.Background(), a, dir)
	if err == nil || strings.Contains(err.Error(), "secret upstream") {
		t.Fatal("failure was hidden or leaked diagnostic")
	}
	reserved := m.data.ClickHouses[a.ID]
	if reserved.Password == "" || reserved.Ready || m.data.Apps[a.ID].Env["CLICKHOUSE_URL"] != "" {
		t.Fatal("failed provision attached connection or lost identity")
	}
	m.clickhouseProvision = func(_ context.Context, c ClickHouseResource) error {
		if c.Password != reserved.Password {
			t.Fatal("retry rotated secret")
		}
		return nil
	}
	if _, err := m.preflightRuntime(context.Background(), a, dir); err != nil {
		t.Fatal(err)
	}
	external := testApp("external")
	external.Env["CLICKHOUSE_URL"] = "https://telemetry.example.com"
	external.Env["CLICKHOUSE_PASSWORD"] = "external-private-secret"
	if err := m.Upsert(external); err != nil {
		t.Fatal(err)
	}
	m.clickhouseProvision = func(context.Context, ClickHouseResource) error { t.Fatal("external connection replaced"); return nil }
	prepared, err := m.preflightRuntime(context.Background(), external, dir)
	if err != nil || prepared.Env["CLICKHOUSE_PASSWORD"] != external.Env["CLICKHOUSE_PASSWORD"] || m.data.ClickHouses[external.ID].Network != "" {
		t.Fatal("external settings lost", err)
	}
	partial := testApp("partial")
	partial.Env["CLICKHOUSE_DATABASE"] = "existing"
	if err := m.Upsert(partial); err != nil {
		t.Fatal(err)
	}
	if _, err := m.preflightRuntime(context.Background(), partial, dir); err == nil {
		t.Fatal("partial connection replaced")
	}
}

func TestClickHouseUnavailableClassificationExcludesAuthentication(t *testing.T) {
	for _, message := range []string{"context deadline exceeded", "Cannot connect to 127.0.0.1:9000", "NETWORK_ERROR: Connection refused"} {
		if !clickhouseUnavailable(errors.New(message)) {
			t.Fatalf("availability failure not recognized: %s", message)
		}
	}
	if clickhouseUnavailable(errors.New("AUTHENTICATION_FAILED: wrong password")) {
		t.Fatal("credential failure would replace retained container")
	}
}
