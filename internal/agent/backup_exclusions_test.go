package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupExclusionsRequireExactDatabaseScopedTables(t *testing.T) {
	for _, table := range []string{"public.kulla_logs", "public.*", "*.kulla_logs", "kulla_logs", "public.logs --exclude-table=users", "public.logs;DROP TABLE users", "public."} {
		t.Run(table, func(t *testing.T) {
			cfg := Config{DataDir: t.TempDir(), APIKey: strings.Repeat("a", 32), BackupExcludeTableData: map[string][]string{"kulla-lidza": {table}}}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(path)
			if table == "public.kulla_logs" {
				if err != nil || len(loaded.BackupExcludeTableData["kulla-lidza"]) != 1 || len(loaded.BackupExcludeTableData["other-app"]) != 0 {
					t.Fatal(loaded.BackupExcludeTableData, err)
				}
			} else if err == nil {
				t.Fatal("unsafe exclusion accepted", table)
			}
		})
	}
}
