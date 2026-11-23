package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/plugin"
)

// handOverPlugin stands in for the ClickHouse plugin: it answers the
// connection it is handed, or a new one for a new application, and backs up.
const handOverPlugin = `#!/bin/sh
request=$(cat)
work=$(printf '%s' "$request" | sed -n 's/.*"work_dir":"\([^"]*\)".*/\1/p')
app=$(printf '%s' "$request" | sed -n 's/.*"app":"\([a-z0-9-]*\)".*/\1/p')
case "$1" in
 describe|release|restore) printf '{"ok":true}';;
 check) printf '{"ok":true,"checks":[{"id":"server","title":"ClickHouse server","ok":true,"detail":"running"}]}';;
 backup) echo "rows of $app" > "$work/data"; printf '{"ok":true}';;
 provision)
  db=$(printf '%s' "$request" | sed -n 's/.*"database":"\([a-z_]*\)".*/\1/p')
  port=$(printf '%s' "$request" | sed -n 's/.*"http_port":\([0-9]*\).*/\1/p')
  pw=$(printf '%s' "$request" | sed -n 's/.*"password":"\([^"]*\)".*/\1/p')
  if printf '%s' "$request" | grep -q '"adopt"'; then echo "adopt $db" >> calls; else echo "new $app" >> calls; fi
  [ -n "$db" ] || db="app_$app"
  [ -n "$port" ] || port=8123
  [ -n "$pw" ] || pw="generated-for-$app"
  [ ! -e changeport ] || port=9999
  printf '{"ok":true,"output":{"env":{"CLICKHOUSE_URL":"http://127.0.0.1:%s","CLICKHOUSE_DATABASE":"%s","CLICKHOUSE_USER":"%s","CLICKHOUSE_PASSWORD":"%s"},"record":{"database":"%s","http_port":%s},"secrets":{"password":"%s"}}}' "$port" "$db" "$db" "$pw" "$db" "$port" "$pw";;
esac
`

func installHandOverPlugin(t *testing.T, m *Manager, hooks []string) {
	t.Helper()
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	if err := m.setPluginKeys([]string{hex.EncodeToString(public)}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: plugin.Program, Mode: 0o755, Size: int64(len(handOverPlugin)), Typeflag: tar.TypeReg})
	tw.Write([]byte(handOverPlugin))
	tw.Close()
	gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	raw, _ := json.Marshal(plugin.Manifest{Contract: plugin.Contract, Name: "clickhouse", Version: "0.5", Provides: "clickhouse", Hooks: hooks,
		Environment: []string{"CLICKHOUSE_URL", "CLICKHOUSE_DATABASE", "CLICKHOUSE_USER", "CLICKHOUSE_PASSWORD"},
		Archives:    []plugin.Archive{{Arch: runtime.GOARCH, SHA256: hex.EncodeToString(sum[:]), Size: int64(buf.Len())}}})
	signature, _ := json.Marshal(plugin.Sign(private, raw))
	if _, err := m.stagePluginManifest(string(raw), signature); err != nil {
		t.Fatal(err)
	}
	if _, err := m.installPlugin(context.Background(), "clickhouse", bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
}

