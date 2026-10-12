package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
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
		found, err := command(ctx, "", nil, "docker", r.kind, "ls", "--filter", "name=^"+r.name+"$", "--format", "{{.Name}}")
		if err != nil {
			return errors.New("could not inspect ClickHouse resources")
		}
		if found == "" {
			if _, err := command(ctx, "", nil, "docker", r.kind, "create", "--label", "io.lidza.clickhouse="+c.Network, r.name); err != nil {
				return errors.New("could not create ClickHouse resource")
			}
		} else {
			owner, err := command(ctx, "", nil, "docker", r.kind, "inspect", "--format", `{{index .Labels "io.lidza.clickhouse"}}`, r.name)
			if err != nil || owner != c.Network {
				return errors.New("ClickHouse resource name occupied by unmanaged resource")
			}
		}
	}
	found, err := command(ctx, "", nil, "docker", "ps", "-a", "--filter", "name=^/"+c.Network+"$", "--format", "{{.ID}}")
	if err != nil {
		return errors.New("could not inspect ClickHouse container")
	}
	if found == "" {
		// Secrets travel in the child environment, never argv or deployment logs.
		err = dockerStream(ctx, nil, io.Discard, []string{"CLICKHOUSE_DB=telemetry", "CLICKHOUSE_USER=lidza", "CLICKHOUSE_PASSWORD=" + c.Password, "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1"},
			"run", "-d", "--name", c.Network, "--label", "io.lidza.clickhouse="+c.Network, "--network", c.Network,
			"--restart", "unless-stopped", "--memory", "2g", "--pids-limit", "512", "--ulimit", "nofile=262144:262144",
			"--log-opt", "max-size=10m", "--log-opt", "max-file=3",
			"--mount", "type=volume,src="+c.Network+"-data,dst=/var/lib/clickhouse",
			"--mount", "type=volume,src="+c.Network+"-logs,dst=/var/log/clickhouse-server",
			"--env", "CLICKHOUSE_DB", "--env", "CLICKHOUSE_USER", "--env", "CLICKHOUSE_PASSWORD", "--env", "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT",
			"--health-cmd", `clickhouse-client --user "$CLICKHOUSE_USER" --password "$CLICKHOUSE_PASSWORD" --query 'SELECT 1'`,
			"--health-interval", "10s", "--health-timeout", "5s", "--health-start-period", "30s", "--health-retries", "6", clickhouseImage)
		if err != nil {
			return errors.New("could not start ClickHouse container")
		}
	} else {
		owner, err := command(ctx, "", nil, "docker", "inspect", "--format", `{{index .Config.Labels "io.lidza.clickhouse"}}`, c.Network)
		if err != nil || owner != c.Network {
			return errors.New("ClickHouse container name occupied by unmanaged container")
		}
		if _, err := command(ctx, "", nil, "docker", "start", c.Network); err != nil {
			return errors.New("could not start existing ClickHouse container")
		}
	}
	for {
		// Authenticate using the agent's retained secret (not a health check using
		// the container's own env), so stale/mismatched credentials fail closed.
		query := `<clickhouse><user>lidza</user><password>` + c.Password + `</password></clickhouse>`
		if dockerStream(ctx, strings.NewReader(query), io.Discard, nil, "exec", "-i", c.Network, "sh", "-c", `umask 077; f=$(mktemp); trap 'rm -f "$f"' EXIT; cat > "$f"; clickhouse-client --config-file "$f" --database telemetry --query 'SELECT 1'`) == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("authenticated ClickHouse did not become ready")
		case <-time.After(time.Second):
		}
	}
}
