package agent

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Live progress of long operations (database dumps, restores, uploads and
// move archives), so the control panel can show that work continues while a
// step prints nothing. Progress is metadata only: a phase description and
// byte counts, never contents, connection URLs or signed URLs. It lives in
// memory and is never persisted; an operation that restarts starts over.

// Progress is a snapshot of one running operation. StartedAt is when the
// current phase started; UpdatedAt is when DoneBytes last changed (or the
// phase began). TotalBytes is zero when the size is unknown.
//
// Engine, DoneObjects and TotalObjects are set by multi-object operations
// (an application move copies databases, tables and file sets): the engine
// working and how many of its objects are done. Counted reports that the
// phase has live counters, so a counter that stops advancing means the
// operation stalled; phases without counters (or of unknown size whose
// counter has not moved yet) only report elapsed time.
type Progress struct {
	Phase        string    `json:"phase"`
	DoneBytes    int64     `json:"done_bytes"`
	TotalBytes   int64     `json:"total_bytes,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Engine       string    `json:"engine,omitempty"`
	DoneObjects  int64     `json:"done_objects,omitempty"`
	TotalObjects int64     `json:"total_objects,omitempty"`
	Counted      bool      `json:"counted,omitempty"`
}

// ProgressTracker accumulates the progress of one operation. Counters are
// read lazily: a phase registers a function returning its byte count, and a
// snapshot notices when the count changed. Safe for concurrent use.
type ProgressTracker struct {
	mu   sync.Mutex
	now  func() time.Time
	p    Progress
	read func() int64
}

// NewProgressTracker starts a tracker in phase.
func NewProgressTracker(phase string) *ProgressTracker {
	t := &ProgressTracker{now: time.Now}
	t.Stage(phase, 0, nil)
	return t
}

// Stage starts a phase. read returns the bytes done in this phase; nil means
// the phase reports no byte count (or Report supplies it).
func (t *ProgressTracker) Stage(phase string, total int64, read func() int64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now().UTC()
	t.p = Progress{Phase: phase, TotalBytes: max(total, 0), StartedAt: now, UpdatedAt: now, Engine: t.p.Engine, DoneObjects: t.p.DoneObjects, TotalObjects: t.p.TotalObjects, Counted: read != nil && total > 0}
	t.read = read
}

// Objects records the engine working and how many of its objects are done.
// A change counts as progress.
func (t *ProgressTracker) Objects(engine string, done, total int64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.p.Engine != engine || t.p.DoneObjects != done || t.p.TotalObjects != total {
		t.p.Engine, t.p.DoneObjects, t.p.TotalObjects = engine, max(done, 0), max(total, 0)
		t.p.UpdatedAt = t.now().UTC()
	}
}

// Mirror replaces the progress with one observed elsewhere, for example
// another agent's, under phase. Times later than now (clock skew) are
// clamped to now; a zero time means now.
func (t *ProgressTracker) Mirror(phase string, p Progress) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now().UTC()
	clamp := func(at time.Time) time.Time {
		if at.IsZero() || at.After(now) {
			return now
		}
		return at.UTC()
	}
	// The tracker's own engine and object counts stay; the observed
	// progress has counters when it reports bytes.
	t.p = Progress{Phase: phase, DoneBytes: max(p.DoneBytes, 0), TotalBytes: max(p.TotalBytes, 0), StartedAt: clamp(p.StartedAt), UpdatedAt: clamp(p.UpdatedAt),
		Engine: t.p.Engine, DoneObjects: t.p.DoneObjects, TotalObjects: t.p.TotalObjects, Counted: p.Counted || p.DoneBytes > 0 || p.TotalBytes > 0}
	t.read = nil
}

// Snapshot returns the current progress; a nil tracker has none.
func (t *ProgressTracker) Snapshot() Progress {
	if t == nil {
		return Progress{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.read != nil {
		if done := t.read(); done != t.p.DoneBytes {
			t.p.DoneBytes, t.p.UpdatedAt = done, t.now().UTC()
		}
		// A phase of unknown size counts once its counter moved.
		t.p.Counted = t.p.Counted || t.p.DoneBytes > 0
	}
	return t.p
}

// progressKey carries an operation's tracker through its context, so the
// stages of a backup (dump, finalize, upload) report into it.
type progressKey struct{}

func withProgress(ctx context.Context, t *ProgressTracker) context.Context {
	return context.WithValue(ctx, progressKey{}, t)
}

func progressFrom(ctx context.Context) *ProgressTracker {
	t, _ := ctx.Value(progressKey{}).(*ProgressTracker)
	return t
}

// progressRegistry holds the trackers of running operations by key, for
// example "database/<id>" or "move/<move>/<set>".
type progressRegistry struct {
	mu       sync.Mutex
	trackers map[string]*ProgressTracker
}

// track registers a tracker under key and returns it with a function that
// removes it again. A later registration under the same key replaces it.
func (r *progressRegistry) track(key, phase string) (*ProgressTracker, func()) {
	t := NewProgressTracker(phase)
	r.mu.Lock()
	if r.trackers == nil {
		r.trackers = map[string]*ProgressTracker{}
	}
	r.trackers[key] = t
	r.mu.Unlock()
	return t, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.trackers[key] == t {
			delete(r.trackers, key)
		}
	}
}

// snapshot returns the progress registered under key, or nil.
func (r *progressRegistry) snapshot(key string) *Progress {
	r.mu.Lock()
	t := r.trackers[key]
	r.mu.Unlock()
	if t == nil {
		return nil
	}
	p := t.Snapshot()
	return &p
}

// prefixed returns every progress whose key starts with prefix, keyed by the
// rest of the key.
func (r *progressRegistry) prefixed(prefix string) map[string]Progress {
	r.mu.Lock()
	found := map[string]*ProgressTracker{}
	for key, t := range r.trackers {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			found[key[len(prefix):]] = t
		}
	}
	r.mu.Unlock()
	out := make(map[string]Progress, len(found))
	for key, t := range found {
		out[key] = t.Snapshot()
	}
	return out
}

func databaseProgressKey(id string) string { return "database/" + id }

// trackDatabase starts progress for a database operation and returns ctx
// carrying it with a function that ends it.
func (m *Manager) trackDatabase(ctx context.Context, id, phase string) (context.Context, func()) {
	t, done := m.progress.track(databaseProgressKey(id), phase)
	return withProgress(ctx, t), done
}

// progressReader counts the bytes read through it.
type progressReader struct {
	r io.Reader
	n atomic.Int64
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n.Add(int64(n))
	return n, err
}

// progressWriter counts the bytes written through it.
type progressWriter struct{ n atomic.Int64 }

func (w *progressWriter) Write(p []byte) (int, error) {
	w.n.Add(int64(len(p)))
	return len(p), nil
}
