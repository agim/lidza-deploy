package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Native applications report through the same console collector as
// container applications. Instead of Docker console output the agent reads
// the journal of each registered service unit (system units as root, user
// units through the owner's manager), converts the entries to the
// collector's timestamped line form and runs the same parsers, cursors,
// outbox and delivery. Static sites have no process and nothing to read.

// nativeConsoleTarget is one native application the control panel assigned
// to the collector, with the release its units currently run.
type nativeConsoleTarget struct {
	id      string
	release Release
}

// nativeConsoleTargetsLocked lists the assigned native applications that
// run services. The caller holds m.mu.
func (m *Manager) nativeConsoleTargetsLocked(assigned map[string]string) []nativeConsoleTarget {
	targets := []nativeConsoleTarget{}
	for id, a := range m.data.NativeApps {
		if assigned[id] == "" || len(a.Services) == 0 {
			continue
		}
		current := m.data.NativeReleases[id].Current
		targets = append(targets, nativeConsoleTarget{id: id, release: Release{ID: current.ID, Commit: current.Commit}})
	}
	slices.SortFunc(targets, func(a, b nativeConsoleTarget) int { return strings.Compare(a.id, b.id) })
	return targets
}

// journalConsoleLines converts journalctl JSON export entries to the
// collector's line form. A multi-line message becomes one line per text
// line at the entry's time; binary (non-UTF-8) messages are skipped.
func journalConsoleLines(raw string) string {
	var b strings.Builder
	for _, entry := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(entry, "{") {
			continue
		}
		var fields struct {
			Stamp   string          `json:"__REALTIME_TIMESTAMP"`
			Message json.RawMessage `json:"MESSAGE"`
		}
		if json.Unmarshal([]byte(entry), &fields) != nil {
			continue
		}
		_, lines := journalEntryLines(fields.Stamp, fields.Message)
		for _, line := range lines {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// journalEntryLines is one export entry's message as collector lines, and
// the entry's time. Entries without a time or a text message have none.
func journalEntryLines(stamp string, raw json.RawMessage) (time.Time, []string) {
	var message string
	if json.Unmarshal(raw, &message) != nil {
		return time.Time{}, nil
	}
	micros, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || micros <= 0 {
		return time.Time{}, nil
	}
	at := time.UnixMicro(micros).UTC()
	prefix := at.Format(time.RFC3339Nano) + " "
	parts := strings.Split(strings.TrimRight(message, "\n"), "\n")
	for i, part := range parts {
		parts[i] = prefix + part
	}
	return at, parts
}

// nativeConsoleUnit is one collected service unit and where its reports go.
type nativeConsoleUnit struct {
	key     string // parser cursor key: incarnation:unit
	unit    string
	user    bool
	app     string
	secrets []string
	release Release
}

// collectNativeConsole reads the journals of assigned native applications
// into the outbox through one journal stream per unit scope. It returns the
// records and the collection warning, if any, and updates the parser
// cursors (keyed like container cursors: incarnation:unit) and the
// streams' resume points.
func (m *Manager) collectNativeConsole(ctx context.Context, cfg ErrorReporting, targets []nativeConsoleTarget, cursors map[string]ConsoleCursor, journals map[string]JournalCursor, active map[string]bool, room int) ([]ConsoleError, string) {
	records := []ConsoleError{}
	warning := ""
	units := []nativeConsoleUnit{}
	scopes := map[bool][]string{false: {}, true: {}}
	since := map[bool]map[string]time.Time{false: {}, true: {}}
	for _, t := range targets {
		a, err := m.nativeApp(t.id)
		if err != nil {
			warning = "Native service bindings changed; review the application before collection resumes"
			continue
		}
		user := !m.nativeCreatedSystemUnits(t.id)
		secrets := m.nativeSecretsFor(a)
		for _, unit := range a.Services {
			if !nativeUnitPattern.MatchString(unit) || slices.Contains(scopes[user], unit) {
				continue
			}
			key := cfg.Apps[t.id] + ":" + unit
			active[key] = true
			release := t.release
			release.Container = unit
			units = append(units, nativeConsoleUnit{key: key, unit: unit, user: user, app: t.id, secrets: secrets, release: release})
			scopes[user] = append(scopes[user], unit)
			since[user][unit] = cursors[key].Time
		}
	}
	streams := map[bool]*journalStream{}
	for _, user := range []bool{false, true} {
		slices.Sort(scopes[user])
		saved := journals[journalScopeKey(user)]
		stream, err := m.ensureJournal(ctx, user, scopes[user], since[user], &saved)
		if err == nil && stream != nil {
			err = stream.failure()
		}
		if err != nil {
			warning = "Native service log collection failed; existing reports remain available"
		}
		if saved.Cursor == "" {
			delete(journals, journalScopeKey(user))
		} else {
			journals[journalScopeKey(user)] = saved
		}
		streams[user] = stream
	}
	for _, u := range units {
		if room-len(records) <= 0 {
			warning = "Error forwarding backlog is full; collection paused until delivery succeeds"
			break
		}
		stream := streams[u.user]
		if stream == nil {
			continue
		}
		cursor := cursors[u.key]
		if cursor.Time.IsZero() {
			cursor.Time = time.Now().UTC().Add(-journalLookback)
		}
		logs, limited := stream.take(u.unit)
		logs, bounded := boundConsoleRead(logs)
		if limited || bounded {
			warning = "Service log read reached its size limit; some output may be missing"
		}
		parsed, next := parseConsoleLines(logs, u.app, u.secrets, u.release, cfg.Apps[u.app], cursor, room-len(records))
		records = append(records, parsed...)
		cursors[u.key] = next
		stream.consumed(u.unit, next.Time)
	}
	for _, user := range []bool{false, true} {
		if stream := streams[user]; stream != nil {
			if cursor, ok := stream.resumeCursor(); ok {
				journals[journalScopeKey(user)] = JournalCursor{Cursor: cursor, Units: slices.Clone(stream.units)}
			}
		}
	}
	return records, warning
}