// The ClickHouse the built-in support runs is handed to the plugin without
// the application noticing: same connection, same resource and backups,
// nothing installed, written or restarted. Afterwards ClickHouse on the
// server is the plugin's.
func TestTheBuiltInClickHouseIsHandedToThePluginWithoutTouchingIt(t *testing.T) {
	m, f := newClickHouseHostFixture(t, nativeCHKeyFingerprint)
	account := NativeAppAccount{User: "app-shop", UID: 1001, GID: 1001, Root: "/srv/apps/shop"}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	short, stop := context.WithTimeout(ctx, 2*time.Second)
	_, _ = provisionNativeData(short, m, "shop", account, NativeDataRequest{ClickHouse: true, ClickHouseVersion: "26.8"})
	stop()
	f.learnAdmin(t, m)
	grant, err := provisionNativeData(ctx, m, "shop", account, NativeDataRequest{ClickHouse: true, ClickHouseVersion: "26.8"})
	if err != nil || len(grant.Databases) != 1 {
		t.Fatalf("built-in provisioning: %v %+v", err, grant)
	}
	resource := grant.Databases[0]
	m.mu.Lock()
	if m.data.NativeCreatedSpecs == nil {
		m.data.NativeCreatedSpecs = map[string]NativeCreateRequest{}
	}
	m.data.NativeCreatedSpecs["shop"] = NativeCreateRequest{ID: "shop", Data: NativeDataRequest{ClickHouse: true, ClickHouseVersion: "26.8"}}
	m.mu.Unlock()

	// Without the plugin there is nothing to hand it to.
	if plan := m.clickhouseHandOverPlan(); !plan.BuiltIn || plan.Available || !strings.Contains(plan.Reason, "Install the ClickHouse plugin") || len(plan.Applications) != 1 || plan.Applications[0].Database != "app_shop" {
		t.Fatalf("plan without the plugin: %+v", plan)
	}
	if _, err := m.handOverClickHouse(ctx); err == nil {
		t.Fatal("a hand-over ran without the plugin")
	}
	// A plugin that only checks cannot take it.
	installHandOverPlugin(t, m, []string{"describe", "check"})
	if plan := m.clickhouseHandOverPlan(); plan.Available || !strings.Contains(plan.Reason, "cannot run a ClickHouse server") {
		t.Fatalf("plan with a checking-only plugin: %+v", plan)
	}
	installHandOverPlugin(t, m, []string{"describe", "check", "provision", "release", "backup", "restore"})
	// While the built-in support runs the server, ClickHouse is not the plugin's.
	if m.clickhouseViaPlugin() {
		t.Fatal("the plugin owns ClickHouse before the hand-over")
	}
	if plan := m.clickhouseHandOverPlan(); !plan.Available || plan.Plugin != "clickhouse" || plan.PluginVersion != "0.5" {
		t.Fatalf("plan: %+v", plan)
	}
	state := m.pluginState("clickhouse")
	// A plugin that would change the application's connection changes nothing.
	os.WriteFile(filepath.Join(state, "changeport"), nil, 0o600)
	if _, err := m.handOverClickHouse(ctx); err == nil || !strings.Contains(err.Error(), "would change the connection of shop; nothing was changed") {
		t.Fatalf("a changed connection: %v", err)
	}
	os.Remove(filepath.Join(state, "changeport"))
	if record, _ := m.hostDataRecord("shop"); record.ClickHouse == nil || len(record.Plugins) != 0 {
		t.Fatalf("a refused hand-over changed the records: %+v", record)
	}

	f.mu.Lock()
	before := len(f.commands)
	f.mu.Unlock()
	done, err := m.handOverClickHouse(ctx)
	if err != nil || len(done.Done) != 2 || !strings.Contains(done.Done[0], "its connection is unchanged") {
		t.Fatalf("hand-over: %v %+v", err, done)
	}
	record, _ := m.hostDataRecord("shop")
	entry := record.Plugins["clickhouse"]
	if record.ClickHouse != nil || entry.Plugin != "clickhouse" || entry.Resource != resource || !strings.Contains(string(entry.Record), `"database":"app_shop"`) || strings.Contains(string(entry.Record), "adopt") {
		t.Fatalf("record after the hand-over: %+v", record)
	}
	if secrets, err := m.pluginSecrets(entry.Secrets); err != nil || secrets["password"] != grant.Env["CLICKHOUSE_PASSWORD"] || len(secrets) != 1 {
		t.Fatalf("secrets after the hand-over: %v %v", secrets, err)
	}
	m.mu.Lock()
	policy, d, spec := m.data.NativeDatabases[resource].Policy, m.data.Databases[resource], m.data.NativeCreatedSpecs["shop"]
	server, inUse := m.data.NativeClickHouse, m.nativeDatabaseInUseLocked(resource)
	m.mu.Unlock()
	if policy.PluginService != "clickhouse" || policy.ClickHouseDatabase != "" || policy.AppID != "shop" || !inUse || server != nil || strings.Contains(d.URL, "@") || !d.Ready {
		t.Fatalf("resource after the hand-over: %+v %+v server %v in use %v", policy, d, server, inUse)
	}
	if spec.Data.ClickHouse || len(spec.Data.Plugins) != 1 || spec.Data.Plugins[0].Service != "clickhouse" || string(spec.Data.Plugins[0].Options) != `{"series":"26.8"}` {
		t.Fatalf("the recorded request after the hand-over: %+v", spec.Data)
	}
	// Nothing was installed, written or restarted on the server.
	f.mu.Lock()
	for _, command := range f.commands[before:] {
		if strings.Contains(command, "apt-get") || strings.Contains(command, "systemctl") {
			t.Fatalf("the hand-over ran %s", command)
		}
	}
	f.mu.Unlock()
	if raw, _ := os.ReadFile(filepath.Join(state, "calls")); string(raw) != "adopt app_shop\nadopt app_shop\n" {
		t.Fatalf("what the plugin was asked: %q", raw)
	}
	// The first backup through the plugin follows.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		m.mu.Lock()
		d = m.data.Databases[resource]
		m.mu.Unlock()
		if d.Operation == "" && len(d.Backups) > 0 {
			break
		}
	}
	if d.Operation != "" || len(d.Backups) == 0 || d.Error != "" {
		t.Fatalf("the first plugin backup: %+v", d)
	}
	// From here on ClickHouse is the plugin's: nothing is left to hand
	// over, and a new application's ClickHouse comes from the plugin.
	if plan := m.clickhouseHandOverPlan(); plan.BuiltIn || plan.Available || !m.clickhouseViaPlugin() {
		t.Fatalf("plan after the hand-over: %+v", plan)
	}
	grant, err = provisionNativeData(ctx, m, "blog", NativeAppAccount{User: "app-blog", UID: 1002, GID: 1002, Root: "/srv/apps/blog"}, NativeDataRequest{ClickHouse: true})
	if err != nil || grant.Env["CLICKHOUSE_DATABASE"] != "app_blog" || grant.Env["CLICKHOUSE_PASSWORD"] != "generated-for-blog" {
		t.Fatalf("a new application's ClickHouse: %v %+v", err, grant.Env)
	}
	if blog, _ := m.hostDataRecord("blog"); blog.ClickHouse != nil || blog.Plugins["clickhouse"].Plugin != "clickhouse" {
		t.Fatalf("the new application's record: %+v", blog)
	}
}
