package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agim/lidza"
	"github.com/agim/lidza/packs/db"
)

// Failures, retries and their reporting.
//
// Every failed attempt of a step is classified:
//
//   - transient: the network, an agent that is restarting or upgrading, a
//     5xx answer, a busy resource, a timeout, a stalled transfer, a
//     database or ClickHouse connection error. The step is retried
//     automatically with exponential backoff (30s, 1m, 2m, 5m, 10m) up to
//     the move's attempt limit (six by default), and the log says
//     "attempt 2/6 in 1m: <reason>". Steps are idempotent and engines keep
//     checkpoints, so a retry continues where the last attempt stopped.
//   - operator: something only the operator can fix (disk space, a
//     permission, an authentication, an incompatible version, a capacity
//     limit, a checksum that mismatched twice). The move stops at once with
//     the precise message and what to do.
//   - fatal: anything else. The move stops; the operator retries or aborts.
//
// Each failure is recorded with the job (step, engine, server, the
// underlying error, classification, attempts and the next retry), shown in
// the move panel and the application list, written to the audit log, and
// sent through the application's error pipeline: an entry on the Errors
// page and, for a move that stopped, an operational alert.

const (
	moveClassTransient = "transient"
	moveClassOperator  = "operator"
	moveClassFatal     = "fatal"

	moveDefaultAttempts = 6
	moveMaxAttempts     = 10
	moveDetailLimit     = 600
)

var moveDefaultRetryDelays = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}

// MoveFailure is a step's last failure.
type MoveFailure struct {
	Step   string `json:"step"`
	Engine string `json:"engine,omitempty"` // postgresql shop, clickhouse events, files storage…
	Server string `json:"server,omitempty"`
	// Class is transient, operator or fatal; Code a short machine name of
	// the cause (disk_space, permission, agent_unavailable…).
	Class string `json:"class"`
	Code  string `json:"code"`
	// Message is the failure in one sentence; Detail the underlying error
	// text (bounded); Remediation what to do about it.
	Message     string     `json:"message"`
	Detail      string     `json:"detail,omitempty"`
	Remediation string     `json:"remediation,omitempty"`
	Attempt     int        `json:"attempt"`
	MaxAttempts int        `json:"max_attempts"`
	NextRetry   *time.Time `json:"next_retry,omitempty"`
	Time        time.Time  `json:"time"`
	// Diagnostics are what the agents reported when a stalled attempt was
	// cancelled.
	Diagnostics []string `json:"diagnostics,omitempty"`
	// Retained is the destination's retained data that stopped the Prepare
	// step (move_retained.go); the panel offers its removal.
	Retained *MoveRetainedData `json:"retained,omitempty"`
}

// moveError is an error with its classification. Engines return one for
// causes they know; everything else is classified from the error.
type moveError struct {
	class, code, remediation string
	err                      error
}

func (e *moveError) Error() string { return e.err.Error() }
func (e *moveError) Unwrap() error { return e.err }

// moveOperatorError is a failure only the operator can resolve.
func moveOperatorError(code, message, remediation string) error {
	return &moveError{class: moveClassOperator, code: code, remediation: remediation, err: errors.New(message)}
}

// moveTransientError is a failure a later attempt can overcome.
func moveTransientError(code string, err error) error {
	return &moveError{class: moveClassTransient, code: code, err: err}
}

// moveServerError attributes an error to a server.
type moveServerError struct {
	server string
	err    error
}

func (e *moveServerError) Error() string { return e.err.Error() }
func (e *moveServerError) Unwrap() error { return e.err }

// moveOn attributes err to server; nil stays nil.
func moveOn(server string, err error) error {
	if err == nil {
		return nil
	}
	var known *moveServerError
	if errors.As(err, &known) {
		return err
	}
	return &moveServerError{server: server, err: err}
}

var (
	errMoveStalled     = errors.New("the transfer made no progress")
	errMoveInterrupted = errors.New("the move was interrupted")
)

type moveRule struct {
	class, code string
	patterns    []string
	remediation string
}

