package agent

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Reports metadata only: never dump contents, connection URLs or signed S3 URLs.
// Joining the reporter prevents late updates after a deployment has completed.
func backupProgress(ctx context.Context, stage string, bytes func() int64) func() {
	return backupProgressEvery(ctx, stage, bytes, 15*time.Second)
}

func backupProgressEvery(ctx context.Context, stage string, bytes func() int64, interval time.Duration) func() {
	if _, ok := ctx.Value(diagnosticKey{}).(func(string)); !ok {
		return func() {}
	}
	start := time.Now()
	report := func() {
		text := fmt.Sprintf("%s; elapsed %s", stage, time.Since(start).Round(time.Second))
		if bytes != nil {
			text += fmt.Sprintf("; %.1f MiB processed", float64(bytes())/(1<<20))
		}
		commandDiagnostic(ctx, "backup", text+".")
	}
	report()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				report()
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(stop) }); <-done }
}
