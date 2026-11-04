package agent

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtendedBackupLimitIsScopedAndBounded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := Config{DataDir: dir, APIKey: strings.Repeat("a", 32)}
	load := func(limits map[string]int64) (Config, error) {
		t.Helper()
		cfg.BackupMaxBytes = limits
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return LoadConfig(path)
	}
	extended, err := load(map[string]int64{"large-db": maxBackupSize + 1})
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: extended}
	missing := filepath.Join(dir, "missing")
	if limit, err := m.backupDiskLimit(missing, "other-db"); err != nil || limit != maxBackupSize {
		t.Fatal("unrelated database did not keep its default limit", limit, err)
	}
	if _, err := m.backupDiskLimit(missing, "large-db"); err == nil {
		t.Fatal("extended backup continued without verified free space")
	}
	for _, invalid := range []map[string]int64{
		{"*": maxBackupSize}, {"large-db": maxBackupSize - 1}, {"large-db": maxExtendedBackupSize + 1},
	} {
		if _, err := load(invalid); err == nil {
			t.Fatal("accepted invalid extended backup configuration", invalid)
		}
	}
}

func TestBackupWriterRejectsOversizeWithoutTruncatingWrite(t *testing.T) {
	for _, limit := range []int64{maxBackupSize, maxBackupSize + 1<<30} {
		writer := &backupWriter{w: io.Discard, limit: limit}
		writer.n.Store(limit - 1)
		if n, err := writer.Write([]byte{1, 2}); err == nil || n != 0 || writer.n.Load() != limit-1 {
			t.Fatal("oversized write changed the archive size", n, err, writer.n.Load())
		}
		if n, err := writer.Write([]byte{1}); err != nil || n != 1 || writer.n.Load() != limit {
			t.Fatal("exact-limit write failed", n, err, writer.n.Load())
		}
	}
}
