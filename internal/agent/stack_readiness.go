package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type stackServiceFailure struct {
	container            StackContainer
	reason               string
	terminal, restarting bool
}
type stackServiceFailures struct{ services []stackServiceFailure }

func (e *stackServiceFailures) Error() string {
	out := []string{}
	for _, f := range e.services {
		out = append(out, fmt.Sprintf("stack service %s is not ready (%s)", f.container.Service.Name, f.reason))
	}
	return strings.Join(out, "; ")
}
func (d *Docker) probeStackServices(ctx context.Context, r *Release) error {
	failures := &stackServiceFailures{}
	for _, c := range r.Stack.Containers {
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		quiet := context.WithValue(context.WithValue(bounded, diagnosticKey{}, false), diagnosticStreamKey{}, false)
		raw, err := command(quiet, "", nil, "docker", "inspect", "--format", `{"status":{{json .State.Status}},"exit":{{json .State.ExitCode}},"oom":{{json .State.OOMKilled}}}`, c.Container)
		var state struct {
			Status string
			Exit   int
			OOM    bool
		}
		if err != nil || json.Unmarshal([]byte(raw), &state) != nil || state.Status == "" {
			failures.services = append(failures.services, stackServiceFailure{container: c, reason: "container state unavailable"})
			cancel()
			continue
		}
		if state.Status != "running" || state.OOM {
			failures.services = append(failures.services, stackServiceFailure{container: c, reason: fmt.Sprintf("%s, exit=%d, OOMKilled=%t", state.Status, state.Exit, state.OOM), terminal: state.OOM || state.Status == "exited" || state.Status == "dead", restarting: state.Status == "restarting"})
			cancel()
			continue
		}
		_, err = command(bounded, "", nil, "docker", append([]string{"exec", c.Container}, c.Service.HealthCommand...)...)
		cancel()
		if err != nil {
			failures.services = append(failures.services, stackServiceFailure{container: c, reason: "health command failed"})
		}
	}
	if len(failures.services) > 0 {
		return failures
	}
	return nil
}
func (d *Docker) reportStackServices(ctx context.Context, err error) {
	var failures *stackServiceFailures
	if !errors.As(err, &failures) {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for _, f := range failures.services {
		commandDiagnostic(bounded, "stack-service", fmt.Sprintf("Service %s; container %s; %s", f.container.Service.Name, f.container.Container, f.reason))
		probe, stop := context.WithTimeout(bounded, 5*time.Second)
		_, _ = command(probe, "", nil, "docker", "inspect", "--format", `status={{.State.Status}} exit={{.State.ExitCode}} OOMKilled={{.State.OOMKilled}} restarts={{.RestartCount}} error={{json .State.Error}}`, f.container.Container)
		stop()
		probe, stop = context.WithTimeout(bounded, 5*time.Second)
		_, _ = command(probe, "", nil, "docker", "logs", "--tail", "50", "--timestamps", f.container.Container)
		stop()
	}
}
