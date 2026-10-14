package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const clickhouseImage = "clickhouse/clickhouse-server:26.8.15.10"

func provisionLocalClickHouse(ctx context.Context, c ClickHouseResource) error {
	password, decodeErr := hex.DecodeString(c.Password)
	if !strings.HasPrefix(c.Network, "lidza-clickhouse-") || !idPattern.MatchString(strings.TrimPrefix(c.Network, "lidza-clickhouse-")) || decodeErr != nil || len(password) != 32 {
		return errors.New("invalid managed ClickHouse identity")
	}
	for _, r := range []struct{ kind, name string }{{"network", c.Network}, {"volume", c.Network + "-data"}, {"volume", c.Network + "-logs"}} {
		found, err := clickhouseDockerCommand(ctx, c, "inspect "+r.kind+" "+r.name, nil, nil, r.kind, "ls", "--filter", "name=^"+r.name+"$", "--format", "{{.Name}}")
		if err != nil {
			return err
		}
		if found == "" {
			if _, err := clickhouseDockerCommand(ctx, c, "create "+r.kind+" "+r.name, nil, nil, r.kind, "create", "--label", "io.lidza.clickhouse="+c.Network, r.name); err != nil {
				return err
			}
		} else {
			owner, err := clickhouseDockerCommand(ctx, c, "check ownership of "+r.name, nil, nil, r.kind, "inspect", "--format", `{{index .Labels "io.lidza.clickhouse"}}`, r.name)
			if err != nil {
				return err
			}
			if owner != c.Network {
				return clickhouseFailure("resource ownership", fmt.Errorf("%s is not owned by this managed ClickHouse service; existing data retained", r.name), c.Password)
			}
		}
	}
	found, err := clickhouseDockerCommand(ctx, c, "inspect existing container", nil, nil, "ps", "-a", "--filter", "name=^/"+c.Network+"$", "--format", "{{.ID}}")
	if err != nil {
		return err
	}
	if found == "" {
		image, err := clickhouseDockerCommand(ctx, c, "inspect ClickHouse image", nil, nil, "image", "ls", "--quiet", "--filter", "reference="+clickhouseImage)
		if err != nil {
			return err
		}
		if image == "" {
			commandDiagnostic(ctx, "clickhouse", "Downloading "+clickhouseImage+" before container creation.")
			if _, err := clickhouseDockerCommand(ctx, c, "download ClickHouse image", nil, nil, "pull", clickhouseImage); err != nil {
				return err
			}
		}
		commandDiagnostic(ctx, "clickhouse", "Starting private ClickHouse container "+c.Network+".")
		// Secrets travel in the child environment, never argv or deployment logs.
		args := []string{
			"run", "-d", "--name", c.Network, "--label", "io.lidza.clickhouse=" + c.Network, "--network", c.Network,
			"--restart", "unless-stopped", "--memory", "2g", "--pids-limit", "512", "--ulimit", "nofile=262144:262144",
			"--log-opt", "max-size=10m", "--log-opt", "max-file=3",
			"--mount", "type=volume,src=" + c.Network + "-data,dst=/var/lib/clickhouse",
			"--mount", "type=volume,src=" + c.Network + "-logs,dst=/var/log/clickhouse-server",
			"--env", "CLICKHOUSE_DB", "--env", "CLICKHOUSE_USER", "--env", "CLICKHOUSE_PASSWORD", "--env", "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT",
			"--health-cmd", `clickhouse-client --user "$CLICKHOUSE_USER" --password "$CLICKHOUSE_PASSWORD" --query 'SELECT 1'`,
			"--health-interval", "10s", "--health-timeout", "5s", "--health-start-period", "30s", "--health-retries", "6", clickhouseImage}
		_, err = clickhouseDockerCommand(ctx, c, "create/start ClickHouse container", nil, []string{"CLICKHOUSE_DB=telemetry", "CLICKHOUSE_USER=lidza", "CLICKHOUSE_PASSWORD=" + c.Password, "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1"}, args...)
		if err != nil {
			return err
		}
	} else {
		owner, err := clickhouseDockerCommand(ctx, c, "check container ownership", nil, nil, "inspect", "--format", `{{index .Config.Labels "io.lidza.clickhouse"}}`, c.Network)
		if err != nil {
			return err
		}
		if owner != c.Network {
			return clickhouseFailure("container ownership", fmt.Errorf("%s is not owned by this managed ClickHouse service; existing container retained", c.Network), c.Password)
		}
		if _, err := clickhouseDockerCommand(ctx, c, "start retained ClickHouse container", nil, nil, "start", c.Network); err != nil {
			return err
		}
	}
	commandDiagnostic(ctx, "clickhouse", "Authenticating with the retained agent credential and ensuring the telemetry database exists.")
	var lastErr error
	lastProgress := time.Now()
	restarted := false
	for {
		// Authenticate using the agent's retained secret (not a health check using
		// the container's own env), so stale/mismatched credentials fail closed.
		// Do not select telemetry before creating it: an interrupted first start can
		// leave a valid server and user on the retained volume without that database.
		query := `<clickhouse><user>lidza</user><password>` + c.Password + `</password></clickhouse>`
		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := clickhouseDockerCommand(queryCtx, c, "authenticated ClickHouse query", strings.NewReader(query), nil, "exec", "-i", c.Network, "sh", "-c", `umask 077; f=$(mktemp); trap 'rm -f "$f"' EXIT; cat > "$f"; clickhouse-client --host 127.0.0.1 --connect_timeout 3 --receive_timeout 5 --send_timeout 5 --config-file "$f" --multiquery --query 'CREATE DATABASE IF NOT EXISTS telemetry; SELECT 1'`)
		probeTimedOut := errors.Is(queryCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		// A client blocked inside a running container is not normal warm-up. Restart
		// this owned service once, preserving its volumes and retained credential.
		if probeTimedOut && !restarted {
			commandDiagnostic(ctx, "clickhouse", "ClickHouse is running but not answering a bounded local query; restarting the managed container once.")
			if _, restartErr := clickhouseDockerCommand(ctx, c, "restart unresponsive ClickHouse container", nil, nil, "restart", "--time", "10", c.Network); restartErr != nil {
				return restartErr
			}
			restarted = true
			lastProgress = time.Now()
		}
		if time.Since(lastProgress) >= 15*time.Second {
			commandDiagnostic(ctx, "clickhouse", "Still waiting for authenticated ClickHouse readiness: "+boundedErrorText(lastErr.Error(), 500))
			lastProgress = time.Now()
		}
		select {
		case <-ctx.Done():
			return clickhouseFailure("authenticated readiness", fmt.Errorf("%v; last query: %v; %s", ctx.Err(), lastErr, clickhouseContainerState(ctx, c)), c.Password)
		case <-time.After(time.Second):
		}
	}
}
