package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ClickHouse system log retention. A stock ClickHouse server keeps its
// system logs (query_log, text_log, trace_log, metric_log and friends)
// forever, and on a busy server they outgrow the application's own data.
// Every ClickHouse the agent manages, the host server and each managed
// container, gets the same policy:
//
//   - new system log tables are created with a TTL, from a config.d file
//     (the host server's lidza.xml, a container's lidza-logs.xml);
//   - existing system log tables, including the "<name>_N" tables ClickHouse
//     leaves behind when a table's definition changes, get the same TTL with
//     ALTER TABLE ... MODIFY TTL, which takes effect without a restart;
//   - text_log records warnings and errors only;
//   - the profiling and metrics logs (sampled stack traces, per-processor and
//     per-thread query profiles, periodic metric snapshots) are switched off.
//     They exist to profile ClickHouse itself; on a small server they were
//     measured at twice the size of the application's data and are a
//     constant write load. The config.d file removes them, which ClickHouse
//     applies at its next start; until then, and for tables left from
//     before, the hourly reconciliation empties them.
//
// What stays is what explains a problem after the fact: queries, part
// operations, warnings and errors, for a few days.
//
// The reconciliation is idempotent and runs on provisioning and hourly.

// clickhouseLogTable is one system log table: how many days it keeps, or
// Off when the server does not write it at all.
type clickhouseLogTable struct {
	Name string
	Days int
	Off  bool
}

// clickhouseSystemLogTables are the system log tables the policy bounds.
var clickhouseSystemLogTables = []clickhouseLogTable{
	{Name: "query_log", Days: 3}, {Name: "part_log", Days: 3}, {Name: "text_log", Days: 3}, {Name: "error_log", Days: 14},
	{Name: "query_thread_log", Off: true}, {Name: "query_views_log", Off: true}, {Name: "query_metric_log", Off: true},
	{Name: "metric_log", Off: true}, {Name: "asynchronous_metric_log", Off: true}, {Name: "processors_profile_log", Off: true},
	{Name: "latency_log", Off: true}, {Name: "trace_log", Off: true},
}

const (
	clickhouseLogsConfigFile = "/etc/clickhouse-server/config.d/lidza-logs.xml"
	clickhouseTextLogLevel   = "warning"
	// clickhouseLogWarnBytes is the system log size server health reports
	// as a warning.
	clickhouseLogWarnBytes = 2 << 30
	clickhouseLogsInterval = time.Hour
)

// renderClickHouseSystemLogs is the system log section of a config.d file,
// one element per table, indented by four spaces.
func renderClickHouseSystemLogs() string {
	var b strings.Builder
	for _, table := range clickhouseSystemLogTables {
		if table.Off {
			fmt.Fprintf(&b, "    <%s remove=\"1\"/>\n", table.Name)
			continue
		}
		extra := ""
		if table.Name == "text_log" {
			extra = "<level>" + clickhouseTextLogLevel + "</level>"
		}
		fmt.Fprintf(&b, "    <%s>%s<ttl>event_date + INTERVAL %d DAY DELETE</ttl></%s>\n", table.Name, extra, table.Days, table.Name)
	}
	return b.String()
}

// renderClickHouseLogsConfig is a managed container's config.d/lidza-logs.xml.
func renderClickHouseLogsConfig() string {
	return `<?xml version="1.0"?>
<!-- Managed by Līdza Deploy. Changes are overwritten. -->
<clickhouse>
` + renderClickHouseSystemLogs() + `</clickhouse>
`
}

// clickhouseLogTableName matches a system log table and the numbered copies
// ClickHouse keeps after a definition change.
var clickhouseLogTableName = regexp.MustCompile(`^([a-z_]+?)(_[0-9]+)?$`)

func clickhouseLogPolicy(table string) (clickhouseLogTable, bool) {
	match := clickhouseLogTableName.FindStringSubmatch(table)
	if match == nil {
		return clickhouseLogTable{}, false
	}
	for _, name := range []string{table, match[1]} {
		if i := slices.IndexFunc(clickhouseSystemLogTables, func(t clickhouseLogTable) bool { return t.Name == name }); i >= 0 {
			return clickhouseSystemLogTables[i], true
		}
	}
	return clickhouseLogTable{}, false
}

