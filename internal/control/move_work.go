package control

import (
	"context"
	"sync"
)

// moveWork is the set of background goroutines of moves: the runners and
// the replication tunnels through the panel. Each one runs with a context
// that ends when its own does or when the panel stops its move work, and
// stop waits until all of them returned. Without it a runner or a tunnel
// outlived whatever started it (a request, a tick, a unit test) with
// nothing to end it.
type moveWork struct {
	mu      sync.Mutex
	stopped bool
	ctx     context.Context
	cancel  context.CancelFunc
	running sync.WaitGroup
}

// goWith runs fn in a goroutine with ctx, cancelled as well when the move
// work stops; false (fn does not run) once it stopped.
func (w *moveWork) goWith(ctx context.Context, fn func(context.Context)) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return false
	}
	if w.ctx == nil {
		w.ctx, w.cancel = context.WithCancel(context.Background())
	}
	run, cancel := context.WithCancel(ctx)
	unlink := context.AfterFunc(w.ctx, cancel)
	w.running.Add(1)
	go func() {
		defer w.running.Done()
		defer unlink()
		defer cancel()
		fn(run)
	}()
	return true
}

// stop cancels every runner and tunnel, refuses new ones and waits until
// they all returned.
func (w *moveWork) stop() {
	w.mu.Lock()
	w.stopped = true
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Unlock()
	w.running.Wait()
}

// stopMoveWork ends the panel's move runners and replication tunnels and
// waits for them. Jobs keep their saved state: the tick of a panel that
// starts again resumes them.
func (c *Control) stopMoveWork() {
	c.moveWork.stop()
	c.moveTunnelMu.Lock()
	running := c.moveTunnels
	c.moveTunnels = nil
	c.moveTunnelMu.Unlock()
	for _, t := range running {
		t.cancel()
		<-t.done
	}
}
