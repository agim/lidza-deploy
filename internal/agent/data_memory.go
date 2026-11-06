package agent

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Containers run uncapped. Managed data services and applications used to
// get fixed CPU and memory caps from a profile; a database on a fraction of
// a core and a sliver of memory crawled for hours while nothing in the
// panel showed why. Containers now use the server's resources like native
// services do, and containers created with the old caps are lifted in place.

// hostTotalMemoryMiB reads the host's memory; zero when unknown.
func hostTotalMemoryMiB() int {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	return parseMeminfo(string(raw)).TotalMiB
}

// liftContainerCaps removes a running container's CPU and memory caps by
// raising them to the host's cores and memory (`docker update` cannot clear
// a cap, and applies at once, without a restart). Containers without caps
// are left alone.
func liftContainerCaps(ctx context.Context, run func(ctx context.Context, args ...string) (string, error), container string) (bool, error) {
	out, err := run(ctx, "inspect", "--format", "{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}", container)
	if err != nil {
		return false, err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return false, nil
	}
	cpus, _ := strconv.ParseInt(fields[0], 10, 64)
	memory, _ := strconv.ParseInt(fields[1], 10, 64)
	cores := int64(runtime.NumCPU())
	total := int64(hostTotalMemoryMiB()) << 20
	wantCPU := cpus > 0 && cpus < cores*1_000_000_000
	wantMemory := memory > 0 && total > 0 && memory < total
	if !wantCPU && !wantMemory {
		return false, nil
	}
	args := []string{"update"}
	if wantCPU {
		args = append(args, "--cpus", strconv.FormatInt(cores, 10))
	}
	if wantMemory {
		size := fmt.Sprintf("%dm", total>>20)
		args = append(args, "--memory", size, "--memory-swap", size)
	}
	if _, err := run(ctx, append(args, container)...); err != nil {
		return false, err
	}
	return true, nil
}
