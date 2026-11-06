package agent

import (
	"strings"
	"testing"
	"time"
)

func TestClickHouseRowTTLAndCountFilter(t *testing.T) {
	cutoff := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		name, create string
		expr         string
		complex      bool
	}{
		{"no ttl", "CREATE TABLE d.t (`ts` DateTime) ENGINE = MergeTree ORDER BY ts SETTINGS index_granularity = 8192", "", false},
		{"column ttl only", "CREATE TABLE d.t (`ts` DateTime, `c` String TTL ts + toIntervalDay(1)) ENGINE = MergeTree ORDER BY ts SETTINGS index_granularity = 8192", "", false},
		{"plain row ttl", "CREATE TABLE d.t (`ts` DateTime, `retain_days` UInt16) ENGINE = MergeTree ORDER BY ts TTL greatest(toDateTime(ts), toDateTime(received_at)) + toIntervalDay(least(retain_days, 30)) SETTINGS index_granularity = 8192", "greatest(toDateTime(ts), toDateTime(received_at)) + toIntervalDay(least(retain_days, 30))", false},
		{"row ttl without settings", "CREATE TABLE d.t (`ts` Date) ENGINE = MergeTree ORDER BY ts TTL ts + toIntervalDay(7)", "ts + toIntervalDay(7)", false},
		{"two clauses", "CREATE TABLE d.t (`ts` DateTime) ENGINE = MergeTree ORDER BY ts TTL ts + toIntervalDay(1) TO VOLUME 'cold', ts + toIntervalDay(30) SETTINGS index_granularity = 8192", "ts + toIntervalDay(1) TO VOLUME 'cold', ts + toIntervalDay(30)", true},
		{"conditional delete", "CREATE TABLE d.t (`ts` DateTime, `k` UInt8) ENGINE = MergeTree ORDER BY ts TTL ts + toIntervalDay(1) DELETE WHERE k = 0 SETTINGS index_granularity = 8192", "ts + toIntervalDay(1) DELETE WHERE k = 0", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := clickhouseObject{Name: "t", Engine: "MergeTree", OwnData: 1, Create: c.create}
			expr, complex := o.rowTTL()
			if expr != c.expr || complex != c.complex {
				t.Fatalf("rowTTL = %q, %v; want %q, %v", expr, complex, c.expr, c.complex)
			}
			filter := o.countFilter(cutoff)
			if c.expr == "" || c.complex {
				if filter != "" {
					t.Fatalf("filter %q for a table without a plain row TTL", filter)
				}
				return
			}
			want := " WHERE (" + c.expr + ") > toDateTime('2026-11-06 14:00:00', 'UTC')"
			if filter != want {
				t.Fatalf("filter %q, want %q", filter, want)
			}
			if o.countFilter(time.Time{}) != "" {
				t.Fatal("a zero cutoff must not filter (archives written before cutoffs existed)")
			}
		})
	}
}

func TestClickHouseVerifyToleratesComplexTTLTables(t *testing.T) {
	manifest := ClickHouseManifest{Tables: []ClickHouseManifestTable{
		{Name: "plain", Engine: "MergeTree", Data: true, RowsBefore: 100, RowsAfter: 100, TTL: "ts + toIntervalDay(1)"},
		{Name: "tiered", Engine: "MergeTree", Data: true, RowsBefore: 100, RowsAfter: 100, TTL: "a, b", TTLComplex: true},
	}}
	present := map[string]bool{"plain": true, "tiered": true}
	if err := verifyClickHouseCounts(manifest, map[string]uint64{"plain": 100, "tiered": 90}, present); err != nil {
		t.Fatalf("a complex-TTL table may lose expired rows: %v", err)
	}
	err := verifyClickHouseCounts(manifest, map[string]uint64{"plain": 90, "tiered": 100}, present)
	if err == nil || !strings.Contains(err.Error(), "plain has 90 rows") {
		t.Fatalf("a plain-TTL table is counted exactly within the cutoff: %v", err)
	}
	if err := verifyClickHouseCounts(manifest, map[string]uint64{"plain": 100, "tiered": 120}, present); err == nil {
		t.Fatal("a complex-TTL table must not exceed the source")
	}
}
