package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/agim/lidza-deploy/internal/movetunnel"
)

// Replication through the control panel (the default path of a replicated
// database, move_replication.go). The destination's PostgreSQL connects to
// a listener of the destination agent; the panel keeps a few idle upgraded
// streams ("legs") open to that agent, and when the agent hands a
// connection to one of them the panel opens a stream to the source agent,
// which connects it to the database, and copies bytes both ways
// (internal/movetunnel). Neither agent reaches the other or the panel:
// every connection is the panel's, over the same agent channels as every
// other agent call (HTTPS to the agent's Caddy site or the SSH tunnel the
// panel maintains, authenticated with the agent's bearer token), so cloud
// firewalls that admit only HTTPS and SSH do not stand in the way.
//
// Each tunnel is a goroutine per replicated database that keeps
// moveTunnelIdleLegs idle legs, replaces dead ones (heartbeats on both
// ends), and stops at the teardown. The streams carry a token derived from
// the panel's key for the move and database, so a restarted panel derives
// the same tokens and resumes every tunnel from the move state (the runner
// on resume, the move tick for moves waiting at the cutover).

const (
	moveTunnelIdleLegs  = 2
	moveTunnelMaxActive = 16
	moveTunnelReadyWait = 20 * time.Second
	moveTunnelDialLimit = 30 * time.Second
)

// moveTunnel is the panel's tunnel of one replicated database.
type moveTunnel struct {
	move, sourceServer, targetServer string
	sourceDB, targetDB               string
	cancel                           context.CancelFunc
	done                             chan struct{}

	mu         sync.Mutex
	connecting int
	idle       int
	active     int
	failures   int
	lastError  string
	logged     time.Time
	ready      chan struct{}
	readyOnce  sync.Once
}

func moveTunnelKey(move, sourceDB string) string { return move + "/" + sourceDB }

// moveTunnelToken is the token the streams of one end carry: derived from
// the panel's key, so it needs no storage and survives a panel restart.
func (c *Control) moveTunnelToken(move, side, database string) string {
	mac := hmac.New(sha256.New, c.cfg.Key)
	mac.Write([]byte("lidza move tunnel\x00" + move + "\x00" + side + "\x00" + database))
	return hex.EncodeToString(mac.Sum(nil))
}

// moveTunnelStart runs the tunnel of a database unless it runs already.
func (c *Control) moveTunnelStart(move, sourceServer, targetServer, sourceDB, targetDB string) *moveTunnel {
	c.moveTunnelMu.Lock()
	defer c.moveTunnelMu.Unlock()
	if c.moveTunnels == nil {
		c.moveTunnels = map[string]*moveTunnel{}
	}
	key := moveTunnelKey(move, sourceDB)
	if t := c.moveTunnels[key]; t != nil {
		if t.targetDB == targetDB && t.sourceServer == sourceServer && t.targetServer == targetServer {
			return t
		}
		t.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &moveTunnel{move: move, sourceServer: sourceServer, targetServer: targetServer, sourceDB: sourceDB, targetDB: targetDB, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{})}
	c.moveTunnels[key] = t
	go t.run(ctx, c)
	return t
}

// moveTunnelStop ends a database's tunnel and waits for its streams to
// close.
func (c *Control) moveTunnelStop(move, sourceDB string) {
	c.moveTunnelMu.Lock()
	key := moveTunnelKey(move, sourceDB)
	t := c.moveTunnels[key]
	delete(c.moveTunnels, key)
	c.moveTunnelMu.Unlock()
	if t != nil {
		t.cancel()
		<-t.done
	}
}

// moveTunnelError is the last failure of a running tunnel, if any.
func (c *Control) moveTunnelError(move, sourceDB string) string {
	c.moveTunnelMu.Lock()
	t := c.moveTunnels[moveTunnelKey(move, sourceDB)]
	c.moveTunnelMu.Unlock()
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastError
}

// moveTunnelSync runs the tunnel of every database that replicates through
// the panel in a move that has not ended, and stops the rest: after a panel
// restart it resumes them from the move state.
func (c *Control) moveTunnelSync() {
	type want struct{ move, sourceServer, targetServer, sourceDB, targetDB string }
	desired := map[string]want{}
	c.mu.Lock()
	for id, job := range c.data.Moves {
		if terminalMoveState(job.State) {
			continue
		}
		for _, d := range job.Databases {
			if d.Link != nil && d.Link.Path == moveReplicationPathPanel && d.Link.Published && d.TargetID != "" && d.Replication == moveReplicationContinuous {
				desired[moveTunnelKey(id, d.SourceID)] = want{id, job.SourceServer, job.TargetServer, d.SourceID, d.TargetID}
			}
		}
	}
	c.mu.Unlock()
	c.moveTunnelMu.Lock()
	stale := []*moveTunnel{}
	for key, t := range c.moveTunnels {
		if _, ok := desired[key]; !ok {
			stale = append(stale, t)
		}
	}
	c.moveTunnelMu.Unlock()
	for _, t := range stale {
		c.moveTunnelStop(t.move, t.sourceDB)
	}
	for _, w := range desired {
		c.moveTunnelStart(w.move, w.sourceServer, w.targetServer, w.sourceDB, w.targetDB)
	}
}

