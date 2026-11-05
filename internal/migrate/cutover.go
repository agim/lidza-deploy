package migrate

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Step describes one cutover operation for the operator's runbook.
type Step struct {
	Name        string `json:"name"`
	Host        string `json:"host"` // source, target, dns or operator
	Description string `json:"description"`
	Reversible  bool   `json:"reversible"`
}

// Steps returns the cutover sequence in order. Each automated step maps to
// one function in this package; the DNS switch is external.
func Steps() []Step {
	return []Step{
		{"preflight", "both", "PreflightSource and PreflightTarget report no blockers; note the DNS time-to-live and lower it ahead of the window.", true},
		{"export", "source", "Export a full bundle (shared files, environment, units, schedule, database dumps) while the source keeps serving.", true},
		{"transfer", "operator", "Transport.Push the bundle over the existing SSH connection.", true},
		{"import", "target", "Import: lay out the application, restore the rehearsal data, enable units without starting them, deploy the exact commit.", true},
		{"verify", "target", "StartServices, then Verify by Host header against the target address; stop again if anything is wrong. Jobs and schedules stay off.", true},
		{"freeze", "source", "FreezeSource: maintenance page on, workers and timers stopped. The write window starts here.", true},
		{"final-sync", "source", "Export again (shared files and dumps only change after the freeze), Push, then ImportData on the target. pg_dump has no incremental mode: the window lasts as long as the dump, transfer and restore.", true},
		{"start", "target", "StartServices and InstallSchedule on the target; Verify again. From here the target is the single writer.", false},
		{"dns", "dns", "Point the name at the target. Until it converges, the source keeps answering with the maintenance page.", false},
		{"certificates", "target", "Once the name resolves to the target, its web server obtains the public certificate; Verify over TLS without InsecureTLS.", false},
		{"retain", "source", "Keep the frozen source and its final bundle until the retention period passes. Rolling back after the target accepted writes needs a fresh dump from the target.", false},
	}
}

// Freeze records what FreezeSource changed so ThawSource can undo it.
type Freeze struct {
	Stopped []string `json:"stopped"` // units that were running and are now stopped
}

// FreezeSource turns on the maintenance page and stops the given worker and
// timer units, so no job or request writes to the source while the final
// data moves. Web units stay up: the web server answers with the page. The
// units that were actually running are returned for ThawSource.
func FreezeSource(ctx context.Context, maintenance Maintenance, services ServiceControl, workers []string, message string, log io.Writer) (Freeze, error) {
	var freeze Freeze
	logf(log, "enabling maintenance page")
	if err := maintenance.Enable(message); err != nil {
		return freeze, err
	}
	for i := len(workers) - 1; i >= 0; i-- {
		unit := workers[i]
		state, err := services.State(ctx, unit)
		if err != nil {
			return freeze, err
		}
		if state.Active == "inactive" || state.Active == "failed" {
			continue
		}
		logf(log, "stopping %s", unit)
		if err := services.Stop(ctx, unit); err != nil {
			return freeze, fmt.Errorf("migrate: stop %s: %w", unit, err)
		}
		freeze.Stopped = append(freeze.Stopped, unit)
	}
	slices.Reverse(freeze.Stopped)
	return freeze, nil
}

// ThawSource reverses FreezeSource: it starts only the units that were
// running before and removes the maintenance page. Use it to abandon a
// cutover before the target has accepted writes.
func ThawSource(ctx context.Context, maintenance Maintenance, services ServiceControl, freeze Freeze, log io.Writer) error {
	for _, unit := range freeze.Stopped {
		logf(log, "starting %s", unit)
		if err := services.Start(ctx, unit); err != nil {
			return fmt.Errorf("migrate: start %s: %w", unit, err)
		}
	}
	logf(log, "disabling maintenance page")
	return maintenance.Disable()
}

// Workers returns the units of an application that write without serving
// web requests: everything that is not a socket or a unit named as a server.
func Workers(units []string) []string {
	var out []string
	for _, unit := range units {
		if strings.HasSuffix(unit, ".socket") || strings.Contains(unit, "-server.") || strings.Contains(unit, "-web.") {
			continue
		}
		out = append(out, unit)
	}
	return out
}
