package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	ConsoleTimeout = 10 * time.Minute
	// Allow the HTTP response to finish after the command deadline expires.
	ConsoleResponseTimeout = ConsoleTimeout + 30*time.Second
	consoleMaxInput        = 64 << 10
	ConsoleMaxOutput       = 8 << 20
	// JSON may escape each output byte as six characters (for example, NUL).
	ConsoleMaxResponse = 6*ConsoleMaxOutput + 64<<10
)

type ConsoleRequest struct {
	Command string `json:"command"`
}

type ConsoleResult struct {
	Output   string  `json:"output"`
	Duration float64 `json:"duration_seconds"`
	ExitCode int     `json:"exit_code"`
}

type cappedConsoleBuffer struct {
	bytes.Buffer
	truncated bool
}

func (b *cappedConsoleBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := ConsoleMaxOutput - b.Len()
	if remaining <= 0 {
		b.truncated = true
		return original, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.Buffer.Write(p)
	return original, nil
}

func runConsole(ctx context.Context, command string) (ConsoleResult, error) {
	if os.Geteuid() != 0 {
		return ConsoleResult{}, errors.New("hosting agent must run as root to use the server console")
	}
	if strings.TrimSpace(command) == "" || len(command) > consoleMaxInput || strings.IndexByte(command, 0) >= 0 {
		return ConsoleResult{}, errors.New("command must contain 1 to 65536 bytes")
	}
	ctx, cancel := context.WithTimeout(ctx, ConsoleTimeout)
	defer cancel()
	started := time.Now()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-lc", command)
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	var output cappedConsoleBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	result := ConsoleResult{Output: output.String(), Duration: time.Since(started).Seconds()}
	if output.truncated {
		result.Output += "\n[output truncated at 8 MiB]\n"
	}
	if ctx.Err() != nil {
		result.ExitCode = 124
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.Output += "\n[command runtime limit reached; process terminated]\n"
			return result, nil
		}
		return result, ctx.Err()
	}
	if err == nil {
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	return result, err
}
