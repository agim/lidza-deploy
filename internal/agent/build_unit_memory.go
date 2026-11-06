package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// buildResourceProperties are the systemd resource properties of one build
// step unit: its memory cap, swap only when a retry allows it, and most of
// the host's CPU at a low weight, so running applications win under
// contention while the one build on the host is not starved.
func buildResourceProperties(limits buildLimits) []string {
	memory := limits.MemoryMiB
	if memory <= 0 {
		memory = 2048
	}
	quota := max(limits.CPUQuota, 100)
	props := []string{"--property=MemoryMax=" + strconv.FormatInt(int64(memory)<<20, 10)}
	if !limits.Swap {
		props = append(props, "--property=MemorySwapMax=0")
	}
	return append(props, "--property=OOMScoreAdjust=500",
		"--property=CPUQuota="+strconv.Itoa(quota)+"%", "--property=CPUWeight=20",
		"--property=IOWeight=10", "--property=Nice=10")
}

// unitMemoryOutcome is what systemd reported about a finished step unit.
type unitMemoryOutcome struct {
	OutOfMemory bool
	PeakBytes   int64
}

var unitMemoryPeakPattern = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)([BKMGTPE]) memory peak`)

// parseUnitJournal reads a unit's end-of-run journal lines: systemd logs
// "result 'oom-kill'" for a unit ended for memory, and current versions
// log "Consumed …, 1.2G memory peak" when it stops.
func parseUnitJournal(journal string) unitMemoryOutcome {
	var out unitMemoryOutcome
	out.OutOfMemory = strings.Contains(journal, "result 'oom-kill'")
	for _, match := range unitMemoryPeakPattern.FindAllStringSubmatch(journal, -1) {
		value, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			continue
		}
		shift := strings.Index("BKMGTPE", match[2]) * 10
		out.PeakBytes = max(out.PeakBytes, int64(value*float64(int64(1)<<shift)))
	}
	return out
}

// nativeReleaseUnitOutcome reads a collected step unit's journal; tests
// replace it. A failed unit is read again briefly when its lines have not
// arrived yet (the journal is written asynchronously).
var nativeReleaseUnitOutcome = func(unit string, failed bool) unitMemoryOutcome {
	var out unitMemoryOutcome
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		raw, err := exec.CommandContext(ctx, "journalctl", "--no-pager", "-o", "cat", "-n", "30", "-u", unit).Output()
		cancel()
		if err == nil {
			out = parseUnitJournal(string(raw))
		}
		if out.PeakBytes > 0 || out.OutOfMemory || !failed {
			return out
		}
		time.Sleep(200 * time.Millisecond)
	}
	return out
}

// Unit memory sampling, for systemd versions that do not log the peak:
// the unit's cgroup is read while it runs. memory.peak (cgroup v2, Linux
// 5.19+) is the true peak; otherwise the largest memory.current sample
// counts, and on cgroup v1 memory.max_usage_in_bytes.
var (
	cgroupRoot         = "/sys/fs/cgroup"
	unitSampleInterval = time.Second
	unitControlGroup   = func(unit string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "systemctl", "show", "--property=ControlGroup", "--value", unit).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
)

func readCgroupBytes(path string) int64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0
	}
	return value
}

func sampleControlGroup(group string) int64 {
	if group == "" || !strings.HasPrefix(group, "/") || strings.Contains(group, "..") {
		return 0
	}
	v2 := filepath.Join(cgroupRoot, group)
	if peak := readCgroupBytes(filepath.Join(v2, "memory.peak")); peak > 0 {
		return peak
	}
	if current := readCgroupBytes(filepath.Join(v2, "memory.current")); current > 0 {
		return current
	}
	return readCgroupBytes(filepath.Join(cgroupRoot, "memory", group, "memory.max_usage_in_bytes"))
}

// watchUnitMemory samples a unit's memory until the returned function is
// called; that function returns the largest sample in bytes.
func watchUnitMemory(unit string) func() int64 {
	var (
		mu   sync.Mutex
		peak int64
		wg   sync.WaitGroup
	)
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		group := ""
		ticker := time.NewTicker(unitSampleInterval)
		defer ticker.Stop()
		for {
			if group == "" {
				group = unitControlGroup(unit)
			}
			if value := sampleControlGroup(group); value > 0 {
				mu.Lock()
				peak = max(peak, value)
				mu.Unlock()
			}
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	return func() int64 {
		close(done)
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		return peak
	}
}
