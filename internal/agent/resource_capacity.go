package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/procfs"
)

// In automatic mode a limit is a ceiling, not a permanent RAM reservation.
// Admit bounded new work against live reclaimable RAM, host headroom, the
// build/helper pools, and concurrent launches that have not started yet.
func availableHostMemory() (int64, error) {
	fs, err := procfs.NewFS("/proc")
	if err != nil {
		return 0, err
	}
	mem, err := fs.Meminfo()
	if err != nil || mem.MemAvailableBytes == nil {
		return 0, errors.New("live host RAM measurement unavailable")
	}
	return int64(*mem.MemAvailableBytes) / mib, nil
}

func capacityState(state resourceState, containers []ContainerResource, pending map[string]resourceLaunch, building bool, available int64) resourceState {
	state.HostAvailableMB = &available
	state.PendingMemoryMB = 0
	state.StartingWork = false
	running := map[string]bool{}
	for _, c := range containers {
		running[c.Name] = c.Running
	}
	for _, launch := range pending {
		if !running[launch.Name] {
			state.PendingMemoryMB += launch.MemoryMB
			state.StartingWork = true
		}
	}
	if building && !running[state.BuilderName] {
		state.StartingWork = true
	}
	return state
}

func liveResourceCapacityState(state resourceState, containers []ContainerResource, pending map[string]resourceLaunch, building bool) (resourceState, error) {
	available, err := availableHostMemory()
	if err != nil {
		return state, err
	}
	return capacityState(state, containers, pending, building, available), nil
}

type resourceMemoryPressure struct{ ShortfallMB int64 }

func (e *resourceMemoryPressure) Error() string {
	return fmt.Sprintf("waiting for available RAM: need %d MiB more free memory to start this work while preserving host headroom", e.ShortfallMB)
}

func waitResourceCapacity[T any](ctx context.Context, attempt func(context.Context) (T, func(), error)) (T, func(), error) {
	wait, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var last error
	for {
		value, done, err := attempt(wait)
		var pressure *resourceMemoryPressure
		if !errors.As(err, &pressure) {
			return value, done, err
		}
		last = err
		commandDiagnostic(ctx, "resources", pressure.Error()+". Automatically checking again in 5 seconds.")
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-wait.Done():
			timer.Stop()
			var zero T
			if ctx.Err() != nil {
				return zero, nil, ctx.Err()
			}
			return zero, nil, fmt.Errorf("memory did not become available before the wait ended: %w", last)
		case <-timer.C:
		}
	}
}
