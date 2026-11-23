package agent

// Repairing the host's PostgreSQL before applications need it. A cluster
// that should run but cannot is the commonest reason a native application
// cannot be created or moved to a server. The agent used to stop with "the
// matching PostgreSQL cluster is stopped, a standby or not owned by
// postgres". It now says which of these it is, and repairs the case that has
// one safe answer: a cluster whose service failed. A cluster someone stopped
// on purpose, a standby and a cluster with another owner are reported and
// left alone.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// pgProblem is why the cluster applications would use is not usable.
type pgProblem struct {
	Cluster pgCluster
	// Summary is a full sentence for people.
	Summary string
	// Fix is "access" (the postgres account cannot reach the data
	// directory), "start" (the service failed and can be started) or empty
	// when the agent must not decide.
	Fix string
}

func (p *pgProblem) Error() string { return p.Summary }

func pgClusterLabel(c pgCluster) string {
	return fmt.Sprintf("PostgreSQL %d cluster %q (port %d)", c.Major, c.Name, c.Port)
}

func pgClusterUnit(c pgCluster) string {
	return "postgresql@" + strconv.Itoa(c.Major) + "-" + c.Name + ".service"
}

// closedToPostgres returns the first directory on the way to dir, or dir
// itself, that the postgres account cannot enter.
func (h nativeHostData) closedToPostgres(ctx context.Context, dir string) string {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	path := ""
	for _, part := range parts {
		path += "/" + part
		if _, err := h.run(ctx, "", "/usr/sbin/runuser", "-u", "postgres", "--", "/usr/bin/test", "-x", path); err != nil {
			return path
		}
	}
	return ""
}

// diagnosePostgres explains why no cluster is usable; nil when one is, or
// when the host has no matching cluster at all (one is then installed).
func (h nativeHostData) diagnosePostgres(ctx context.Context) *pgProblem {
	version, port := h.cfg.PostgresVersion, h.cfg.PostgresPort
	clusters, _ := h.listClusters(ctx)
	if _, err := chooseNativePGCluster(clusters, version, port); err == nil || errors.Is(err, errNativePGNoCluster) {
		return nil
	}
	for _, c := range clusters {
		if version != 0 && c.Major != version || port != 0 && c.Port != port {
			continue
		}
		label := pgClusterLabel(c)
		switch {
		case c.Owner != "postgres":
			return &pgProblem{Cluster: c, Summary: label + " is owned by " + c.Owner + ", not by postgres; it is left unchanged."}
		case c.Online && c.Recovery:
			return &pgProblem{Cluster: c, Summary: label + " is a standby (in recovery) and cannot take new databases; it is left unchanged."}
		case c.Online:
			continue
		}
		state, _ := h.run(ctx, "", "/usr/bin/systemctl", "show", "-p", "ActiveState", "--value", pgClusterUnit(c))
		state = strings.TrimSpace(state)
		closed := h.closedToPostgres(ctx, c.DataDir)
		switch {
		case state != "failed":
			return &pgProblem{Cluster: c, Summary: label + " is stopped and its service did not fail, so it was stopped on purpose; start it (systemctl start " + pgClusterUnit(c) + ") or remove it. It is left unchanged."}
		case closed != "":
			return &pgProblem{Cluster: c, Fix: "access", Summary: label + " cannot start: the postgres account cannot enter " + closed + ", which leads to its data directory " + c.DataDir + "."}
		default:
			return &pgProblem{Cluster: c, Fix: "start", Summary: label + " is down: its service failed."}
		}
	}
	return nil
}

// openForPostgres lets the postgres account enter one directory and nothing
// more: an access-control entry for that account where the tool exists,
// otherwise the search bit for others. Neither lets anyone list or read it.
func (h nativeHostData) openForPostgres(ctx context.Context, dir string) error {
	if h.fileExists("/usr/bin/setfacl") {
		if _, err := h.run(ctx, "", "/usr/bin/setfacl", "-m", "u:postgres:x", dir); err == nil {
			return nil
		}
	}
	if out, err := h.run(ctx, "", "/usr/bin/chmod", "o+x", dir); err != nil {
		return fmt.Errorf("%s could not be opened for the postgres account: %s", dir, nativeTrimOutput(out))
	}
	return nil
}

// repairPostgres applies the fix of a diagnosed problem and confirms that a
// cluster is usable afterwards. It returns what it did.
func (h nativeHostData) repairPostgres(ctx context.Context, problem *pgProblem) (string, error) {
	if problem.Fix == "" {
		return "", problem
	}
	c := problem.Cluster
	done := []string{}
	if problem.Fix == "access" {
		for range 8 {
			closed := h.closedToPostgres(ctx, c.DataDir)
			if closed == "" {
				break
			}
			if err := h.openForPostgres(ctx, closed); err != nil {
				return "", err
			}
			done = append(done, "let the postgres account enter "+closed)
		}
		if closed := h.closedToPostgres(ctx, c.DataDir); closed != "" {
			return "", fmt.Errorf("%s still cannot start: the postgres account cannot enter %s", pgClusterLabel(c), closed)
		}
	}
	unit := pgClusterUnit(c)
	_, _ = h.run(ctx, "", "/usr/bin/systemctl", "reset-failed", unit)
	if out, err := h.run(ctx, "", "/usr/bin/systemctl", "start", unit); err != nil {
		tail, _ := h.run(ctx, "", "/usr/bin/journalctl", "-u", unit, "-n", "5", "--no-pager", "-o", "cat")
		return "", fmt.Errorf("%s did not start: %s", pgClusterLabel(c), nativeTrimOutput(strings.TrimSpace(out+"\n"+tail)))
	}
	clusters, _ := h.listClusters(ctx)
	if _, err := chooseNativePGCluster(clusters, h.cfg.PostgresVersion, h.cfg.PostgresPort); err != nil {
		return "", fmt.Errorf("%s was started but is still not usable", pgClusterLabel(c))
	}
	done = append(done, "started "+pgClusterLabel(c))
	return strings.Join(done, "; "), nil
}
