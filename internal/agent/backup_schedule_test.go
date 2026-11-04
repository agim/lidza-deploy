package agent

import (
	"testing"
	"time"
)

func TestCalendarBackupScheduleKeepsLocalCalendarTime(t *testing.T) {
	policy := BackupPolicy{Cron: "@monthly", Timezone: "America/New_York", Keep: 1}
	if err := policy.Validate(); err != nil || !policy.Scheduled() {
		t.Fatal("calendar backup policy rejected", err)
	}
	before := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	first := policy.NextBackupAfter(before)
	if !first.Equal(time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)) {
		t.Fatal("November backup missed local midnight", first)
	}
	if second := policy.NextBackupAfter(first); !second.Equal(time.Date(2026, 12, 1, 5, 0, 0, 0, time.UTC)) {
		t.Fatal("December backup missed local midnight after DST", second)
	}
	legacy := BackupPolicy{Hours: 24, Keep: 1}
	if !legacy.NextBackupAfter(before).Equal(before.Add(24*time.Hour)) || (BackupPolicy{Keep: 1}).Scheduled() {
		t.Fatal("legacy interval or manual backup behavior changed")
	}
}

func TestCalendarBackupScheduleRejectsAmbiguousOrFrequentRuns(t *testing.T) {
	for _, policy := range []BackupPolicy{
		{Hours: 1, Cron: "@daily", Keep: 1},
		{Cron: "*/15 * * * *", Keep: 1},
		{Cron: "@reboot", Keep: 1},
		{Cron: "@monthly", Timezone: "unknown/zone", Keep: 1},
		{Timezone: "UTC", Keep: 1},
	} {
		if policy.Validate() == nil {
			t.Fatal("invalid calendar policy accepted", policy)
		}
	}
}
