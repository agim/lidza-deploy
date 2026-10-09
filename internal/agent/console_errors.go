package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agim/lidza/packs/analytics"
	"github.com/agim/lidza/pkg/report"
)

type ErrorReporting struct {
	URL    string            `json:"url"`
	Server string            `json:"server"`
	Apps   map[string]string `json:"apps"` // control-panel app incarnation
}
type ConsoleCursor struct {
	Time time.Time      `json:"time"`
	Seen map[string]int `json:"seen"`
}
type ConsoleError struct {
	Security *Probe `json:"security,omitempty"`
	analytics.StoredError
	AppID      string `json:"app_id"`
	Generation string `json:"generation"`
	Container  string `json:"container"`
	Release    string `json:"release"`
	Commit     string `json:"commit,omitempty"`
}
type ConsoleBatch struct {
	Errors          []ConsoleError `json:"errors"`
	CollectionError string         `json:"collection_error,omitempty"`
}
type consoleRuntime interface {
	ConsoleLogs(context.Context, *Release, time.Time) (string, error)
}

func (d *Docker) ConsoleLogs(ctx context.Context, release *Release, since time.Time) (string, error) {
	return command(ctx, "", nil, "docker", "logs", "--timestamps", "--since", since.Format(time.RFC3339Nano), "--tail", "1000", release.Container)
}

func (m *Manager) configureErrorReporting(w http.ResponseWriter, r *http.Request) {
	var cfg ErrorReporting
	if err := Decode(w, r, &cfg); err != nil {
		Fail(w, 400, err)
		return
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/api/agent/errors" || u.Host == "" || !(u.Scheme == "https" || u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")) || !idPattern.MatchString(cfg.Server) || len(cfg.Apps) > 500 {
		Fail(w, 400, errors.New("invalid error reporting destination"))
		return
	}
	for id, generation := range cfg.Apps {
		if !idPattern.MatchString(id) || len(generation) < 16 || len(generation) > 128 {
			Fail(w, 400, errors.New("invalid error reporting assignment"))
			return
		}
	}
	m.reportMu.Lock()
	defer m.reportMu.Unlock()
	m.mu.Lock()
	old := m.data.ErrorReporting
	if old != nil && old.URL == cfg.URL && old.Server == cfg.Server && maps.Equal(old.Apps, cfg.Apps) {
		m.mu.Unlock()
		JSON(w, 200, map[string]string{"status": "configured"})
		return
	}
	m.data.ErrorReporting = &cfg
	if err := m.save(); err != nil {
		m.data.ErrorReporting = old
		m.mu.Unlock()
		Fail(w, 503, errors.New("could not save error reporting configuration"))
		return
	}
	m.mu.Unlock()
	select {
	case m.reportWake <- struct{}{}:
	default:
	}
	JSON(w, 200, map[string]string{"status": "configured"})
}

func (m *Manager) consoleLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.reportWake:
		case <-ticker.C:
		}
		m.consoleTick(m.ctx)
	}
}

