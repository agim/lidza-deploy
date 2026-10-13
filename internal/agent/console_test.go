package agent

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestCappedConsoleBuffer(t *testing.T) {
	var buffer cappedConsoleBuffer
	input := strings.Repeat("x", consoleMaxOutput+128)
	n, err := buffer.Write([]byte(input))
	if err != nil || n != len(input) || buffer.Len() != consoleMaxOutput || !buffer.truncated {
		t.Fatalf("bounded output: n=%d len=%d truncated=%v err=%v", n, buffer.Len(), buffer.truncated, err)
	}
}

func TestRunConsoleAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("agent console requires the production root service account")
	}
	result, err := runConsole(context.Background(), "printf 'console-ready'")
	if err != nil || result.ExitCode != 0 || result.Output != "console-ready" {
		t.Fatalf("root command: %#v %v", result, err)
	}
	result, err = runConsole(context.Background(), "printf 'failed'; exit 7")
	if err != nil || result.ExitCode != 7 || result.Output != "failed" {
		t.Fatalf("exit result: %#v %v", result, err)
	}
}