// moveRules classify errors by their text, operator causes first: a
// permission problem inside a connection error is still a permission
// problem. {server} is replaced with the server's name.
var moveRules = []moveRule{
	{moveClassOperator, "disk_space", []string{"not enough free disk space", "free disk space", "disk reserve", "no space left", "insufficient storage", "keep free disk space"},
		"Free disk space on {server} (or add a disk), then retry the move."},
	{moveClassOperator, "permission", []string{"permission denied", "operation not permitted", "read-only file system", "access is denied"},
		"Fix the ownership or permissions of the path named in the error on {server}, then retry the move."},
	{moveClassOperator, "authentication", []string{"authentication failed", "password authentication", "authentication_failed", "not authorized", "unauthorized", "access denied", "agent returned 401", "agent returned 403", "invalid credentials"},
		"Check the credentials of the service on {server} (and the agent token of the server in the control panel), then retry the move."},
	{moveClassOperator, "version", []string{"version mismatch", "incompatible", "upgrade its agent", "upgrade the agent", "does not support", "not supported", "unsupported"},
		"Upgrade the agent or the service on {server} to a compatible version, then retry the move."},
	{moveClassOperator, "capacity", []string{"exceeds the transfer limit", "too large", "out of memory", "memory limit", "capacity"},
		"Raise the limit or capacity named in the error on {server}, or reduce the data, then retry the move."},
	{moveClassOperator, "databases_changed", []string{"databases changed during the move"},
		"Abort this move and start a new one, so the new database layout is planned and copied."},
	{moveClassTransient, "agent_unavailable", []string{"agent unavailable", "agent is unavailable", "connection refused", "connection reset", "broken pipe", "no route to host", "network is unreachable", "unexpected eof", "eof", "connection interrupted", "server closed", "restarting"},
		""},
	{moveClassTransient, "agent_upgrading", []string{"upgrade in progress", "agent upgrade"}, ""},
	{moveClassTransient, "busy", []string{"already running", "operation is already", "active operation", "wait for the application's active deployment", "is busy", "in progress", "free database slot", "locked"}, ""},
	{moveClassTransient, "timeout", []string{"timed out", "timeout", "deadline exceeded", "context canceled"}, ""},
	{moveClassTransient, "service_connection", []string{"could not connect", "connection to server", "clickhouse is not answering", "is not ready", "not ready", "could not be measured", "temporarily unavailable", "try again"}, ""},
	{moveClassTransient, "checksum", []string{"sha-256", "checksum", "digest"}, ""},
}

// classifyMoveError returns an error's class, code and remediation.
func classifyMoveError(err error) (class, code, remediation string) {
	var known *moveError
	if errors.As(err, &known) {
		return known.class, known.code, known.remediation
	}
	if errors.Is(err, errMoveStalled) {
		return moveClassTransient, "stalled", ""
	}
	if errors.Is(err, errMoveTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return moveClassTransient, "timeout", ""
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) {
		return moveClassTransient, "agent_unavailable", ""
	}
	var network net.Error
	if errors.As(err, &network) {
		return moveClassTransient, "network", ""
	}
	text := strings.ToLower(err.Error())
	var remote *agentHTTPError
	if errors.As(err, &remote) {
		switch {
		case remote.status == http.StatusInsufficientStorage:
			return moveClassOperator, "disk_space", moveRules[0].remediation
		case remote.status == http.StatusUnauthorized || remote.status == http.StatusForbidden:
			return moveClassOperator, "agent_auth", "The agent on {server} refused the control panel's token; pair the server again, then retry the move."
		case remote.status == http.StatusNotFound && remote.message == "":
			return moveClassOperator, "version", "The agent on {server} lacks a route the move needs; upgrade it, then retry the move."
		case remote.status == http.StatusRequestEntityTooLarge:
			return moveClassOperator, "capacity", moveRules[4].remediation
		}
		text = strings.ToLower(remote.message)
		if remote.status >= 500 && !moveOperatorText(text) {
			return moveClassTransient, "agent_" + strconv.Itoa(remote.status), ""
		}
	}
	for _, rule := range moveRules {
		for _, pattern := range rule.patterns {
			if strings.Contains(text, pattern) {
				return rule.class, rule.code, rule.remediation
			}
		}
	}
	return moveClassFatal, "step_failed", "Read the step's log for the cause; retry the move once it is fixed, or abort it."
}

func moveOperatorText(text string) bool {
	for _, rule := range moveRules {
		if rule.class != moveClassOperator {
			continue
		}
		for _, pattern := range rule.patterns {
			if strings.Contains(text, pattern) {
				return true
			}
		}
	}
	return false
}

func moveBounded(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return strings.ToValidUTF8(text[:limit], "") + "…"
}

// moveStepServer is the server a step works on when an error names none.
func moveStepServer(job MoveJob, step string) string {
	switch step {
	case moveStepPrepare, moveStepDeploy:
		return job.TargetServer
	}
	return job.SourceServer
}

