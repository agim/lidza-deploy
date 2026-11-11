package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The console collector reads native service journals through one
// long-lived `journalctl --follow` per unit scope (system units as root,
// user units as the native user) that matches every collected unit at
// once. journalctl opens and indexes every journal file on start, whatever
// the match, so a read per unit and tick costs a second of CPU on a host
// with a large journal; a follower pays that once and is then woken by
// journald. Entries are demultiplexed into a bounded buffer per unit, which
// the tick hands to the unchanged per-unit parser and cursor.
//
// A stream restarts when its unit set changes or journalctl exits. It
// resumes after the journal cursor persisted in agent state when that
// cursor covered every unit; otherwise (first start, new unit, invalid
// cursor) it starts at the oldest unit cursor, at most a day back. The
// parser's per-unit cursor skips what was already reported either way.

const (
	journalBufferLines = 4000      // per unit, oldest dropped beyond it
	journalBufferBytes = 256 << 10 // per unit
	journalFeedLines   = 1000      // per unit and tick, like the former --lines=1000
	journalFeedBytes   = 65535     // per unit and tick, the collector's read limit
	journalEntryMax    = 1 << 20   // longer export lines are skipped
	journalLookback    = 15 * time.Minute
	journalMaxLookback = 24 * time.Hour
	journalSettleQuiet = 300 * time.Millisecond
	journalSettleMax   = 3 * time.Second
)

// JournalCursor is a scope's resume point: the journal cursor before the
// oldest entry not yet handed to the parser, and the units it covered.
type JournalCursor struct {
	Cursor string   `json:"cursor"`
	Units  []string `json:"units"`
}

// journalLine is one message line of a buffered entry.
type journalLine struct {
	seq  uint64
	prev string // cursor of the entry before this one ("" when unknown)
	at   time.Time
	text string // "RFC3339Nano message"
}

type journalBuffer struct {
	lines   []journalLine
	bytes   int
	dropped bool
	parsed  time.Time // the parser cursor's time after the last parse
}

type journalStream struct {
	user   bool
	units  []string
	cancel context.CancelFunc
	done   chan struct{}

	mu         sync.Mutex
	buffers    map[string]*journalBuffer
	seq        uint64
	lastCursor string
	received   int
	lastLine   time.Time
	err        error
}

// journalFollower owns the streams of both unit scopes.
type journalFollower struct {
	mu      sync.Mutex
	streams map[bool]*journalStream
	spawned int // journalctl processes started, for tests and diagnostics
}

func journalScopeKey(user bool) string {
	if user {
		return "user"
	}
	return "system"
}

// journalCommand builds the unstarted journalctl process of a scope.
func (m *Manager) journalCommand(ctx context.Context, user bool, args []string) (*exec.Cmd, error) {
	if m.journalSpawn != nil {
		return m.journalSpawn(ctx, user, args)
	}
	if m.nativeCommand != nil {
		return nil, errors.New("journal streams are not available in this fixture")
	}
	argv := append([]string{"journalctl"}, args...)
	if user {
		name, full, err := m.nativeUserArgv(argv)
		if err != nil {
			return nil, err
		}
		return exec.CommandContext(ctx, name, full...), nil
	}
	if os.Geteuid() != 0 {
		return nil, errors.New("this operation requires the root agent")
	}
	c := exec.CommandContext(ctx, "journalctl", args...)
	c.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	return c, nil
}

// journalStreamArgs are the follower's arguments: after a cursor, or since
// a time when no usable cursor exists.
func journalStreamArgs(user bool, units []string, after string, since time.Time) []string {
	args := []string{"--follow", "--no-tail", "--no-pager", "--quiet", "--output=json"}
	if after != "" {
		args = append(args, "--after-cursor="+after)
	} else {
		args = append(args, "--since=@"+strconv.FormatInt(since.Unix(), 10))
	}
	flag := "--unit="
	if user {
		flag = "--user-unit="
	}
	for _, unit := range units {
		args = append(args, flag+unit)
	}
	return args
}

