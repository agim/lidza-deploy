package retention

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func at(days int, hour int) time.Time {
	return time.Date(2026, 11, 5, hour, 0, 0, 0, time.UTC).AddDate(0, 0, -days)
}

func object(id string, created time.Time) Object {
	return Object{Key: "databases/app/" + id + ".dump", Size: 100, Created: created}
}

func keys(entries []Entry) []string {
	out := []string{}
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(e.Key, "databases/app/"), ".dump"))
	}
	slices.Sort(out)
	return out
}

func reasonOf(t *testing.T, plan DatabasePlan, id string) string {
	t.Helper()
	for _, e := range append(append([]Entry{}, plan.Keep...), plan.Delete...) {
		if e.Key == "databases/app/"+id+".dump" {
			return e.Reason
		}
	}
	t.Fatalf("%s not planned", id)
	return ""
}

func expect(t *testing.T, plan DatabasePlan, keep, remove []string) {
	t.Helper()
	slices.Sort(keep)
	slices.Sort(remove)
	if got := keys(plan.Keep); !slices.Equal(got, keep) {
		t.Errorf("keep = %v, want %v", got, keep)
	}
	if got := keys(plan.Delete); !slices.Equal(got, remove) {
		t.Errorf("delete = %v, want %v", got, remove)
	}
}

func dailyHistory(days int) []Object {
	var out []Object
	for d := 0; d < days; d++ {
		out = append(out, object(fmt.Sprintf("d%03d", d), at(d, 3)))
	}
	return out
}

