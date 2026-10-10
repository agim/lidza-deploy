package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const cacheImage = "valkey/valkey:8-alpine"

func provisionLocalCache(ctx context.Context, c CacheResource) error {
	for _, kind := range []string{"network", "volume"} {
		name := c.Network
		if kind == "volume" {
			name += "-data"
		}
		found, err := command(ctx, "", nil, "docker", kind, "ls", "--filter", "name=^"+name+"$", "--format", "{{.Name}}")
		if err != nil {
			return err
		}
		if found == "" {
			if _, err = command(ctx, "", nil, "docker", kind, "create", "--label", "io.lidza.cache="+c.Network, name); err != nil {
				return err
			}
		} else {
			label, e := command(ctx, "", nil, "docker", kind, "inspect", "--format", `{{index .Labels "io.lidza.cache"}}`, name)
			if e != nil || label != c.Network {
				return errors.New("cache resource name is already occupied")
			}
		}
	}
	found, err := command(ctx, "", nil, "docker", "ps", "-a", "--filter", "name=^/"+c.Network+"$", "--format", "{{.ID}}")
	if err != nil {
		return err
	}
	if found == "" {
		// Configuration arrives on stdin, not argv, an environment variable, or a
		// public bind-mounted file. The cache runs as the same unprivileged UID as apps.
		cfg := fmt.Sprintf("bind 0.0.0.0\nprotected-mode yes\nport 6379\nrequirepass %s\ndir /data\nappendonly yes\nappendfsync everysec\nsave \"\"\nmaxmemory 128mb\nmaxmemory-policy allkeys-lru\n", c.LocalPassword)
		seed := []string{"run", "--rm", "-i", "--network", "none", "--user", "0:0", "--read-only", "--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER", "--security-opt", "no-new-privileges", "--memory", "128m", "--pids-limit", "32", "--mount", "type=volume,src=" + c.Network + "-data,dst=/data", cacheImage, "sh", "-c", "umask 077; cat > /data/lidza.conf && chown -R 65532:65532 /data && chmod 700 /data"}
		if err = dockerStream(ctx, strings.NewReader(cfg), io.Discard, nil, seed...); err != nil {
			return errors.New("could not initialize cache configuration")
		}
		_, err = command(ctx, "", nil, "docker", "run", "-d", "--name", c.Network, "--label", "io.lidza.cache="+c.Network, "--network", c.Network, "--restart", "unless-stopped", "--user", "65532:65532", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "256m", "--cpus", "0.5", "--pids-limit", "64", "--log-opt", "max-size=5m", "--log-opt", "max-file=2", "--mount", "type=volume,src="+c.Network+"-data,dst=/data", cacheImage, "valkey-server", "/data/lidza.conf")
		if err != nil {
			return errors.New("could not start private Valkey container")
		}
	} else {
		label, e := command(ctx, "", nil, "docker", "inspect", "--format", `{{index .Config.Labels "io.lidza.cache"}}`, c.Network)
		if e != nil || label != c.Network {
			return errors.New("cache container name is already occupied")
		}
		if _, err = command(ctx, "", nil, "docker", "start", c.Network); err != nil {
			return err
		}
	}
	for i := 0; i < 30; i++ {
		out, e := command(ctx, "", []string{"REDISCLI_AUTH=" + c.LocalPassword}, "docker", "exec", "--env", "REDISCLI_AUTH", c.Network, "valkey-cli", "PING")
		if e == nil && out == "PONG" {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("cache startup cancelled")
		case <-time.After(time.Second):
		}
	}
	return errors.New("authenticated cache did not become ready")
}