// waitReady waits until a leg to the destination is open.
func (t *moveTunnel) waitReady(ctx context.Context, limit time.Duration) error {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-t.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		t.mu.Lock()
		detail := t.lastError
		t.mu.Unlock()
		if detail == "" {
			detail = "no answer"
		}
		return fmt.Errorf("the control panel opened no replication stream to %s within %s: %s", t.targetServer, moveDuration(limit), detail)
	}
}

func (t *moveTunnel) run(ctx context.Context, c *Control) {
	defer close(t.done)
	client := movetunnel.Client(c.client.Transport)
	events := make(chan struct{}, 4*moveTunnelIdleLegs)
	var legs sync.WaitGroup
	defer legs.Wait()
	for {
		t.mu.Lock()
		for t.connecting+t.idle < moveTunnelIdleLegs && t.active < moveTunnelMaxActive {
			t.connecting++
			legs.Add(1)
			go func() {
				defer legs.Done()
				t.leg(ctx, c, client)
				select {
				case events <- struct{}{}:
				default:
				}
			}()
		}
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-events:
		}
	}
}

// fail records a failure and words it once in the move's log while it
// repeats (again after five minutes).
func (t *moveTunnel) fail(c *Control, format string, args ...any) time.Duration {
	text := fmt.Sprintf(format, args...)
	t.mu.Lock()
	t.failures++
	backoff := min(time.Second<<min(t.failures-1, 5), 30*time.Second)
	report := text != t.lastError || time.Since(t.logged) > 5*time.Minute
	t.lastError = text
	if report {
		t.logged = time.Now()
	}
	t.mu.Unlock()
	if report {
		log.Printf("move %s: replication stream of %s: %s", t.move, t.sourceDB, text)
		if step := c.moveLiveStepKey(t.move); step != "" {
			c.moveLog(t.move, step, "Replication stream of %s through the control panel: %s.", t.sourceDB, text)
		}
	}
	return backoff
}

func (t *moveTunnel) pause(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// leg opens one idle leg to the destination, waits for a client and pipes
// it to a fresh stream to the source.
func (t *moveTunnel) leg(ctx context.Context, c *Control, client *http.Client) {
	opened := time.Now()
	conn, err := t.dial(ctx, c, client, t.targetServer, "/v1/move/databases/"+url.PathEscape(t.targetDB)+"/subscription/tunnel?move="+url.QueryEscape(t.move), c.moveTunnelToken(t.move, "destination", t.targetDB))
	if err != nil {
		backoff := t.fail(c, "the destination %s did not accept a stream: %v", t.targetServer, err)
		t.pause(ctx, backoff)
		t.mu.Lock()
		t.connecting--
		t.mu.Unlock()
		return
	}
	t.mu.Lock()
	t.connecting--
	t.idle++
	t.mu.Unlock()
	t.readyOnce.Do(func() { close(t.ready) })
	err = movetunnel.AwaitClient(ctx, conn)
	t.mu.Lock()
	t.idle--
	if err == nil {
		t.active++
	}
	t.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil && time.Since(opened) < 2*time.Second {
			// A leg the destination drops at once: do not reopen it in a
			// tight loop.
			t.pause(ctx, t.fail(c, "the destination %s closed the stream at once: %v", t.targetServer, err))
		}
		return
	}
	defer func() {
		t.mu.Lock()
		t.active--
		t.mu.Unlock()
	}()
	source, err := t.dial(ctx, c, client, t.sourceServer, "/v1/move/databases/"+url.PathEscape(t.sourceDB)+"/replication/stream?move="+url.QueryEscape(t.move), c.moveTunnelToken(t.move, "source", t.sourceDB))
	if err != nil {
		_ = movetunnel.Fail(conn)
		conn.Close()
		t.fail(c, "the source %s did not open a stream to the database: %v", t.sourceServer, err)
		return
	}
	if err := movetunnel.Ack(conn); err != nil {
		conn.Close()
		source.Close()
		return
	}
	t.mu.Lock()
	t.failures, t.lastError = 0, ""
	t.mu.Unlock()
	movetunnel.Pipe(ctx, conn, source)
}

// dial opens an upgraded stream to an agent route.
func (t *moveTunnel) dial(ctx context.Context, c *Control, client *http.Client, serverID, path, token string) (io.ReadWriteCloser, error) {
	server, err := c.moveServer(serverID)
	if err != nil {
		return nil, err
	}
	handshake, cancel := context.WithTimeout(ctx, moveTunnelDialLimit)
	defer cancel()
	stream, err := movetunnel.Dial(handshake, client, server.URL+path, server.Token, token)
	if err != nil {
		return nil, fmt.Errorf("%s", moveBounded(err.Error(), 300))
	}
	return stream, nil
}
