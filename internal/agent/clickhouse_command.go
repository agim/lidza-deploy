package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Only this type contains diagnostics that have been scrubbed at the point
// where the retained service password is available. Unknown errors stay private.
type clickhouseProvisionError struct{ stage, detail string }

func (e *clickhouseProvisionError) Error() string { return e.stage + ": " + e.detail }

func clickhouseFailure(stage string, err error, secrets ...string) error {
	return &clickhouseProvisionError{stage: stage, detail: boundedErrorText(scrubLiveOutput(err.Error(), secrets), 2048)}
}

// Capture Docker output privately, then redact before returning a failure. In
// particular, the first image download must not disappear behind dockerStream's
// generic PostgreSQL error. No environment values or stdin are logged.
func clickhouseDockerCommand(ctx context.Context, c ClickHouseResource, stage string, input io.Reader, extra []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = input
	cmd.WaitDelay = 5 * time.Second
	secrets := []string{c.Password}
	for _, name := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"} {
		if value := os.Getenv(name); value != "" {
			cmd.Env = append(cmd.Env, name+"="+value)
			if strings.HasSuffix(name, "_PROXY") {
				secrets = append(secrets, outputSecrets(map[string]string{name: value})...)
			}
		}
	}
	cmd.Env = append(cmd.Env, extra...)
	for _, variable := range extra {
		if name, value, ok := strings.Cut(variable, "="); ok && (strings.Contains(name, "PASSWORD") || strings.Contains(name, "TOKEN") || strings.Contains(name, "SECRET") || strings.HasSuffix(name, "_KEY") || strings.HasSuffix(name, "_URL")) {
			secrets = append(secrets, outputSecrets(map[string]string{name: value})...)
		}
	}
	var output limitedBuffer
	if stage == "download ClickHouse image" {
		if stream, ok := ctx.Value(diagnosticStreamKey{}).(func(string) func(string)); ok {
			write := stream("clickhouse: " + stage)
			output.diagnostic = func(snapshot string) { write(scrubLiveOutput(snapshot, secrets)) }
		}
	}
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		cause := err
		if ctx.Err() != nil {
			cause = ctx.Err()
		}
		if message := strings.TrimSpace(output.String()); message != "" {
			cause = fmt.Errorf("%w: %s", cause, message)
		}
		return "", clickhouseFailure(stage, cause, secrets...)
	}
	return strings.TrimSpace(output.String()), nil
}

func clickhouseContainerState(ctx context.Context, c ClickHouseResource) string {
	probe, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	state, err := clickhouseDockerCommand(probe, c, "inspect container state", nil, nil, "inspect", "--format", `status={{.State.Status}} OOMKilled={{.State.OOMKilled}} exit={{.State.ExitCode}} error={{.State.Error}}`, c.Network)
	if err != nil {
		return "container state unavailable"
	}
	return boundedErrorText(scrubLiveOutput(state, []string{c.Password}), 2048)
}

func publicClickHouseFailure(err error) error {
	var safe *clickhouseProvisionError
	if errors.As(err, &safe) {
		return fmt.Errorf("managed ClickHouse provisioning failed: %w; previous release retained", safe)
	}
	return errors.New("managed ClickHouse provisioning failed; check Docker access, available memory and resource ownership on the hosting agent; previous release retained")
}