// applyClickHouseLogTTL gives every existing system log table the policy's
// TTL and returns the tables it changed. Tables that already have it are
// left alone, so repeated runs send no ALTER.
func applyClickHouseLogTTL(ctx context.Context, c clickhouseClient) ([]string, error) {
	type row struct {
		Name   string `json:"name"`
		Engine string `json:"engine_full"`
		Rows   uint64 `json:"rows,string"`
	}
	rows, err := clickhouseRows[row](ctx, c, "SELECT name, engine_full, toString(toUInt64(ifNull(total_rows, 0))) AS rows FROM system.tables WHERE database = 'system' AND engine LIKE '%MergeTree'")
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, r := range rows {
		policy, ok := clickhouseLogPolicy(r.Name)
		if !ok {
			continue
		}
		if policy.Off {
			// Switched off: what a running server still writes, and what was
			// left from before, is emptied. An empty table costs nothing.
			if r.Rows == 0 {
				continue
			}
			ident, err := chIdent(r.Name)
			if err != nil {
				continue
			}
			if err := c.exec(ctx, "TRUNCATE TABLE IF EXISTS system."+ident); err != nil {
				return changed, fmt.Errorf("system.%s: %w", r.Name, err)
			}
			changed = append(changed, r.Name)
			continue
		}
		// ClickHouse stores the TTL normalised, for example
		// "TTL event_date + toIntervalDay(14)".
		if strings.Contains(r.Engine, fmt.Sprintf("TTL event_date + toIntervalDay(%d)", policy.Days)) {
			continue
		}
		ident, err := chIdent(r.Name)
		if err != nil {
			continue
		}
		// Not materialised: rewriting gigabytes of old log parts at once would
		// load the server for nothing. ClickHouse drops the expired parts in
		// its regular TTL merges over the following hours.
		if err := c.exec(ctx, fmt.Sprintf("ALTER TABLE system.%s MODIFY TTL event_date + INTERVAL %d DAY DELETE SETTINGS materialize_ttl_after_modify = 0", ident, policy.Days)); err != nil {
			return changed, fmt.Errorf("system.%s: %w", r.Name, err)
		}
		changed = append(changed, r.Name)
	}
	return changed, nil
}

// clickhouseLogBytes is the disk space of the server's system log tables.
func clickhouseLogBytes(ctx context.Context, c clickhouseClient) (int64, error) {
	type row struct {
		Bytes uint64 `json:"bytes"`
	}
	rows, err := clickhouseRows[row](ctx, c, "SELECT toUInt64(sum(bytes_on_disk)) AS bytes FROM system.parts WHERE database = 'system' AND active AND match(table, '_log(_[0-9]+)?$')")
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return int64(min(rows[0].Bytes, 1<<62)), nil
}

// clickhouseContainerLogsConfig writes lidza-logs.xml into a managed
// container when it differs, as root through docker exec; ClickHouse
// reloads changed config files itself. It reports whether it wrote. Tests
// replace it.
var clickhouseContainerLogsConfig = func(ctx context.Context, c ClickHouseResource) (bool, error) {
	want := renderClickHouseLogsConfig()
	if have, err := clickhouseDockerCommand(ctx, c, "read ClickHouse log policy", nil, nil, "exec", "--user", "0", c.Network, "cat", clickhouseLogsConfigFile); err == nil && strings.TrimSpace(have) == strings.TrimSpace(want) {
		return false, nil
	}
	_, err := clickhouseDockerCommand(ctx, c, "write ClickHouse log policy", strings.NewReader(want), nil, "exec", "-i", "--user", "0", c.Network,
		"sh", "-c", `f="$1"; t="$f.lidza-tmp"; cat > "$t" && chmod 644 "$t" && mv -f "$t" "$f"`, "lidza-logs", clickhouseLogsConfigFile)
	return err == nil, err
}

// clickhouseLogsConfigArchive is lidza-logs.xml as a tar stream for docker
// cp, so a new container starts with the policy and creates its system log
// tables with their TTL.
func clickhouseLogsConfigArchive() *bytes.Reader {
	var buf bytes.Buffer
	body := []byte(renderClickHouseLogsConfig())
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "lidza-logs.xml", Mode: 0o644, Size: int64(len(body)), ModTime: time.Unix(0, 0)})
	_, _ = tw.Write(body)
	_ = tw.Close()
	return bytes.NewReader(buf.Bytes())
}