func (m *Manager) consoleTick(ctx context.Context) {
	m.reportMu.Lock()
	defer m.reportMu.Unlock()
	m.mu.Lock()
	if m.data.ErrorReporting == nil {
		m.mu.Unlock()
		return
	}
	cfg := *m.data.ErrorReporting
	cfg.Apps = maps.Clone(cfg.Apps)
	type target struct {
		app     App
		release Release
	}
	targets := []target{}
	for _, a := range m.data.Apps {
		if a.Retiring || cfg.Apps[a.ID] == "" {
			continue
		}
		a.Env = maps.Clone(a.Env)
		for _, r := range []*Release{a.Current, a.Previous} {
			if r != nil && r.Container != "" {
				targets = append(targets, target{a, *r})
			}
		}
		for _, t := range m.data.Tasks {
			if t.AppID == a.ID && t.Mode == "worker" && t.Container != "" {
				targets = append(targets, target{a, Release{ID: t.Release, Container: t.Container}})
			}
		}
	}
	cursors := maps.Clone(m.data.ErrorCursors)
	outbox := slices.Clone(m.data.ErrorOutbox)
	m.mu.Unlock()
	if cursors == nil {
		cursors = map[string]ConsoleCursor{}
	}
	collectionError := ""
	runtime, supported := m.runtime.(consoleRuntime)
	active := map[string]bool{}
	for _, t := range targets {
		if len(outbox) >= 1000 {
			collectionError = "Error forwarding backlog is full; collection paused until delivery succeeds"
			break
		}
		if !supported {
			collectionError = "Agent runtime does not support console collection"
			break
		}
		key := cfg.Apps[t.app.ID] + ":" + t.release.Container
		active[key] = true
		cursor := cursors[key]
		if cursor.Time.IsZero() {
			cursor.Time = time.Now().UTC().Add(-15 * time.Minute)
		}
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		logs, err := runtime.ConsoleLogs(readCtx, &t.release, cursor.Time)
		cancel()
		if err != nil {
			collectionError = "Container console collection failed; existing reports remain available"
			continue
		}
		if len(logs) >= 65535 || strings.Count(logs, "\n") >= 999 {
			collectionError = "Container log read reached its size limit; some output may be missing"
		}
		if len(logs) >= 65535 {
			if end := strings.LastIndex(logs, "\n"); end >= 0 {
				logs = logs[:end]
			} else {
				logs = ""
			}
		}
		records, next := parseConsole(logs, t.app, t.release, cfg.Apps[t.app.ID], cursor, 1000-len(outbox))
		outbox = append(outbox, records...)
		cursors[key] = next
	}
	// Bound obsolete release cursors, while preserving cursors during a full backlog.
	if len(outbox) < 1000 {
		for key := range cursors {
			if !active[key] {
				delete(cursors, key)
			}
		}
	}
	m.mu.Lock()
	oldQueue, oldCursors := m.data.ErrorOutbox, m.data.ErrorCursors
	m.data.ErrorOutbox, m.data.ErrorCursors = outbox, cursors
	if err := m.save(); err != nil {
		m.data.ErrorOutbox, m.data.ErrorCursors = oldQueue, oldCursors
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	count := min(20, len(outbox))
	batch := ConsoleBatch{Errors: outbox[:count], CollectionError: collectionError}
	body, _ := json.Marshal(batch)
	req, err := http.NewRequestWithContext(ctx, "POST", cfg.URL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	req.Header.Set("X-Lidza-Server", cfg.Server)
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	var ack struct {
		Status string `json:"status"`
	}
	if res.StatusCode != 200 || json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&ack) != nil || ack.Status != "stored" {
		return
	}
	m.mu.Lock()
	m.data.ErrorOutbox = outbox[count:]
	if err := m.save(); err != nil {
		m.data.ErrorOutbox = outbox
	}
	m.mu.Unlock()
}

func parseConsole(text string, app App, release Release, generation string, cursor ConsoleCursor, capacity int) ([]ConsoleError, ConsoleCursor) {
	out := []ConsoleError{}
	next := ConsoleCursor{Time: cursor.Time, Seen: maps.Clone(cursor.Seen)}
	if next.Seen == nil {
		next.Seen = map[string]int{}
	}
	counts := map[string]int{}
	probeCount := 0
	lines := strings.Split(text, "\n")
	slices.SortStableFunc(lines, func(a, b string) int {
		x, _, _ := strings.Cut(a, " ")
		y, _, _ := strings.Cut(b, " ")
		at, _ := time.Parse(time.RFC3339Nano, x)
		bt, _ := time.Parse(time.RFC3339Nano, y)
		return at.Compare(bt)
	})
	for _, line := range lines {
		stamp, payload, ok := strings.Cut(line, " ")
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if !ok || err != nil || at.Before(cursor.Time) {
			continue
		}
		hash := sha256.Sum256([]byte(line))
		h := hex.EncodeToString(hash[:])
		counts[h]++
		if at.Equal(cursor.Time) && counts[h] <= cursor.Seen[h] {
			continue
		}
		e, found := consoleRecord(payload)
		var probe *Probe
		if !found {
			e, probe, found = securityRecord(payload)
		}
		if probe != nil {
			if probeCount >= 25 {
				found = false
				probe = nil
			} else {
				probeCount++
			}
		}
		if found && len(out) >= capacity {
			break
		} // never advance beyond unsaved errors
		if at.After(next.Time) {
			next.Time = at
			next.Seen = map[string]int{}
		}
		if at.Equal(next.Time) {
			next.Seen[h] = counts[h]
		}
		if !found {
			if len(out) > 0 && strings.HasPrefix(strings.ToUpper(out[len(out)-1].Message), "PANIC:") && (strings.HasPrefix(payload, "goroutine ") || strings.HasPrefix(payload, "\t") || strings.HasSuffix(payload, ")")) {
				last := &out[len(out)-1]
				stack := ""
				if last.Stack != nil {
					stack = *last.Stack
				}
				stack = boundedErrorText(stack+"\n"+scrubOutput(payload, outputSecrets(app.Env)), 4096)
				last.Stack = &stack
			}
			continue
		}
		secrets := outputSecrets(app.Env)
		e.Message = boundedErrorText(scrubOutput(e.Message, secrets), 2048)
		e.Stack = boundedErrorText(scrubOutput(e.Stack, secrets), 4096)
		e.Route = boundedErrorText(scrubOutput(e.Route, secrets), 512)
		e.RequestID = boundedErrorText(scrubOutput(e.RequestID, secrets), 128)
		// App/incarnation/container/time/ordinal make replay IDs deterministic.
		sum := sha256.Sum256([]byte(app.ID + generation + release.Container + line + strconv.Itoa(counts[h])))
		stored := analytics.StoredError{ID: hex.EncodeToString(sum[:]), Source: "server", Message: e.Message, Fingerprint: analytics.Fingerprint(e), CreatedAt: at}
		if e.Stack != "" {
			stored.Stack = &e.Stack
		}
		if e.Route != "" {
			stored.Route = &e.Route
		}
		if e.Method != "" {
			stored.Method = &e.Method
		}
		if e.RequestID != "" {
			stored.RequestID = &e.RequestID
		}
		out = append(out, ConsoleError{Security: probe, StoredError: stored, AppID: app.ID, Generation: generation, Container: release.Container, Release: release.ID, Commit: release.Commit})
	}
	return out, next
}

func consoleRecord(line string) (report.Error, bool) {
	e := report.Error{Source: "server"}
	var data map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &data) == nil && data != nil {
		text := func(key string) string { var s string; json.Unmarshal(data[key], &s); return s }
		level := strings.ToUpper(text("level"))
		var status int
		json.Unmarshal(data["status"], &status)
		if level != "ERROR" && level != "FATAL" && level != "PANIC" && status < 500 {
			return e, false
		}
		e.Message = text("msg")
		if e.Message == "" {
			e.Message = text("message")
		}
		if detail := text("error"); detail != "" {
			e.Message += ": " + detail
		}
		if status >= 500 && level != "ERROR" {
			e.Message = "HTTP " + strconv.Itoa(status) + " " + e.Message
		}
		e.Stack = text("stack")
		e.Route = text("route")
		if e.Route == "" {
			e.Route = strings.SplitN(text("path"), "?", 2)[0]
		}
		e.Method = text("method")
		e.RequestID = text("request_id")
		return e, e.Message != ""
	}
	upper := strings.ToUpper(strings.TrimSpace(line))
	if strings.HasPrefix(upper, "PANIC:") || strings.HasPrefix(upper, "FATAL ERROR:") || strings.HasPrefix(upper, "ERROR:") || strings.Contains(upper, "LEVEL=ERROR") || strings.HasPrefix(upper, "[ERROR]") || strings.Contains(upper, " ERROR:") || strings.Contains(upper, " FATAL ERROR:") {
		e.Message = line
		return e, true
	}
	return e, false
}