// moveFailureFor classifies a failed attempt of step. previous is the
// step's earlier failure, if any, for the attempt count.
func (c *Control) moveFailureFor(job MoveJob, step, engine string, cause error) MoveFailure {
	class, code, remediation := classifyMoveError(cause)
	server := moveStepServer(job, step)
	var attributed *moveServerError
	if errors.As(cause, &attributed) {
		server = attributed.server
	}
	attempt := 1
	if f := job.Failure; f != nil && f.Step == step {
		attempt = f.Attempt + 1
		// A checksum that mismatches twice in a row is not the network.
		if code == "checksum" && f.Code == "checksum" {
			class, remediation = moveClassOperator, "The data changes between its export and its verification, or a disk on {server} corrupts it; check the disk and the source, then retry the move."
		}
	}
	limit := job.MaxAttempts
	if limit <= 0 {
		limit = moveDefaultAttempts
	}
	name := c.serverName(server)
	message := fmt.Sprintf("%s failed", moveStepTitle(step, step))
	if engine != "" {
		message += " (" + engine + ")"
	}
	message += " on " + name + ": " + moveBounded(moveAgentError(cause), 240)
	failure := MoveFailure{Step: step, Engine: engine, Server: server, Class: class, Code: code, Message: message, Detail: moveBounded(cause.Error(), moveDetailLimit),
		Remediation: strings.ReplaceAll(remediation, "{server}", name), Attempt: attempt, MaxAttempts: limit, Time: time.Now().UTC()}
	var held *moveRetainedError
	if errors.As(cause, &held) {
		retained := held.retained
		failure.Retained = &retained
	}
	return failure
}

// moveRetryDelay is the wait before attempt (2 for the first retry).
func (c *Control) moveRetryDelay(attempt int) time.Duration {
	delays := c.moveRetryDelays
	if len(delays) == 0 {
		delays = moveDefaultRetryDelays
	}
	index := min(max(attempt-2, 0), len(delays)-1)
	return delays[index]
}

func moveDuration(d time.Duration) string {
	switch {
	case d >= time.Minute && d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d >= time.Second:
		return d.Round(time.Second).String()
	}
	return d.String()
}

// moveStepTitle is a step's current title; titles of recorded jobs follow
// the current wording.
func moveStepTitle(key, fallback string) string {
	for _, s := range moveStepTitles {
		if s.key == key {
			return s.title
		}
	}
	return fallback
}

// reportMoveFailure sends a failure through the audit log and the
// application's error pipeline. stopped marks a move that stopped (not a
// failure that waits for its retry): it also raises an operational alert.
func (c *Control) reportMoveFailure(ctx context.Context, job MoveJob, failure MoveFailure, stopped bool) {
	meta := map[string]string{"step": failure.Step, "class": failure.Class, "code": failure.Code, "attempt": strconv.Itoa(failure.Attempt), "server": failure.Server}
	if failure.Engine != "" {
		meta["engine"] = failure.Engine
	}
	moveAuditMeta(ctx, job, "step."+failure.Step, true, meta)
	c.recordMoveAppError(ctx, job, failure)
	if stopped {
		_ = c.observe(context.WithoutCancel(ctx), "move:"+job.AppID, "Moving "+job.AppID+" to "+c.serverName(job.TargetServer)+" stopped: "+failure.Message, true, 1)
	}
}

// clearMoveIncident resolves the move's operational alert.
func (c *Control) clearMoveIncident(ctx context.Context, job MoveJob) {
	c.mu.Lock()
	incident, open := c.data.Incidents["move:"+job.AppID]
	c.mu.Unlock()
	if open && (incident.Active || incident.Failures > 0) {
		_ = c.observe(context.WithoutCancel(ctx), "move:"+job.AppID, incident.Message, false, 1)
	}
}

// recordMoveAppError adds the failure to the application's Errors page,
// grouped by step and cause.
func (c *Control) recordMoveAppError(ctx context.Context, job MoveJob, failure MoveFailure) {
	store, ok := lidza.Optional[*db.DB](ctx)
	if !ok || store == nil || store.Pool == nil {
		return
	}
	c.mu.Lock()
	live, found := c.data.Apps[job.AppID]
	c.mu.Unlock()
	if !found {
		return
	}
	sum := sha256.Sum256([]byte("move:" + job.ID + ":" + failure.Step + ":" + strconv.Itoa(failure.Attempt) + ":" + failure.Time.Format(time.RFC3339Nano)))
	v := hex.EncodeToString(sum[:16])
	id := fmt.Sprintf("%s-%s-%s-%s-%s", v[:8], v[8:12], v[12:16], v[16:20], v[20:])
	fingerprint := "move:" + failure.Step + ":" + failure.Code
	stack := failure.Detail
	if failure.Remediation != "" {
		stack += "\n\n" + failure.Remediation
	}
	route := "move " + job.ID
	extra, _ := json.Marshal(map[string]any{"deploy_server": live.ServerID, "deploy_app": live.ID, "deploy_generation": live.Generation, "container": "move", "release": job.ID,
		"move": map[string]any{"step": failure.Step, "engine": failure.Engine, "server": failure.Server, "class": failure.Class, "code": failure.Code, "attempt": failure.Attempt}})
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = store.Pool.Exec(write, `INSERT INTO app_error(id,source,message,stack,route,method,request_id,fingerprint,extra,created_at) VALUES($1,'server',$2,$3,$4,NULL,NULL,$5,$6,$7) ON CONFLICT(id) DO NOTHING`,
		id, failure.Message, stack, route, fingerprint, extra, failure.Time)
}
