package control

import (
	"context"
	"log"
	"sync"
	"time"
)

// The control panel's state file is written outside c.mu. save() still
// runs under the lock, but only to marshal and encrypt a snapshot (fast,
// in memory); the write with its fsyncs, which takes seconds on a busy
// disk, happens in the background. Before this, every state change held
// c.mu through two fsyncs, so a saturated disk (a large database restore
// on the same host) stalled every page and API read of the panel.
//
// Ordering: snapshots are taken under c.mu, so a later snapshot always
// holds every earlier change. The writer keeps only the newest pending
// snapshot and writes it; nothing older can overwrite it. Durability: a
// crash loses at most the changes of the write in flight; the file on disk
// is always a complete earlier snapshot (temp file, fsync, rename). Disk
// errors are logged and retried with the newest snapshot instead of being
// returned to the caller, whose change is already in memory.

// stateWritesAsync is false in unit tests, which read the file right after
// a change (control_state_test.go).
var stateWritesAsync = true

// liveControls tracks panels in this process, so shutdown can flush them.
var liveControls sync.Map

type stateWriter struct {
	mu      sync.Mutex
	cond    *sync.Cond
	path    string
	pending any
	running bool
	lastErr error
}

// submit queues the newest snapshot and starts the writer if idle.
func (w *stateWriter) submit(path string, sealed any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cond == nil {
		w.cond = sync.NewCond(&w.mu)
	}
	w.path, w.pending = path, sealed
	if !w.running {
		w.running = true
		go w.loop()
	}
}

func (w *stateWriter) loop() {
	for {
		w.mu.Lock()
		if w.pending == nil {
			w.running = false
			w.cond.Broadcast()
			w.mu.Unlock()
			return
		}
		path, value := w.path, w.pending
		w.pending = nil
		w.mu.Unlock()
		err := saveState(path, value)
		w.mu.Lock()
		w.lastErr = err
		if err != nil && w.pending == nil {
			// Keep the snapshot for the retry unless a newer one came.
			w.pending = value
		}
		w.cond.Broadcast()
		w.mu.Unlock()
		if err != nil {
			log.Printf("control state: writing %s failed (%v); retrying", path, err)
			time.Sleep(time.Second)
		}
	}
}

// flush waits until every submitted snapshot is on disk or ctx ends.
func (w *stateWriter) flush(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		w.mu.Lock()
		if w.cond == nil {
			w.cond = sync.NewCond(&w.mu)
		}
		for w.running || w.pending != nil {
			w.cond.Wait()
		}
		w.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.lastErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// FlushState writes every panel's pending state before the process exits.
func FlushState(ctx context.Context) {
	liveControls.Range(func(key, _ any) bool {
		if c, ok := key.(*Control); ok {
			if err := c.stateWriter.flush(ctx); err != nil {
				log.Printf("control state: final write failed: %v", err)
			}
		}
		return true
	})
}