// ensureJournal makes the scope's stream follow exactly units, starting or
// restarting it as needed. cursors are the parser cursors by unit; saved is
// the persisted resume point, which it clears when journalctl rejects it.
// It returns the running stream, or nil and the reason.
func (m *Manager) ensureJournal(ctx context.Context, user bool, units []string, cursors map[string]time.Time, saved *JournalCursor) (*journalStream, error) {
	f := &m.journal
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.streams == nil {
		f.streams = map[bool]*journalStream{}
	}
	current := f.streams[user]
	if current != nil {
		select {
		case <-current.done:
		default:
			if slices.Equal(current.units, units) {
				return current, nil
			}
		}
		current.stop()
		// The stopped stream's unparsed lines are read again from the
		// resume point; its position is the one to resume from.
		if cursor, ok := current.resumeCursor(); ok {
			*saved = JournalCursor{Cursor: cursor, Units: slices.Clone(current.units)}
		}
		delete(f.streams, user)
	}
	if len(units) == 0 {
		return nil, nil
	}
	if m.ctx.Err() != nil {
		return nil, m.ctx.Err()
	}
	after := ""
	if saved.Cursor != "" && journalCovers(saved.Units, units) {
		after = saved.Cursor
	}
	since := time.Now().UTC().Add(-journalLookback)
	for _, unit := range units {
		if at, ok := cursors[unit]; ok && !at.IsZero() && at.Before(since) {
			since = at
		}
	}
	since = maxTime(since, time.Now().UTC().Add(-journalMaxLookback))
	stream, err := m.startJournal(user, units, after, since)
	if err != nil {
		return nil, err
	}
	stream.settle()
	if after != "" && stream.rejected() {
		// An invalid or foreign cursor: journalctl exits without output.
		*saved = JournalCursor{}
		if stream, err = m.startJournal(user, units, "", since); err != nil {
			return nil, err
		}
		stream.settle()
	}
	f.streams[user] = stream
	return stream, nil
}

func journalCovers(saved, units []string) bool {
	for _, unit := range units {
		if !slices.Contains(saved, unit) {
			return false
		}
	}
	return true
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// startJournal starts one follower process. The caller holds m.journal.mu.
func (m *Manager) startJournal(user bool, units []string, after string, since time.Time) (*journalStream, error) {
	ctx, cancel := context.WithCancel(m.ctx)
	c, err := m.journalCommand(ctx, user, journalStreamArgs(user, units, after, since))
	if err != nil {
		cancel()
		return nil, err
	}
	// The whole process group stops with the stream, setpriv's child too.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	c.WaitDelay = 2 * time.Second
	c.Stderr = nil
	stdout, err := c.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := c.Start(); err != nil {
		cancel()
		return nil, err
	}
	m.journal.spawned++
	s := &journalStream{user: user, units: slices.Clone(units), cancel: cancel, done: make(chan struct{}), buffers: map[string]*journalBuffer{}, lastCursor: after, lastLine: time.Now()}
	for _, unit := range units {
		s.buffers[unit] = &journalBuffer{}
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(s.done)
		s.read(stdout)
		err := c.Wait()
		s.mu.Lock()
		if err == nil {
			err = errors.New("journalctl exited")
		}
		s.err = err
		s.mu.Unlock()
	}()
	return s, nil
}

func (s *journalStream) stop() {
	s.cancel()
	<-s.done
}

// settle waits briefly for a fresh stream's backlog, so the tick that
// started it already sees the recent entries.
func (s *journalStream) settle() {
	deadline := time.Now().Add(journalSettleMax)
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			return
		case <-time.After(50 * time.Millisecond):
		}
		s.mu.Lock()
		quiet := time.Since(s.lastLine) >= journalSettleQuiet
		s.mu.Unlock()
		if quiet {
			return
		}
	}
}

