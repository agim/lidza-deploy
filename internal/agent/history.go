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

func commandDiagnostic(ctx context.Context, name, output string) {
	if log, ok := ctx.Value(diagnosticKey{}).(func(string)); ok {
		log(name + "\n" + output + "\n")
	}
}
func (m *Manager) deploymentDiagnostics(ctx context.Context, j job) context.Context {
	secrets := append(outputSecrets(j.app.Env), j.token)
	return context.WithValue(ctx, diagnosticKey{}, func(line string) {
		line = scrubOutput(line, secrets)
		m.mu.Lock()
		defer m.mu.Unlock()
		for i := range m.data.Deployments {
			d := &m.data.Deployments[i]
			if d.ID == j.deployment && len(d.Log) < 65536 {
				remaining := 65536 - len(d.Log)
				if len(line) > remaining {
					line = line[:remaining]
				}
				d.Log += line
				break
			}
		}
	})
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
