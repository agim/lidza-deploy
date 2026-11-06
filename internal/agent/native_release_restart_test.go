package agent

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agim/lidza-deploy/internal/release"
)

type restartServices struct{ restarted bool }

func (s *restartServices) State(context.Context, string) (release.ServiceState, error) {
	if s.restarted {
		return release.ServiceState{Active: "active", Sub: "running", UnitFile: "enabled"}, nil
	}
	return release.ServiceState{Active: "inactive", Sub: "dead", UnitFile: "enabled"}, nil
}

func (s *restartServices) Restart(context.Context, string) error { s.restarted = true; return nil }

func TestSystemRestartLogsOneLinePerUnit(t *testing.T) {
	var log bytes.Buffer
	if _, err := nativeSystemRestart(context.Background(), &restartServices{}, []string{"shop-web.service"}, nil, time.Second, &log); err != nil {
		t.Fatal(err)
	}
	if got := log.String(); got != "==> restarting shop-web.service\nshop-web.service: active (running)\n" {
		t.Fatalf("log %q", got)
	}
}

func TestStatusProbesStayOutOfTheDeploymentLog(t *testing.T) {
	var logged strings.Builder
	ctx := context.WithValue(context.Background(), diagnosticKey{}, func(line string) { logged.WriteString(line) })
	if _, err := command(withoutDeploymentLog(ctx), "", nil, "sh", "-c", "echo ActiveState=active"); err != nil {
		t.Fatal(err)
	}
	if logged.Len() != 0 {
		t.Fatalf("probe output reached the log: %q", logged.String())
	}
	if _, err := command(ctx, "", nil, "sh", "-c", "echo visible"); err != nil || !strings.Contains(logged.String(), "visible") {
		t.Fatal("ordinary commands still log", logged.String(), err)
	}
}
