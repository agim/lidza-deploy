package agent

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

// Pre-deployment backups are bounded so a stuck dump cannot hold a release
// forever, but the bound follows the database: a fixed deadline that suits a
// small database fails every release of a large one.
const (
	backupLimitDefault = 10 * time.Minute
	backupLimitCeiling = 4 * time.Hour
	backupLimitMargin  = 5 * time.Minute
	// backupConservativeRate is a deliberately low rate, in bytes of live
	// database per second, for dumping and uploading a database whose
	// backups have not been timed yet.
	backupConservativeRate = 4 << 20
)

// backupLimit is one database's pre-deployment backup deadline and the
// reasoning shown with it.
type backupLimit struct {
	Limit time.Duration
	Why   string
}

// predeploymentBackupLimit derives a database's pre-deployment backup
// deadline from the configured minimum, the duration of its last
// successful timed backup (twice that plus a margin) and its current size
// at a conservative rate (plus the same margin). The largest applies, at
// most backupLimitCeiling. sizeBytes is 0 when the size is unknown.
func predeploymentBackupLimit(floor time.Duration, history []BackupRecord, sizeBytes int64) backupLimit {
	if floor <= 0 {
		floor = backupLimitDefault
	}
	limit := floor
	basis := "minimum"
	reasons := []string{"minimum " + minutesText(floor)}
	var last BackupRecord
	for _, record := range history {
		if record.Seconds > 0 && record.UploadError == "" && (!record.OffsiteRequested || record.Offsite) && record.Created.After(last.Created) {
			last = record
		}
	}
	if last.Seconds > 0 {
		took := time.Duration(last.Seconds * float64(time.Second))
		estimate := 2*took + backupLimitMargin
		reasons = append(reasons, fmt.Sprintf("last successful backup (%s, %s) took %s: twice that plus %s is %s",
			last.Created.UTC().Format("2006-01-02 15:04"), bytesText(last.Size), minutesText(took), minutesText(backupLimitMargin), minutesText(estimate)))
		if estimate > limit {
			limit, basis = estimate, "last backup"
		}
	} else {
		reasons = append(reasons, "no timed backup yet")
	}
	if sizeBytes > 0 {
		estimate := time.Duration(float64(sizeBytes)/backupConservativeRate*float64(time.Second)) + backupLimitMargin
		reasons = append(reasons, fmt.Sprintf("database is %s: at a conservative %s/s plus %s that is %s",
			bytesText(sizeBytes), bytesText(backupConservativeRate), minutesText(backupLimitMargin), minutesText(estimate)))
		if estimate > limit {
			limit, basis = estimate, "database size"
		}
	} else {
		reasons = append(reasons, "current size unknown")
	}
	if limit > backupLimitCeiling {
		limit, basis = backupLimitCeiling, "ceiling"
		reasons = append(reasons, "capped at "+minutesText(backupLimitCeiling))
	}
	limit = limit.Round(time.Minute)
	if limit < floor {
		limit = floor
	}
	return backupLimit{Limit: limit, Why: basis + " decides; " + strings.Join(reasons, "; ")}
}

func minutesText(d time.Duration) string {
	minutes := int(math.Ceil(d.Minutes()))
	if minutes == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", minutes)
}

func bytesText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KiB", (n+1023)>>10)
}

// predeploymentBackupBudget computes the deadline for one database from
// its backup history and its current size, measured on the host.
func (m *Manager) predeploymentBackupBudget(ctx context.Context, id string) backupLimit {
	m.mu.Lock()
	d, ok := m.data.Databases[id]
	history := append([]BackupRecord(nil), d.Backups...)
	m.mu.Unlock()
	var size int64
	if ok {
		measure, cancel := context.WithTimeout(ctx, 10*time.Second)
		size = m.backupSourceSize(measure, d)
		cancel()
	}
	return predeploymentBackupLimit(time.Duration(m.cfg.PredeploymentBackupMinutes)*time.Minute, history, size)
}

// backupSourceSize is the live size of a database the host can measure:
// external PostgreSQL through psql, SQLite files on disk and ClickHouse
// databases this agent provisioned. It is 0 when unknown.
func (m *Manager) backupSourceSize(ctx context.Context, d Database) int64 {
	if strings.HasPrefix(d.URL, "clickhouse://") {
		if n, err := m.hostClickHouseSize(ctx, d.AppID); err == nil {
			return n
		}
		return 0
	}
	if n, ok, err := hostDatabaseSize(ctx, d); ok && err == nil {
		return n
	}
	return 0
}

type backupLimitsKey struct{}
type backupThenKey struct{}

// withBackupLimits carries deadlines computed before a release started, so
// the backup uses the same deadline the release's own timeout allowed for.
func withBackupLimits(ctx context.Context, limits map[string]backupLimit) context.Context {
	return context.WithValue(ctx, backupLimitsKey{}, limits)
}

// withBackupThen names what the operation does once its backups complete,
// for the backup's progress messages.
func withBackupThen(ctx context.Context, then string) context.Context {
	return context.WithValue(ctx, backupThenKey{}, then)
}

func backupThen(ctx context.Context) string {
	if then, ok := ctx.Value(backupThenKey{}).(string); ok && then != "" {
		return then
	}
	return "The operation continues after this completes."
}

// backupLimitFor returns the deadline for one database: precomputed by the
// release when available, otherwise computed now. It never exceeds the time
// the caller's context has left.
func (m *Manager) backupLimitFor(ctx context.Context, id string) backupLimit {
	limits, _ := ctx.Value(backupLimitsKey{}).(map[string]backupLimit)
	limit, ok := limits[id]
	if !ok {
		limit = m.predeploymentBackupBudget(ctx, id)
	}
	if deadline, has := ctx.Deadline(); has {
		if left := time.Until(deadline); left < limit.Limit {
			limit.Limit = max(left, 0)
			limit.Why += "; limited to the operation's remaining " + minutesText(left)
		}
	}
	return limit
}

// nativeReleaseBackupLimits computes the deadline of every database the
// application's pre-release backup covers, and their sum.
func (m *Manager) nativeReleaseBackupLimits(ctx context.Context, appID string) (map[string]backupLimit, time.Duration) {
	m.mu.Lock()
	ids := m.nativeReleaseBackupTargetsLocked(appID)
	m.mu.Unlock()
	limits := map[string]backupLimit{}
	var total time.Duration
	for _, id := range ids {
		limits[id] = m.predeploymentBackupBudget(ctx, id)
		total += limits[id].Limit
	}
	return limits, total
}

// nativeReleaseBackupThen tells the pre-release backup's progress what a
// native release does next: its release steps, then publication.
func nativeReleaseBackupThen(backup func(context.Context) error, profile release.Profile) func(context.Context) error {
	names := []string{}
	for _, step := range profile.Release {
		names = append(names, step.Name)
	}
	then := "The new release is published and its running services restart after this completes."
	if len(names) > 0 {
		then = "The release steps (" + strings.Join(names, ", ") + ") run after this completes; then the new release is published and its running services restart."
	}
	return func(ctx context.Context) error { return backup(withBackupThen(ctx, then)) }
}
