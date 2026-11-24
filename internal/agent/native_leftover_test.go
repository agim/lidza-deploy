package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// What an earlier attempt of the same application left on a server is
// cleared by the agent when the application is created again; the data of
// an application that was really removed is not.
func TestLeftoversOfAnEarlierAttemptAreClearedNotRefused(t *testing.T) {
	m, _ := newHostDataFixture(t)
	ctx := context.Background()
	set := func(change func(*diskState)) {
		m.mu.Lock()
		defer m.mu.Unlock()
		change(&m.data)
	}
	record := NativeHostDataRecord{Updated: time.Now().UTC()}
	set(func(d *diskState) { d.NativeHostData = map[string]NativeHostDataRecord{"shop": record} })

	// A creation that did not complete: provisioned, never an application, nothing retained.
	if kind, _ := m.leftoverNativeData("shop"); kind != "unfinished" {
		t.Fatalf("an unfinished creation: %q", kind)
	}
	if note, err := m.clearLeftoverNativeData(ctx, "shop"); err != nil || !strings.Contains(note, "never completed") {
		t.Fatalf("clearing an unfinished creation: %q %v", note, err)
	}
	if _, left := m.hostDataRecord("shop"); left {
		t.Fatal("the unfinished creation's record stayed")
	}
	if kind, _ := m.leftoverNativeData("shop"); kind != "" {
		t.Fatalf("nothing is left, but: %q", kind)
	}

	// With a backup on one of its databases it is not removed: it becomes a
	// retained resource the operator can see and remove.
	set(func(d *diskState) {
		d.NativeHostData["shop"] = NativeHostDataRecord{Postgres: &NativeHostPostgres{Name: "app_shop", Resource: "shop-db1"}}
		d.Databases = map[string]Database{"shop-db1": {AppID: "shop-db1", Backups: []BackupRecord{{ID: "b1"}}}}
	})
	if kind, _ := m.leftoverNativeData("shop"); kind != "unlisted" {
		t.Fatalf("an unfinished creation with a backup: %q", kind)
	}
	if note, err := m.clearLeftoverNativeData(ctx, "shop"); err != nil || note != "" {
		t.Fatalf("nothing may be removed here: %q %v", note, err)
	}
	m.mu.Lock()
	listed := 0
	for _, r := range m.data.RetainedResources {
		if r.AppID == "shop" && r.NativeData {
			listed++
		}
	}
	_, kept := m.data.NativeHostData["shop"]
	m.mu.Unlock()
	if listed != 1 || !kept {
		t.Fatalf("the data with a backup was not recorded as retained: %d %v", listed, kept)
	}
	set(func(d *diskState) { d.RetainedResources, d.Databases = nil, nil })

	// Listed as retained or not, with a backup or not: a database that holds
	// no table, or that no longer exists, is removed with its record.
	savedCount := moveTableCount
	t.Cleanup(func() { moveTableCount = savedCount })
	for name, tables := range map[string]int{"no tables": 0, "has tables": 4} {
		moveTableCount = func(context.Context, *Manager, string) (int, error) { return tables, nil }
		set(func(d *diskState) {
			d.NativeHostData["shop"] = NativeHostDataRecord{Postgres: &NativeHostPostgres{Name: "app_shop", Resource: "shop-db1", Port: 5432, Ready: true}}
			d.Databases = map[string]Database{"shop-db1": {AppID: "shop-db1", Ready: true, Backups: []BackupRecord{{ID: "b1"}}}}
			d.RetainedResources = map[string]RetainedResource{"kept": {ID: "kept", AppID: "shop", NativeData: true, Databases: []string{"shop-db1"}}}
		})
		if got, want := m.nativeDataEmpty("shop"), tables == 0; got != want {
			t.Fatalf("%s: empty = %v", name, got)
		}
		note, err := m.clearLeftoverNativeData(ctx, "shop")
		_, left := m.hostDataRecord("shop")
		if tables == 0 && (err != nil || left || !strings.Contains(note, "held no tables")) {
			t.Fatalf("%s: %q %v, record left: %v", name, note, err, left)
		}
		if tables != 0 && (err != nil || !left || note != "") {
			t.Fatalf("%s: data was removed: %q %v, record left: %v", name, note, err, left)
		}
	}
	set(func(d *diskState) { d.RetainedResources, d.Databases = nil, nil; delete(d.NativeHostData, "shop") })
	moveTableCount = savedCount

	// The data of an application that was removed stays the operator's.
	set(func(d *diskState) {
		d.NativeHostData["shop"] = record
		d.RetainedResources = map[string]RetainedResource{"shop-kept": {ID: "shop-kept", AppID: "shop", NativeData: true}}
	})
	if kind, _ := m.leftoverNativeData("shop"); kind != "" {
		t.Fatalf("a removed application's data was classified as a leftover: %q", kind)
	}
	if note, err := m.clearLeftoverNativeData(ctx, "shop"); err != nil || note != "" {
		t.Fatalf("nothing may be removed here: %q %v", note, err)
	}
	if _, left := m.hostDataRecord("shop"); !left {
		t.Fatal("a removed application's data was cleared")
	}
	// The copy of a move that never switched is a leftover, a switched one is not.
	set(func(d *diskState) {
		kept := d.RetainedResources["shop-kept"]
		kept.Origin = &RetainedOrigin{MoveID: "m1"}
		d.RetainedResources["shop-kept"] = kept
	})
	if kind, item := m.leftoverNativeData("shop"); kind != "move" || item != "shop-kept" {
		t.Fatalf("an unswitched move's copy: %q %q", kind, item)
	}
	set(func(d *diskState) {
		kept := d.RetainedResources["shop-kept"]
		kept.Origin = &RetainedOrigin{MoveID: "m1", Switched: true}
		d.RetainedResources["shop-kept"] = kept
	})
	if kind, _ := m.leftoverNativeData("shop"); kind != "" {
		t.Fatalf("a switched move's data was classified as a leftover: %q", kind)
	}
	// An application that exists owns its data, created by the agent or not.
	set(func(d *diskState) {
		d.RetainedResources = nil
		d.NativeApps = map[string]NativeApplication{"shop": {ID: "shop"}}
	})
	if kind, _ := m.leftoverNativeData("shop"); kind != "" {
		t.Fatalf("an existing application's data was classified as a leftover: %q", kind)
	}
}
