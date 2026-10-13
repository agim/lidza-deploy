package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClickHouseDockerFailurePreservesStageAndRedactsCredentials(t *testing.T) {
	dir := t.TempDir()
	// Reproduce a failed Docker create/download whose stderr was previously
	// discarded. An unexpectedly echoed child credential must remain private.
	script := "#!/bin/sh\nprintf '%s\\n' 'Error response from daemon: registry request timed out' \"$CLICKHOUSE_PASSWORD\" >&2\nexit 125\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	c := ClickHouseResource{Password: strings.Repeat("a", 64)}
	var progress []string
	ctx := context.WithValue(context.Background(), diagnosticStreamKey{}, func(stage string) func(string) {
		return func(snapshot string) { progress = append(progress, stage+": "+snapshot) }
	})
	_, err := clickhouseDockerCommand(ctx, c, "download ClickHouse image", nil, []string{"CLICKHOUSE_PASSWORD=" + c.Password, "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1"}, "pull", clickhouseImage)
	if err == nil {
		t.Fatal("failed Docker command accepted")
	}
	var safe *clickhouseProvisionError
	if !errors.As(err, &safe) {
		t.Fatal("failure lost its public classification")
	}
	message := publicClickHouseFailure(err).Error()
	for _, want := range []string{"download ClickHouse image", "registry request timed out", "exit status 125", "[redacted]", "previous release retained"} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing %q from %q", want, message)
		}
	}
	if strings.Contains(message, c.Password) {
		t.Fatal("Docker error exposed credential")
	}
	if len(progress) == 0 {
		t.Fatal("image download did not report progress")
	}
	for _, snapshot := range progress {
		if strings.Contains(snapshot, c.Password) {
			t.Fatal("image download progress exposed credential")
		}
	}
	if strings.Contains(publicClickHouseFailure(errors.New("private unrelated error")).Error(), "private unrelated") {
		t.Fatal("unknown internal error exposed")
	}
}

func TestClickHouseFailureRedactsTruncatedSecret(t *testing.T) {
	secret := strings.Repeat("0123456789", 7)
	err := clickhouseFailure("container startup", errors.New(secret[11:]+"\nOOMKilled=true\n"+secret[:13]), secret)
	if strings.Contains(err.Error(), secret[11:]) || strings.Contains(err.Error(), secret[:13]) || !strings.Contains(err.Error(), "OOMKilled=true") {
		t.Fatal("truncated credential escaped redaction or useful container state was lost")
	}
}