// rejected reports a stream that exited without producing an entry.
func (s *journalStream) rejected() bool {
	select {
	case <-s.done:
	default:
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received == 0
}

func (s *journalStream) failure() error {
	select {
	case <-s.done:
	default:
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// read demultiplexes JSON export entries into the unit buffers.
func (s *journalStream) read(r io.Reader) {
	reader := bufio.NewReaderSize(r, 64<<10)
	set := map[string]bool{}
	for _, unit := range s.units {
		set[unit] = true
	}
	for {
		line, err := readJournalLine(reader)
		if len(line) > 0 {
			s.add(line, set)
		}
		if err != nil {
			return
		}
	}
}

// readJournalLine returns one line, empty when it exceeded journalEntryMax.
func readJournalLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	oversized := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !oversized {
			if len(line)+len(chunk) > journalEntryMax {
				oversized, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if oversized {
			return nil, err
		}
		return line, err
	}
}

// journalEntry is the part of an export entry the collector uses. Unit
// fields stay raw: a binary field is an array, not a string.
type journalEntry struct {
	Cursor  string          `json:"__CURSOR"`
	Stamp   string          `json:"__REALTIME_TIMESTAMP"`
	Message json.RawMessage `json:"MESSAGE"`

	SystemdUnit     json.RawMessage `json:"_SYSTEMD_UNIT"`
	Unit            json.RawMessage `json:"UNIT"`
	CoredumpUnit    json.RawMessage `json:"COREDUMP_UNIT"`
	ObjectUnit      json.RawMessage `json:"OBJECT_SYSTEMD_UNIT"`
	UserUnit        json.RawMessage `json:"_SYSTEMD_USER_UNIT"`
	ManagerUserUnit json.RawMessage `json:"USER_UNIT"`
	CoredumpUser    json.RawMessage `json:"COREDUMP_USER_UNIT"`
	ObjectUserUnit  json.RawMessage `json:"OBJECT_SYSTEMD_USER_UNIT"`
}

// unit is the collected unit an entry belongs to: the fields journalctl's
// --unit (or --user-unit) match consults, the process's own unit first.
func (e *journalEntry) unit(user bool, set map[string]bool) string {
	fields := []json.RawMessage{e.SystemdUnit, e.Unit, e.CoredumpUnit, e.ObjectUnit}
	if user {
		fields = []json.RawMessage{e.UserUnit, e.ManagerUserUnit, e.CoredumpUser, e.ObjectUserUnit}
	}
	for _, raw := range fields {
		var name string
		if len(raw) > 0 && json.Unmarshal(raw, &name) == nil && set[name] {
			return name
		}
	}
	return ""
}

func (s *journalStream) add(raw []byte, set map[string]bool) {
	if raw[0] != '{' {
		return
	}
	var entry journalEntry
	if json.Unmarshal(raw, &entry) != nil {
		return
	}
	at, lines := journalEntryLines(entry.Stamp, entry.Message)
	unit := entry.unit(s.user, set)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received++
	s.lastLine = time.Now()
	prev := s.lastCursor
	if entry.Cursor != "" {
		s.lastCursor = entry.Cursor
	}
	if unit == "" || len(lines) == 0 {
		return
	}
	s.seq++
	b := s.buffers[unit]
	for _, text := range lines {
		b.lines = append(b.lines, journalLine{seq: s.seq, prev: prev, at: at, text: text})
		b.bytes += len(text) + 1
	}
	drop := 0
	for len(b.lines)-drop > journalBufferLines || b.bytes > journalBufferBytes && drop < len(b.lines) {
		b.bytes -= len(b.lines[drop].text) + 1
		drop++
	}
	if drop > 0 {
		b.lines = slices.Delete(b.lines, 0, drop)
		b.dropped = true
	}
}

// take returns a unit's buffered lines for one parse, bounded like the
// former per-unit read, and whether a limit was reached.
func (s *journalStream) take(unit string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buffers[unit]
	if b == nil {
		return "", false
	}
	limited := b.dropped
	b.dropped = false
	var out strings.Builder
	for i, line := range b.lines {
		if i >= journalFeedLines || out.Len()+len(line.text)+1 >= journalFeedBytes {
			limited = true
			break
		}
		out.WriteString(line.text)
		out.WriteByte('\n')
	}
	return out.String(), limited
}

// consumed drops a unit's lines before its new parser cursor. Lines at the
// cursor's time stay: the parser counts them to skip repeats.
func (s *journalStream) consumed(unit string, cursor time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buffers[unit]
	if b == nil {
		return
	}
	b.parsed = cursor
	b.lines = slices.DeleteFunc(b.lines, func(line journalLine) bool {
		if line.at.Before(cursor) {
			b.bytes -= len(line.text) + 1
			return true
		}
		return false
	})
}

// resumeCursor is the journal position a restart reads from without losing
// an unparsed line: before the oldest one, or after the last entry when
// every line was parsed. Lines kept at a parser cursor's time were parsed;
// a quiet unit's last line must not hold the resume point back for days.
// ok is false when that position is not a cursor.
func (s *journalStream) resumeCursor() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldest := uint64(0)
	cursor := s.lastCursor
	for _, b := range s.buffers {
		for _, line := range b.lines {
			if line.at.After(b.parsed) {
				if oldest == 0 || line.seq < oldest {
					oldest, cursor = line.seq, line.prev
				}
				break
			}
		}
	}
	return cursor, cursor != ""
}

// stopJournals ends both streams (tests and shutdown).
func (m *Manager) stopJournals() {
	m.journal.mu.Lock()
	defer m.journal.mu.Unlock()
	for user, s := range m.journal.streams {
		s.stop()
		delete(m.journal.streams, user)
	}
}

// journalSpawned is the number of journalctl processes started.
func (m *Manager) journalSpawned() int {
	m.journal.mu.Lock()
	defer m.journal.mu.Unlock()
	return m.journal.spawned
}
