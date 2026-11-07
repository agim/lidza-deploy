package control

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// The writer keeps only the newest snapshot, never writes an older one
// after a newer one, retries a failed write, and flush waits for it.
func TestStateWriterWritesTheNewestSnapshotOutsideTheLock(t *testing.T) {
	previous := saveState
	t.Cleanup(func() { saveState = previous })
	var mu sync.Mutex
	written := []string{}
	release, started := make(chan struct{}), make(chan struct{}, 4)
	failures := 1
	saveState = func(_ string, value any) error {
		started <- struct{}{}
		<-release
		mu.Lock()
		defer mu.Unlock()
		if value.(string) == "s3" && failures > 0 {
			failures--
			return errors.New("disk full")
		}
		written = append(written, value.(string))
		return nil
	}
	var w stateWriter
	w.submit("state", "s1")
	<-started // the writer holds s1
	// While s1 is being written (blocked), newer snapshots arrive; only the
	// newest of them is written next.
	w.submit("state", "s2")
	w.submit("state", "s3")
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.flush(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(written) != 2 || written[0] != "s1" || written[1] != "s3" {
		t.Fatalf("written %v, want [s1 s3] (s2 superseded, s3 retried after a failure)", written)
	}
}
