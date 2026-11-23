package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	maxAnswer = 4 << 20
	maxLog    = 64 << 10
)

// Server is what the agent tells a plugin about the server it runs on, so a
// plugin never has to guess (a release its processor cannot run, a disk too
// small for the data).
type Server struct {
	Arch        string   `json:"arch"`
	CPUFlags    []string `json:"cpu_flags,omitempty"`
	MemoryBytes uint64   `json:"memory_bytes,omitempty"`
	// StateFreeBytes is the free space of the file system the plugin's state
	// directory is on.
	StateFreeBytes uint64 `json:"state_free_bytes,omitempty"`
}

// Request is what every hook receives on standard input.
type Request struct {
	Contract int    `json:"contract"`
	Hook     string `json:"hook"`
	Server   Server `json:"server"`
	// StateDir is the plugin's own directory for what it keeps.
	StateDir string `json:"state_dir"`
	// WorkDir is a directory for this one call (a backup to write, an
	// export to read); empty for hooks without data.
	WorkDir string `json:"work_dir,omitempty"`
	// Input is the hook's own request.
	Input json.RawMessage `json:"input,omitempty"`
}

// Check is one thing a plugin verified.
type Check struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	OK         bool   `json:"ok"`
	Detail     string `json:"detail,omitempty"`
	Repairable bool   `json:"repairable,omitempty"`
	Caution    bool   `json:"caution,omitempty"`
}

// Answer is what every hook writes to standard output.
type Answer struct {
	OK       bool            `json:"ok"`
	Messages []string        `json:"messages,omitempty"`
	Checks   []Check         `json:"checks,omitempty"`
	Output   json.RawMessage `json:"output,omitempty"`
}

// Result is a hook's answer with what the agent observed.
type Result struct {
	Answer   Answer
	Log      string // the end of the plugin's standard error
	Duration time.Duration
}

// hookTimeout is HookTimeout; tests shorten it.
var hookTimeout = HookTimeout

// HookTimeout is how long a hook may run.
func HookTimeout(hook string) time.Duration {
	switch hook {
	case "describe":
		return 15 * time.Second
	case "status", "check":
		return time.Minute
	case "backup", "restore", "export", "import":
		return 6 * time.Hour
	}
	return 20 * time.Minute
}

// tail keeps the end of what is written to it.
type tail struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if t.buf.Len() > 2*t.limit {
		keep := t.buf.Bytes()[t.buf.Len()-t.limit:]
		t.buf = *bytes.NewBuffer(append([]byte(nil), keep...))
		t.over = true
	}
	return len(p), nil
}

func (t *tail) String() string {
	out := t.buf.String()
	if len(out) > t.limit {
		out, t.over = out[len(out)-t.limit:], true
	}
	if t.over {
		if i := strings.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		}
	}
	return out
}

// capped fails once more than its limit was written.
type capped struct {
	buf   bytes.Buffer
	limit int
}

func (c *capped) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.limit {
		return 0, errors.New("answer too large")
	}
	return c.buf.Write(p)
}

// Run executes one hook of the plugin installed in dir. The plugin gets the
// request on standard input, a minimal environment without the agent's
// secrets, its state directory as working directory, and the hook's time
// limit: when that passes, the plugin and everything it started are killed.
func Run(ctx context.Context, dir string, req Request) (Result, error) {
	req.Contract = Contract
	if req.StateDir == "" || !filepath.IsAbs(req.StateDir) {
		return Result{}, errors.New("a plugin needs an absolute state directory")
	}
	if err := os.MkdirAll(req.StateDir, 0o700); err != nil {
		return Result{}, err
	}
	input, err := json.Marshal(req)
	if err != nil {
		return Result{}, err
	}
	limit := hookTimeout(req.Hook)
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(dir, Program), req.Hook)
	cmd.Dir = req.StateDir
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive", "HOME=" + req.StateDir}
	cmd.Stdin = bytes.NewReader(input)
	answer, log := &capped{limit: maxAnswer}, &tail{limit: maxLog}
	cmd.Stdout, cmd.Stderr = answer, log
	// Its own process group, so a timeout ends what the hook started too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	started := time.Now()
	runErr := cmd.Run()
	result := Result{Log: log.String(), Duration: time.Since(started)}
	if ctx.Err() == context.DeadlineExceeded {
		return result, fmt.Errorf("the plugin's %s hook did not finish within %s and was stopped", req.Hook, limit)
	}
	if err := json.Unmarshal(answer.buf.Bytes(), &result.Answer); err != nil {
		if runErr != nil {
			return result, fmt.Errorf("the plugin's %s hook failed without an answer: %v", req.Hook, runErr)
		}
		return result, fmt.Errorf("the plugin's %s hook did not answer with JSON", req.Hook)
	}
	if runErr != nil && result.Answer.OK {
		return result, fmt.Errorf("the plugin's %s hook answered ok but exited with an error: %v", req.Hook, runErr)
	}
	return result, nil
}
