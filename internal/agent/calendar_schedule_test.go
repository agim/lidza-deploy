package agent

import (
	"testing"
	"time"
)

func TestTaskCalendarSchedules(t *testing.T) {
	sunday := 0
	weekly := Task{Daily: "09:00", WeeklyDay: &sunday, Timezone: "UTC"}
	s, err := weekly.schedule()
	if err != nil {
		t.Fatal(err)
	}
	if next := s.Next(time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)); !next.Equal(time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("weekly next %s", next)
	}
	s, err = (Task{DailyTimes: []string{"18:00", "09:00"}, Timezone: "America/New_York"}).schedule()
	if err != nil {
		t.Fatal(err)
	}
	if next := s.Next(time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)); !next.Equal(time.Date(2026, 11, 2, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("multi-time next %s", next)
	}
	// Existing records may include both fields; their daily precedence remains.
	s, err = (Task{Daily: "09:00", EveryMinutes: 60}).schedule()
	if err != nil {
		t.Fatal(err)
	}
	if next := s.Next(time.Date(2026, 11, 2, 8, 30, 0, 0, time.UTC)); next.Hour() != 9 || next.Minute() != 0 {
		t.Fatal(next)
	}
}

func TestTaskCalendarRejectsAmbiguousOrInvalidDefinitions(t *testing.T) {
	badDay := 7
	day := 1
	for _, task := range []Task{
		{Daily: "09:00", DailyTimes: []string{"10:00"}},
		{DailyTimes: []string{"09:00", "09:00"}},
		{DailyTimes: []string{"25:00"}},
		{DailyTimes: []string{"09:00"}, Timezone: "Invalid/Zone"},
		{WeeklyDay: &day},
		{WeeklyDay: &badDay, Daily: "09:00"},
		{WeeklyDay: &day, Daily: "09:00", DailyTimes: []string{"10:00"}},
	} {
		if _, err := task.schedule(); err == nil {
			t.Fatalf("invalid schedule accepted: %+v", task)
		}
	}
}

func TestTaskCronPreservesExactClockMinutes(t *testing.T) {
	task := Task{ID: "report", Mode: "schedule", Command: []string{"report"}, Cron: "0,15,30,45 7-9 * * 1-5", Timezone: "America/New_York"}
	if err := task.Validate(); err != nil {
		t.Fatal(err)
	}
	schedule, err := task.schedule()
	if err != nil {
		t.Fatal(err)
	}
	after := time.Date(2026, 11, 2, 12, 16, 0, 0, time.UTC)
	want := time.Date(2026, 11, 2, 12, 30, 0, 0, time.UTC)
	if next := schedule.Next(after); !next.Equal(want) {
		t.Fatalf("next run = %s, want %s", next, want)
	}
	task.EveryMinutes = 15
	if task.Validate() == nil {
		t.Fatal("ambiguous cron and interval accepted")
	}
}