func TestDefaultsAndValidation(t *testing.T) {
	if p := Default(); p != (Policy{7, 4, 0}) {
		t.Fatal(p)
	}
	s := DefaultSettings()
	if s.OrphanGraceDays != 30 || !s.Automatic || s.Validate() != nil {
		t.Fatal(s)
	}
	for _, bad := range []Policy{{-1, 0, 0}, {366, 0, 0}, {0, 261, 0}, {0, 0, 121}, {0, -1, 0}} {
		if bad.Validate() == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	for _, good := range []Policy{{0, 0, 0}, {365, 260, 120}, {1, 0, 12}} {
		if good.Validate() != nil {
			t.Errorf("%+v refused", good)
		}
	}
	for _, grace := range []int{0, -1, 366} {
		if (Settings{Policy: Default(), OrphanGraceDays: grace}).Validate() == nil {
			t.Errorf("grace %d accepted", grace)
		}
	}
}

func TestParseKey(t *testing.T) {
	for key, want := range map[string]bool{
		"databases/app/abc.dump":           true,
		"databases/app/predeployment.dump": true,
		"databases/app/abc.sqlite3":        false,
		"databases/app/../x.dump":          false,
		"databases/App/abc.dump":           false,
		"databases/app/sub/abc.dump":       false,
		"connection-check/abc":             false,
		"databases/app/.dump":              false,
		"other/app/abc.dump":               false,
	} {
		if _, _, ok := ParseKey(key); ok != want {
			t.Errorf("%s: %v", key, ok)
		}
	}
}

func TestSevenDailyFourWeekly(t *testing.T) {
	// Sixty days of one backup a day at 03:00 UTC.
	objects := dailyHistory(60)
	plan := Select("app", objects, Default(), now, nil)
	kept := map[string]bool{}
	for _, e := range plan.Keep {
		kept[e.Key] = true
	}
	// Seven dailies (today .. six days ago) plus the newest backup of four
	// ISO weeks: this week's is today's and last week's (Sunday 11-01) is
	// already a daily, so two more weeklies are added.
	if len(plan.Keep) != 7+2 {
		t.Fatalf("kept %d: %v", len(plan.Keep), keys(plan.Keep))
	}
	if len(plan.Delete) != 60-9 || plan.DeleteBytes != int64(51*100) || plan.KeepBytes != 900 {
		t.Fatalf("delete %d bytes %d keep bytes %d", len(plan.Delete), plan.DeleteBytes, plan.KeepBytes)
	}
	for d := 0; d < 7; d++ {
		if !kept[objects[d].Key] {
			t.Errorf("day %d not kept", d)
		}
	}
	// 2026-11-05 is a Thursday: older weeks end on Sundays 11-01, 10-25, 10-18.
	for _, day := range []int{4, 11, 18} {
		if !kept[objects[day].Key] {
			t.Errorf("weekly from %s not kept", at(day, 3).Format("2006-01-02"))
		}
	}
	if r := reasonOf(t, plan, "d000"); !strings.Contains(r, "newest backup") || !strings.Contains(r, "daily 2026-11-05") || !strings.Contains(r, "weekly 2026-W45") {
		t.Fatal(r)
	}
}

func TestMonthly(t *testing.T) {
	objects := dailyHistory(120)
	plan := Select("app", objects, Policy{Daily: 0, Weekly: 0, Monthly: 3}, now, nil)
	// November, October and September each keep their newest backup.
	var want []string
	for _, day := range []int{0, 5, 36} {
		want = append(want, strings.TrimSuffix(strings.TrimPrefix(objects[day].Key, "databases/app/"), ".dump"))
	}
	if got := keys(plan.Keep); !slices.Equal(got, sorted(want)) {
		t.Fatalf("keep %v want %v (%s %s)", got, want, at(5, 3), at(36, 3))
	}
}

func sorted(v []string) []string { slices.Sort(v); return v }

func TestGapsDoNotUseUpCounts(t *testing.T) {
	// Backups stopped 40 days ago: the last seven backup days are still kept.
	var objects []Object
	for d := 40; d < 50; d++ {
		objects = append(objects, object("old"+string(rune('a'+d-40)), at(d, 3)))
	}
	plan := Select("app", objects, Policy{Daily: 7}, now, nil)
	expect(t, plan, []string{"olda", "oldb", "oldc", "oldd", "olde", "oldf", "oldg"}, []string{"oldh", "oldi", "oldj"})
}

func TestSameDayMultiplesKeepNewest(t *testing.T) {
	objects := []Object{
		object("today-1", at(0, 1)), object("today-2", at(0, 2)), object("today-3", at(0, 11)),
		object("y-1", at(1, 1)), object("y-2", at(1, 22)),
		object("old-1", at(3, 1)), object("old-2", at(3, 5)),
	}
	plan := Select("app", objects, Policy{Daily: 7}, now, nil)
	// Today's earlier backups are younger than 24h but superseded the same
	// day. Yesterday's 22:00 is newest of its day; 01:00 is superseded.
	expect(t, plan, []string{"today-3", "y-2", "old-2"}, []string{"today-1", "today-2", "y-1", "old-1"})
}

func TestRecentObjectsNeverDeletedUnlessSuperseded(t *testing.T) {
	// No daily or weekly retention at all: the newest and anything younger
	// than 24 hours that is the newest of its day still stay.
	objects := []Object{object("now", at(0, 11)), object("earlier-today", at(0, 2)), object("last-night", at(1, 23)), object("old", at(2, 1))}
	plan := Select("app", objects, Policy{}, now, nil)
	expect(t, plan, []string{"now", "last-night"}, []string{"earlier-today", "old"})
	if r := reasonOf(t, plan, "last-night"); r != "less than 24 hours old" {
		t.Fatal(r)
	}
}

func TestNewestSuccessfulAlwaysKept(t *testing.T) {
	objects := []Object{{Key: "databases/app/empty.dump", Size: 0, Created: at(5, 1)}, object("good", at(30, 1)), object("older", at(31, 1))}
	plan := Select("app", objects, Policy{}, now, nil)
	expect(t, plan, []string{"good"}, []string{"empty", "older"})
	if plan := Select("app", nil, Default(), now, nil); len(plan.Keep)+len(plan.Delete) != 0 {
		t.Fatal(plan)
	}
}

func TestPredeploymentAndForeignKeysUntouched(t *testing.T) {
	objects := []Object{
		{Key: "databases/app/predeployment.dump", Size: 9, Created: at(90, 1)},
		object("a", at(0, 1)), object("b", at(40, 1)),
		{Key: "databases/other/x.dump", Size: 5, Created: at(90, 1)},
		{Key: "databases/app/notes.txt", Size: 5, Created: at(90, 1)},
		{Key: "connection-check/abc", Size: 5, Created: at(90, 1)},
	}
	plan := Select("app", objects, Policy{Daily: 1}, now, nil)
	expect(t, plan, []string{"predeployment", "a"}, []string{"b"})
	if r := reasonOf(t, plan, "predeployment"); r != "pre-deployment copy" {
		t.Fatal(r)
	}
	// Predeployment does not count as the newest backup either.
	objects = []Object{{Key: "databases/app/predeployment.dump", Size: 9, Created: at(0, 1)}, object("a", at(50, 1)), object("b", at(51, 1))}
	expect(t, Select("app", objects, Policy{}, now, nil), []string{"predeployment", "a"}, []string{"b"})
}

func TestProtectedKeys(t *testing.T) {
	objects := []Object{object("new", at(0, 1)), object("moving", at(60, 1)), object("old", at(61, 1))}
	plan := Select("app", objects, Policy{Daily: 1}, now, map[string]bool{"databases/app/moving.dump": true})
	expect(t, plan, []string{"new", "moving"}, []string{"old"})
	if r := reasonOf(t, plan, "moving"); r != "used by a running move or restore" {
		t.Fatal(r)
	}
}

func TestLegacyLocationIsADistinctObject(t *testing.T) {
	a := object("same", at(1, 1))
	b := a
	b.Location = "legacy"
	plan := Select("app", []Object{object("new", at(0, 1)), a, b}, Policy{Daily: 2}, now, nil)
	if len(plan.Keep) != 2 || len(plan.Delete) != 1 || plan.Delete[0].Location != "legacy" {
		t.Fatalf("%+v", plan)
	}
}

func TestOrphanGrace(t *testing.T) {
	removed := now.AddDate(0, 0, -10)
	objects := []Object{{Key: "databases/app/predeployment.dump", Size: 1, Created: at(20, 1)}, object("a", at(12, 1)), {Key: "databases/kept/a.dump", Size: 1}}
	plan := Orphan("app", objects, removed, 30*24*time.Hour, now)
	if plan.Status != StatusOrphaned || len(plan.Keep) != 2 || len(plan.Delete) != 0 || !strings.HasPrefix(plan.Note, "orphaned (removed 2026-10-26)") {
		t.Fatalf("%+v", plan)
	}
	if plan.DeleteAfter.Format("2006-01-02") != "2026-11-25" {
		t.Fatal(plan.DeleteAfter)
	}
	plan = Orphan("app", objects, removed, 10*24*time.Hour, now)
	if len(plan.Keep) != 0 || len(plan.Delete) != 2 || plan.DeleteBytes != 101 {
		t.Fatalf("%+v", plan)
	}
}

func TestSummarizeFingerprint(t *testing.T) {
	objects := dailyHistory(20)
	a := Summarize([]DatabasePlan{Select("app", objects, Default(), now, nil), KeepAll("zz", StatusUnknown, "server unavailable", nil)}, now)
	slices.Reverse(objects)
	b := Summarize([]DatabasePlan{KeepAll("zz", StatusUnknown, "server unavailable", nil), Select("app", objects, Default(), now, nil)}, now)
	if a.Fingerprint != b.Fingerprint || a.DeleteObjects == 0 || a.Databases[0].Database != "app" || a.KeepObjects+a.DeleteObjects != 20 {
		t.Fatalf("%+v %+v", a, b)
	}
	if a.Check(a.Fingerprint) != nil || a.Check("") == nil {
		t.Fatal("check")
	}
	// A different deletion set changes the fingerprint.
	c := Summarize([]DatabasePlan{Select("app", objects, Policy{Daily: 8, Weekly: 4}, now, nil)}, now)
	if c.Fingerprint == a.Fingerprint || a.Check(c.Fingerprint) != ErrPlanChanged {
		t.Fatal("fingerprint did not change")
	}
}
