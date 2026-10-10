package agent

import (
	"context"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"
)

type diagnosticKey struct{}
type diagnosticStreamKey struct{}

func commandDiagnostic(ctx context.Context, name, output string) {
	if log, ok := ctx.Value(diagnosticKey{}).(func(string)); ok {
		log(name + "\n" + output + "\n")
	}
}
func (m *Manager) deploymentDiagnostics(ctx context.Context, j job) context.Context {
	secrets := append(outputSecrets(j.app.Env), j.token)
	ctx = context.WithValue(ctx, diagnosticKey{}, func(line string) {
		line = scrubOutput(line, secrets)
		m.mu.Lock()
		defer m.mu.Unlock()
		for i := range m.data.Deployments {
			d := &m.data.Deployments[i]
			if d.ID == j.deployment {
				d.Log = deploymentLogTail(d.Log + line)
				break
			}
		}
	})
	return context.WithValue(ctx, diagnosticStreamKey{}, func(name string) func(string) {
		m.mu.Lock()
		base := ""
		for _, d := range m.data.Deployments {
			if d.ID == j.deployment {
				base = d.Log
				break
			}
		}
		m.mu.Unlock()
		return func(output string) {
			line := deploymentLogTail(base + name + "\n" + scrubLiveOutput(output, secrets) + "\n")
			m.mu.Lock()
			defer m.mu.Unlock()
			for i := range m.data.Deployments {
				if m.data.Deployments[i].ID == j.deployment {
					m.data.Deployments[i].Log = line
					break
				}
			}
		}
	})
}

// Hide incomplete secrets at a chunk boundary as well as complete values.
// Snapshots are re-redacted from the bounded raw command buffer each time.
func scrubLiveOutput(text string, secrets []string) string {
	for _, secret := range secrets {
		// A rolling buffer can begin halfway through a secret. Hide that suffix.
		for n := 1; n < len(secret); n++ {
			if strings.HasPrefix(text, secret[n:]) {
				text = "[redacted]" + text[len(secret)-n:]
				break
			}
		}
		limit := min(len(secret)-1, len(text))
		for n := limit; n > 0; n-- {
			if strings.HasSuffix(text, secret[:n]) {
				text = text[:len(text)-n] + "[redacted]"
				break
			}
		}
	}
	return scrubOutput(text, secrets)
}
func changeKeys(old App, a App) []string {
	keys := []string{}
	if old.Domain != a.Domain {
		keys = append(keys, "domain")
	}
	if old.Branch != a.Branch {
		keys = append(keys, "branch")
	}
	if !maps.Equal(old.Bindings, a.Bindings) {
		keys = append(keys, "database_bindings")
	}
	for k, v := range a.Env {
		if old.Env[k] != v {
			keys = append(keys, "env:"+k)
		}
	}
	for k := range old.Env {
		if _, ok := a.Env[k]; !ok {
			keys = append(keys, "env:"+k)
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(keys)))
}
func deploymentDuration(d Deployment) float64 {
	if d.Started == nil {
		return 0
	}
	end := time.Now()
	if d.Finished != nil {
		end = *d.Finished
	}
	return end.Sub(*d.Started).Seconds()
}

func outputSecrets(env map[string]string) []string {
	values := []string{}
	for _, value := range env {
		if value != "" {
			values = append(values, value)
		}
		if parsed, err := url.Parse(value); err == nil && parsed.User != nil {
			if password, ok := parsed.User.Password(); ok && password != "" {
				values = append(values, password)
			}
		}
	}
	return values
}
func scrubOutput(text string, values []string) string {
	values = slices.Clone(values)
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	for _, value := range values {
		if value != "" {
			text = strings.ReplaceAll(text, value, "[redacted]")
		}
	}
	return text
}

func deploymentLogTail(text string) string {
	const limit = 65536
	const notice = "[Earlier deployment output omitted; showing the latest output.]\n"
	if len(text) <= limit {
		return text
	}
	tail := text[len(text)-(limit-len(notice)):]
	// Cut at a line boundary; never expose a fragment of a redaction marker.
	if i := strings.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	} else {
		return notice + tail
	}
	return notice + tail
}
