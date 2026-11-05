package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestHostDatabaseSizeMeasuresSQLiteAndSkipsContainers(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "production.sqlite3")
	os.WriteFile(db, make([]byte, 4096), 0o600)
	os.WriteFile(db+"-wal", make([]byte, 1024), 0o600)
	if n, ok, err := hostDatabaseSize(context.Background(), Database{URL: db}); !ok || err != nil || n != 5120 {
		t.Fatal("sqlite size", n, ok, err)
	}
	if _, ok, err := hostDatabaseSize(context.Background(), Database{URL: filepath.Join(dir, "missing.sqlite3")}); !ok || err == nil {
		t.Fatal("missing sqlite file must be reported")
	}
	if _, ok, _ := hostDatabaseSize(context.Background(), Database{Mode: "managed", URL: "postgres://u:p@db:5432/app"}); ok {
		t.Fatal("container databases are measured inside their network")
	}
}
