// Package pluginapi is what a Līdza Deploy plugin is written against: the
// request and answer of a hook, and Main, which turns a set of handlers into
// the program the agent runs (docs/plugins-proposal.md).
//
//	func main() {
//		pluginapi.Main(map[string]pluginapi.Handler{
//			"describe": func(ctx context.Context, req pluginapi.Request) (pluginapi.Answer, error) {
//				return pluginapi.Answer{OK: true}, nil
//			},
//		})
//	}
//
// The agent runs the program as `plugin <hook>` with one Request as JSON on
// standard input and reads one Answer as JSON from standard output. Whatever
// the plugin writes to standard error is kept as the hook's log. The working
// directory is the plugin's state directory.
package pluginapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// Contract is the version of the hook contract.
const Contract = 1

// Server is what the agent tells a plugin about the server it runs on.
type Server struct {
	Arch        string   `json:"arch"`
	CPUFlags    []string `json:"cpu_flags,omitempty"`
	MemoryBytes uint64   `json:"memory_bytes,omitempty"`
	// StateFreeBytes is the free space of the file system the plugin's state
	// directory is on.
	StateFreeBytes uint64 `json:"state_free_bytes,omitempty"`
}

// HasCPUFlag reports whether the server's processor has a feature flag, as
// /proc/cpuinfo names it (for example "avx2").
func (s Server) HasCPUFlag(flag string) bool {
	for _, have := range s.CPUFlags {
		if have == flag {
			return true
		}
	}
	return false
}

// Request is what every hook receives.
type Request struct {
	Contract int    `json:"contract"`
	Hook     string `json:"hook"`
	Server   Server `json:"server"`
	// StateDir is the plugin's own directory for what it keeps.
	StateDir string `json:"state_dir"`
	// WorkDir is a directory for this one call; empty for hooks without data.
	WorkDir string `json:"work_dir,omitempty"`
	// Input is the hook's own request; Decode reads it.
	Input json.RawMessage `json:"input,omitempty"`
}

// Decode reads the hook's input into v. An absent input leaves v as it is.
func (r Request) Decode(v any) error {
	if len(r.Input) == 0 {
		return nil
	}
	return json.Unmarshal(r.Input, v)
}

// Check is one thing a plugin verified.
type Check struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	OK    bool   `json:"ok"`
	// Detail says what is wrong, or what is in place.
	Detail string `json:"detail,omitempty"`
	// Repairable problems are fixed by the plugin's repair hook.
	Repairable bool `json:"repairable,omitempty"`
	// Caution marks a passing check with a risk worth reading first.
	Caution bool `json:"caution,omitempty"`
}

// Answer is what every hook answers.
type Answer struct {
	OK       bool            `json:"ok"`
	Messages []string        `json:"messages,omitempty"`
	Checks   []Check         `json:"checks,omitempty"`
	Output   json.RawMessage `json:"output,omitempty"`
}

// WithOutput sets the hook's own answer.
func (a Answer) WithOutput(v any) (Answer, error) {
	raw, err := json.Marshal(v)
	a.Output = raw
	return a, err
}

// AppInput is the input of the hooks that act for one application
// (check, provision, release).
type AppInput struct {
	App     string `json:"app"`
	Account struct {
		User string `json:"user"`
		UID  int    `json:"uid"`
		GID  int    `json:"gid"`
		Root string `json:"root"`
	} `json:"account"`
	// Options are the application's settings for this service.
	Options json.RawMessage `json:"options,omitempty"`
	// Record and Secrets are what an earlier provision answered.
	Record  json.RawMessage   `json:"record,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`
	// Sizes are the bytes that will arrive, by kind, for check.
	Sizes map[string]int64 `json:"sizes,omitempty"`
	// Move is set for export and import: which move the call belongs to.
	Move *Move `json:"move,omitempty"`
}

// Move is one call of a move's export or import. A move copies a service
// twice: everything while the source keeps serving ("bulk"), then, with the
// source stopped, what changed ("final"). "end" comes once on each server
// when the move finished or was given up, with no directory: the plugin
// removes whatever it kept for the move.
type Move struct {
	ID    string `json:"id"`
	Phase string `json:"phase"` // bulk, final or end
	// Full asks an export of the final phase for everything, not only for
	// what changed since the bulk phase.
	Full bool `json:"full,omitempty"`
	// Incremental tells an import that the directory holds only what
	// changed since the bulk phase it imported before.
	Incremental bool `json:"incremental,omitempty"`
}

// Exported is the output of the export hook.
type Exported struct {
	// Incremental says the directory holds only what changed since the
	// export of the bulk phase.
	Incremental bool `json:"incremental,omitempty"`
}

// Provisioned is the output of the provision hook.
type Provisioned struct {
	// Env is the application's environment for the service. Only variables
	// the plugin's manifest declares are accepted.
	Env map[string]string `json:"env,omitempty"`
	// Record describes what was provisioned; the agent hands it back.
	Record any `json:"record,omitempty"`
	// Secrets are sealed by the agent and handed back.
	Secrets map[string]string `json:"secrets,omitempty"`
}

// Handler implements one hook. An error becomes an answer that is not ok
// with the error as its message.
type Handler func(ctx context.Context, req Request) (Answer, error)

// Main runs the hook named by the program's first argument and exits. The
// context ends when the agent stops the hook.
func Main(handlers map[string]Handler) {
	hook := ""
	if len(os.Args) > 1 {
		hook = os.Args[1]
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	code := Serve(ctx, hook, os.Stdin, os.Stdout, handlers)
	stop()
	os.Exit(code)
}

// Serve runs one hook: it reads the request, calls the handler and writes
// the answer. It returns the exit status: 0 for an ok answer, 1 otherwise.
func Serve(ctx context.Context, hook string, in io.Reader, out io.Writer, handlers map[string]Handler) int {
	write := func(a Answer) int {
		if err := json.NewEncoder(out).Encode(a); err != nil || !a.OK {
			return 1
		}
		return 0
	}
	handler, ok := handlers[hook]
	if !ok {
		return write(Answer{Messages: []string{fmt.Sprintf("this plugin does not implement the %q hook", hook)}})
	}
	var req Request
	if err := json.NewDecoder(in).Decode(&req); err != nil {
		return write(Answer{Messages: []string{"the request is not valid JSON: " + err.Error()}})
	}
	if req.Contract != Contract {
		return write(Answer{Messages: []string{fmt.Sprintf("this plugin speaks contract %d, the agent sent %d", Contract, req.Contract)}})
	}
	req.Hook = hook
	answer, err := handler(ctx, req)
	if err != nil {
		answer.OK = false
		answer.Messages = append(answer.Messages, err.Error())
	}
	return write(answer)
}
