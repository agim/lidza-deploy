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
	return provisionLocalClickHouseLimited(ctx, c, nil)
}
func provisionLocalClickHouseLimited(ctx context.Context, c ClickHouseResource, limits []string) error {
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
		args := []string{"run", "-d", "--name", c.Network, "--label", "io.lidza.clickhouse=" + c.Network, "--network", c.Network,
			"--restart", "unless-stopped", "--memory", "2g", "--pids-limit", "512", "--ulimit", "nofile=262144:262144",
			"--log-opt", "max-size=10m", "--log-opt", "max-file=3",
			"--mount", "type=volume,src=" + c.Network + "-data,dst=/var/lib/clickhouse",
			"--mount", "type=volume,src=" + c.Network + "-logs,dst=/var/log/clickhouse-server",
			"--env", "CLICKHOUSE_DB", "--env", "CLICKHOUSE_USER", "--env", "CLICKHOUSE_PASSWORD", "--env", "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT",
			"--health-cmd", `clickhouse-client --user "$CLICKHOUSE_USER" --password "$CLICKHOUSE_PASSWORD" --query 'SELECT 1'`,
			"--health-interval", "10s", "--health-timeout", "5s", "--health-start-period", "30s", "--health-retries", "6", clickhouseImage}
		if len(limits) > 0 {
			args = append(replaceResourceArgs(args[:len(args)-1], limits), clickhouseImage)
		}
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
	commandDiagnostic(ctx, "clickhouse", "Waiting for an authenticated SELECT 1 using the retained agent credential.")
	var lastErr error
	for {
		// Authenticate using the agent's retained secret (not a health check using
		// the container's own env), so stale/mismatched credentials fail closed.
		query := `<clickhouse><user>lidza</user><password>` + c.Password + `</password></clickhouse>`
		if _, err := clickhouseDockerCommand(ctx, c, "authenticated ClickHouse query", strings.NewReader(query), nil, "exec", "-i", c.Network, "sh", "-c", `umask 077; f=$(mktemp); trap 'rm -f "$f"' EXIT; cat > "$f"; clickhouse-client --config-file "$f" --database telemetry --query 'SELECT 1'`); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return clickhouseFailure("authenticated readiness", fmt.Errorf("%v; last query: %v; %s", ctx.Err(), lastErr, clickhouseContainerState(ctx, c)), c.Password)
		case <-time.After(time.Second):
		}
	}
}