// reconcileClickHouseLogs applies the policy to one managed container and
// measures its system logs.
func reconcileContainerClickHouseLogs(ctx context.Context, c ClickHouseResource) (int64, []string, error) {
	if _, err := clickhouseContainerLogsConfig(ctx, c); err != nil {
		return 0, nil, err
	}
	service, err := containerClickHouseService(ctx, c)
	if err != nil {
		return 0, nil, err
	}
	changed, err := applyClickHouseLogTTL(ctx, service.client)
	if err != nil {
		return 0, changed, err
	}
	size, err := clickhouseLogBytes(ctx, service.client)
	return size, changed, err
}

// startClickHouseLogsTick runs clickhouseLogsTick in the background, one at
// a time, so a slow ClickHouse never delays the agent's other maintenance.
func (m *Manager) startClickHouseLogsTick(now time.Time) {
	hs := m.hygiene()
	hs.mu.Lock()
	if hs.logsRunning || now.Sub(hs.logsChecked) < clickhouseLogsInterval {
		hs.mu.Unlock()
		return
	}
	hs.logsRunning = true
	hs.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.clickhouseLogsTick(now)
		hs.mu.Lock()
		hs.logsRunning = false
		hs.mu.Unlock()
	}()
}

// clickhouseLogsTick reconciles every managed ClickHouse hourly: each ready
// container and the host server. Sizes are kept for server health.
func (m *Manager) clickhouseLogsTick(now time.Time) {
	hs := m.hygiene()
	hs.mu.Lock()
	due := now.Sub(hs.logsChecked) >= clickhouseLogsInterval
	if due {
		hs.logsChecked = now
	}
	hs.mu.Unlock()
	if !due {
		return
	}
	m.mu.Lock()
	containers := map[string]ClickHouseResource{}
	for id, c := range m.data.ClickHouses {
		if _, live := m.data.Apps[id]; live && c.Ready && c.Network != "" {
			containers[id] = c
		}
	}
	server := m.data.NativeClickHouse
	m.mu.Unlock()
	sizes := map[string]int64{}
	for id, c := range containers {
		ctx, cancel := context.WithTimeout(m.ctx, 2*time.Minute)
		size, changed, err := reconcileContainerClickHouseLogs(ctx, c)
		cancel()
		if err != nil {
			log.Printf("ClickHouse system log policy for %s deferred: %v", id, err)
			continue
		}
		if len(changed) > 0 {
			log.Printf("ClickHouse system log retention applied for %s: %s", id, strings.Join(changed, ", "))
		}
		sizes[id] = size
	}
	if server != nil && server.Installed {
		if admin, err := m.unsealHostSecret(server.Password); err == nil {
			client := nativeHostDataHost(m).clickhouse(*server, "default", admin)
			ctx, cancel := context.WithTimeout(m.ctx, 2*time.Minute)
			changed, err := applyClickHouseLogTTL(ctx, client)
			if err == nil {
				var size int64
				if size, err = clickhouseLogBytes(ctx, client); err == nil {
					sizes[clickhouseHostLogKey] = size
				}
			}
			cancel()
			if err != nil {
				log.Printf("ClickHouse system log policy for the host server deferred: %v", err)
			} else if len(changed) > 0 {
				log.Printf("ClickHouse system log retention applied on the host server: %s", strings.Join(changed, ", "))
			}
		}
	}
	hs.mu.Lock()
	hs.logBytes = sizes
	hs.mu.Unlock()
}

// clickhouseHostLogKey names the host server in the system log sizes.
const clickhouseHostLogKey = "host"

// clickhouseLogSizes returns the last measured system log sizes.
func (m *Manager) clickhouseLogSizes() map[string]int64 {
	hs := m.hygiene()
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if len(hs.logBytes) == 0 {
		return nil
	}
	out := make(map[string]int64, len(hs.logBytes))
	for k, v := range hs.logBytes {
		out[k] = v
	}
	return out
}

// clickhouseLogWarnings reports system logs that are still large.
func clickhouseLogWarnings(sizes map[string]int64) []string {
	var out []string
	for id, size := range sizes {
		if size < clickhouseLogWarnBytes {
			continue
		}
		who := "ClickHouse of " + id
		if id == clickhouseHostLogKey {
			who = "The host ClickHouse server"
		}
		out = append(out, fmt.Sprintf("%s keeps %s of system logs; the agent bounds them to %d days and ClickHouse removes older rows in the background", who, bytesText(size), 14))
	}
	slices.Sort(out)
	return out
}
