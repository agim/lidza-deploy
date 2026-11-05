package release

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Command is one process the engine needs, always run as the application
// owner. Executors decide how to confine it; the flags describe what it needs.
type Command struct {
	Name     string   // short label for logs
	Dir      string   // working directory
	Args     []string // argv; Args[0] is the program
	Env      []string // complete environment (KEY=value); nothing is inherited
	Network  bool     // needs outbound network (fetching sources, installing packages)
	Database bool     // needs the application's databases (migrations)
	Stdin    io.Reader
	Stdout   io.Writer // nil discards
	Stderr   io.Writer // nil uses Stdout
}

// Executor runs commands as the application owner.
type Executor interface {
	Run(ctx context.Context, c Command) error
}

// LocalExecutor runs commands directly as the current user. It is meant for
// tests and for agents that already run as the application owner.
type LocalExecutor struct{}

func (LocalExecutor) Run(ctx context.Context, c Command) error {
	if len(c.Args) == 0 {
		return fmt.Errorf("release: %s has no command", c.Name)
	}
	cmd := exec.CommandContext(ctx, c.Args[0], c.Args[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdin = c.Dir, c.Env, c.Stdin
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = c.Stdout
	}
	return cmd.Run()
}

// output runs c and returns its trimmed standard output.
func output(ctx context.Context, x Executor, c Command) (string, error) {
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := x.Run(ctx, c); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 600 {
			detail = detail[len(detail)-600:]
		}
		return "", fmt.Errorf("%s: %w: %s", c.Name, err, detail)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// envValue returns the last value of key in env.
func envValue(env []string, key string) (string, bool) {
	value, found := "", false
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == key {
			value, found = v, true
		}
	}
	return value, found
}

// withEnv returns env with each KEY=value in set replacing earlier values.
func withEnv(env []string, set ...string) []string {
	keys := map[string]bool{}
	for _, entry := range set {
		k, _, _ := strings.Cut(entry, "=")
		keys[k] = true
	}
	result := make([]string, 0, len(env)+len(set))
	for _, entry := range env {
		if k, _, _ := strings.Cut(entry, "="); !keys[k] {
			result = append(result, entry)
		}
	}
	return append(result, set...)
}
