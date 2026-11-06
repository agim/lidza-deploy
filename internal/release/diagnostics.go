package release

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Inspector explains a unit after a failed health check: the service
// manager's view of it and what it logged since the restart began.
type Inspector interface {
	Inspect(ctx context.Context, unit string, since time.Time) (UnitReport, error)
}

// UnitReport is a service manager's account of one unit.
type UnitReport struct {
	State string // systemctl show output, one property per line
	Log   string // the unit's log lines since the restart began
}

// HealthResponse is the answer a health check received when its last
// attempt was refused with an HTTP status. A health function returns (or
// wraps) it so the deploy log can show what the application said.
type HealthResponse struct {
	Status int
	Body   string
}

func (r *HealthResponse) Error() string { return "HTTP " + strconv.Itoa(r.Status) }

const (
	// UnitReportProperties are the systemctl show properties a UnitReport
	// carries: why the unit is not serving, how often it has been restarted
	// and how its main process ended.
	UnitReportProperties = "ActiveState,SubState,Result,NRestarts,ExecMainStatus,ExecMainCode"
	// UnitLogLines bounds the journal lines a UnitReport carries.
	UnitLogLines = 40
	// HealthBodyLimit bounds the health response body the deploy log shows.
	HealthBodyLimit = 300
	// unitLogLimit bounds one unit's log in the deploy log.
	unitLogLimit = 8192
)

// JournalArgs are the journalctl arguments that read a unit's last
// UnitLogLines lines since t, as plain message text.
func JournalArgs(unit string, since time.Time) []string {
	return []string{"-u", unit, "--since", "@" + strconv.FormatInt(since.Unix(), 10),
		"-n", strconv.Itoa(UnitLogLines), "-o", "cat", "--no-pager"}
}

// SummarizeUnitState puts systemctl show output on one line, in the order
// of UnitReportProperties.
func SummarizeUnitState(show string) string {
	values := map[string]string{}
	for _, line := range strings.Split(show, "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			values[key] = value
		}
	}
	var parts []string
	for _, key := range strings.Split(UnitReportProperties, ",") {
		if value, ok := values[key]; ok {
			parts = append(parts, key+"="+value)
		}
	}
	return strings.Join(parts, " ")
}

// Inspect reads the unit's state and its journal from the owner's user
// manager.
func (u UserSystemd) Inspect(ctx context.Context, unit string, since time.Time) (UnitReport, error) {
	if !unitPattern.MatchString(unit) {
		return UnitReport{}, fmt.Errorf("release: invalid unit %q", unit)
	}
	var report UnitReport
	state, err := u.systemctl(ctx, "show", unit, "--property="+UnitReportProperties)
	if err != nil {
		return report, err
	}
	report.State = state
	report.Log, err = output(ctx, u.Exec, Command{Name: "journalctl", Dir: "/", Env: u.Env,
		Args: append([]string{"journalctl", "--user"}, JournalArgs(unit, since)...)})
	return report, err
}

// inspector is the plan's explicit inspector, or its services when they
// can inspect.
func (p *Plan) inspector() Inspector {
	if p.Inspector != nil {
		return p.Inspector
	}
	if inspector, ok := p.Services.(Inspector); ok {
		return inspector
	}
	return nil
}

// explain appends to the deploy log why a release failed its checks: the
// health check's last HTTP answer, and for every unit the restart touched
// its state and log since the restart began. Nothing here can fail the
// release; a unit that cannot be inspected is noted.
func (p *Plan) explain(ctx context.Context, since time.Time, units []string, failure error) {
	if p.Log == nil {
		return
	}
	var block strings.Builder
	var response *HealthResponse
	if errors.As(failure, &response) {
		fmt.Fprintf(&block, "==> last health response: HTTP %d\n", response.Status)
		if body := strings.TrimSpace(excerpt(response.Body, HealthBodyLimit)); body != "" {
			block.WriteString(body + "\n")
		}
	}
	if inspector := p.inspector(); inspector != nil {
		for _, unit := range units {
			report, err := inspector.Inspect(ctx, unit, since)
			if err != nil {
				fmt.Fprintf(&block, "==> %s could not be inspected: %v\n", unit, err)
				continue
			}
			fmt.Fprintf(&block, "==> %s since %s: %s\n", unit, since.UTC().Format("15:04:05"), SummarizeUnitState(report.State))
			if log := strings.TrimSpace(lastLines(report.Log, unitLogLimit)); log != "" {
				block.WriteString(log + "\n")
			} else {
				block.WriteString("(no log output since the restart)\n")
			}
		}
	}
	text := block.String()
	if p.Mask != nil {
		text = p.Mask(text)
	}
	_, _ = p.Log.Write([]byte(text))
}

// attempted lists, in order, the units a restart reached: the ones
// restarted and the first one that did not come back. Units the restart
// skipped were never touched.
func (p *Plan) attempted(result *Result) []string {
	if p.Services == nil {
		// Another restart mechanism (system units) handled every unit.
		return p.Units
	}
	var units []string
	restarted := map[string]bool{}
	for _, unit := range result.Restarted {
		restarted[unit] = true
	}
	for _, unit := range p.Units {
		if _, skipped := result.Skipped[unit]; skipped {
			continue
		}
		units = append(units, unit)
		if !restarted[unit] {
			break
		}
	}
	return units
}

// excerpt returns the first limit bytes of text.
func excerpt(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

// lastLines returns the last limit bytes of text, starting at a line boundary.
func lastLines(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := text[len(text)-limit:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 {
		cut = cut[i+1:]
	}
	return "…\n" + cut
}
