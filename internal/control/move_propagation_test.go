package control

import (
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// moveCacheFake answers the propagation wait's TTL queries.
type moveCacheFake struct {
	mu      sync.Mutex
	ttl     time.Duration
	answers map[string][]MoveResolverAnswer
}

func (f *moveCacheFake) AuthoritativeTTL(context.Context, string) (time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ttl, nil
}

func (f *moveCacheFake) PublicAnswers(_ context.Context, host string) []MoveResolverAnswer {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []MoveResolverAnswer{}
	for _, a := range f.answers[host] {
		a.Addresses = append([]string{}, a.Addresses...)
		out = append(out, a)
	}
	return out
}

func (f *moveCacheFake) set(host string, answers ...MoveResolverAnswer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[host] = answers
}

func cachedAt(address string, ttl int) []MoveResolverAnswer {
	return []MoveResolverAnswer{
		{Resolver: "1.1.1.1", Addresses: []string{address}, TTL: ttl},
		{Resolver: "8.8.8.8", Addresses: []string{moveTestTargetIP}, TTL: 3600},
		{Resolver: "9.9.9.9", Error: "timeout"},
	}
}

func TestEvaluatePropagation(t *testing.T) {
	changed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	p := MoveDNSPropagation{OldTTL: 3600, Changed: &changed, Resolvers: []MoveResolverAnswer{
		{Resolver: "1.1.1.1", Hostname: "deploy.example.com", Addresses: []string{moveTestSourceIP}, TTL: 51 * 60},
		{Resolver: "8.8.8.8", Hostname: "deploy.example.com", Addresses: []string{moveTestTargetIP}, Ready: true},
	}}
	done, detail := evaluatePropagation(p, changed.Add(2*time.Minute))
	if done || detail != "Waiting for cached DNS to expire: about 52 minutes left (resolver 1.1.1.1 still returns the old address "+moveTestSourceIP+" for deploy.example.com)." {
		t.Fatalf("%v %q", done, detail)
	}
	// The old TTL and the margin bound the wait even if a resolver lags.
	if done, detail := evaluatePropagation(p, changed.Add(65*time.Minute)); !done || !strings.Contains(detail, "TTL") {
		t.Fatalf("after the TTL: %v %q", done, detail)
	}
	if done, _ := evaluatePropagation(p, changed.Add(64*time.Minute)); done {
		t.Fatal("done before the TTL and margin")
	}
	p.Resolvers[0].Addresses, p.Resolvers[0].Ready = []string{moveTestTargetIP}, true
	if done, _ := evaluatePropagation(p, changed.Add(2*time.Minute)); !done {
		t.Fatal("not done with every resolver on the destination")
	}
	// Nobody answered: the TTL alone decides.
	p.Resolvers = []MoveResolverAnswer{{Resolver: "1.1.1.1", Error: "timeout"}}
	if done, detail := evaluatePropagation(p, changed.Add(time.Minute)); done || !strings.Contains(detail, "no public resolver answered") {
		t.Fatalf("unanswered: %v %q", done, detail)
	}
}

func TestPanelRetireWaitsForCachedDNS(t *testing.T) {
	fx := newPanelFixture(t)
	c := fx.c
	caches := &moveCacheFake{ttl: time.Hour, answers: map[string][]MoveResolverAnswer{}}
	caches.set("deploy.example.com", cachedAt(moveTestSourceIP, 3000)...)
	c.moveCaches = caches
	c.panelPollInterval = time.Hour // checks run only when asked
	if code, body := panelPost(t, c, "", PanelMoveInput{Target: "combined"}); code != 202 {
		t.Fatal(code, body)
	}
	move := waitPanel(t, c, panelMoveStandby, panelMoveFailed)
	if move.Plan.DNSTTL != 3600 || move.Propagation == nil || move.Propagation.OldTTL != 3600 {
		t.Fatalf("plan TTL %d propagation %+v", move.Plan.DNSTTL, move.Propagation)
	}
	panelPost(t, c, "cutover", nil)
	waitPanel(t, c, panelMoveDNS)
	waitPanelFirstCheck(t, c)
	// DNS changed at the zone's nameservers; 1.1.1.1 still has the old one.
	fx.resolver.set("deploy.example.com", moveTestTargetIP)
	fx.probe.set(true)
	if c.panelCheckDNS(context.Background(), false) {
		t.Fatal("retired while a public resolver still returns the old address")
	}
	move, _ = c.panelMove()
	retire := move.step(panelStepRetire)
	if move.State != panelMoveDNS || move.step(panelStepDNS).Status != "done" || retire.Status != "waiting" ||
		!strings.Contains(retire.Detail, "Waiting for cached DNS to expire: about 51 minutes left (resolver 1.1.1.1 still returns the old address") {
		t.Fatalf("state %s steps %+v", move.State, move.Steps)
	}
	if fx.source.called("POST /v1/host/control/retire") != 0 {
		t.Fatal("the old host was retired")
	}
	if items := c.panelMoveActivity(time.Now()); len(items) != 1 || !strings.Contains(items[0].WaitingOn, "cached DNS") {
		t.Fatalf("activity %+v", items)
	}
	// The resolver catches up: the panel retires and the old host forwards.
	caches.set("deploy.example.com", cachedAt(moveTestTargetIP, 3600)...)
	if !c.panelCheckDNS(context.Background(), false) {
		t.Fatal("not retired once every resolver returns the destination")
	}
	if len(fx.source.retired) != 1 || fx.source.retired[0]["forward_to"] != moveTestTargetIP || fx.source.retired[0]["hostname"] != "deploy.example.com" {
		t.Fatalf("retire %v", fx.source.retired)
	}
	move, _ = c.panelMove()
	if move.State != panelMoveDone || !strings.Contains(strings.Join(move.RetireDetail, " "), "forwards to Combined ("+moveTestTargetIP+") for 7 days") {
		t.Fatalf("done %s notes %v", move.State, move.RetireDetail)
	}
}

func TestPanelRetireAfterTheOldTTL(t *testing.T) {
	fx := newPanelFixture(t)
	c := fx.c
	caches := &moveCacheFake{ttl: 10 * time.Minute, answers: map[string][]MoveResolverAnswer{}}
	caches.set("deploy.example.com", cachedAt(moveTestSourceIP, 3000)...) // a lagging resolver
	c.moveCaches = caches
	c.panelPollInterval = time.Hour
	panelPost(t, c, "", PanelMoveInput{Target: "combined"})
	waitPanel(t, c, panelMoveStandby)
	panelPost(t, c, "cutover", nil)
	waitPanel(t, c, panelMoveDNS)
	waitPanelFirstCheck(t, c)
	fx.resolver.set("deploy.example.com", moveTestTargetIP)
	fx.probe.set(true)
	if c.panelCheckDNS(context.Background(), false) {
		t.Fatal("retired at once")
	}
	// The change was seen 16 minutes ago: the 10 minute TTL and the margin
	// have passed.
	c.updatePanelMove(func(m *PanelMove) error {
		at := m.Propagation.Changed.Add(-16 * time.Minute)
		m.Propagation.Changed = &at
		return nil
	})
	if !c.panelCheckDNS(context.Background(), false) {
		t.Fatal("not retired after the old TTL")
	}
	if fx.source.called("POST /v1/host/control/retire") != 1 {
		t.Fatal("the old host was not asked to retire")
	}
}

func TestStateFlushAfterRetireReportsNothing(t *testing.T) {
	var w stateWriter
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	// Nothing pending: an expired shutdown context is no failure.
	for range 50 {
		if err := w.flush(expired); err != nil {
			t.Fatalf("idle flush: %v", err)
		}
	}
}

func TestFlushStateSkipsTheRetiredPanelsFailure(t *testing.T) {
	fx := newPanelFixture(t)
	c := fx.c
	c.mu.Lock()
	c.data.PanelMove = &PanelMove{ID: "m", State: panelMoveDone, ReadOnly: true}
	c.mu.Unlock()
	release := make(chan struct{})
	oldSave := saveState
	saveState = func(string, any) error { <-release; return nil }
	t.Cleanup(func() {
		close(release)
		// The writer goroutine called saveState; restore the seam only once
		// it has finished, or the restore races with that call.
		if err := c.stateWriter.flush(context.Background()); err != nil {
			t.Error(err)
		}
		saveState = oldSave
	})
	c.stateWriter.submit(c.path(), "sealed") // a write that does not finish in time
	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	expired, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	FlushState(expired)
	if strings.Contains(logged.String(), "final write failed") {
		t.Fatalf("logged %q", logged.String())
	}
}

func TestMoveFinishKeepsTheSourceUntilCachedDNSExpires(t *testing.T) {
	fx := newMoveFixture(t)
	rep := fx.replicationSource()
	caches := &moveCacheFake{ttl: time.Hour, answers: map[string][]MoveResolverAnswer{}}
	caches.set("shop.example.com", cachedAt(moveTestSourceIP, 1800)...)
	caches.set("www.shop.example.com", cachedAt(moveTestTargetIP, 1800)...)
	fx.c.moveCaches = caches
	status, view := startMove(t, fx.c, "shop", map[string]any{"target_server": "beta", "acknowledge_warnings": true})
	if status != 202 {
		t.Fatal(status)
	}
	waitMove(t, fx.c, view.ID, moveStateCutover)
	rep.with(func() { rep.sourceLSN = "0/5000900" })
	fx.resolver.set("shop.example.com", moveTestTargetIP)
	fx.resolver.set("www.shop.example.com", moveTestTargetIP)
	fx.probe.set(true)
	fx.c.moveTick(context.Background())
	var job MoveJob
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		var running bool
		job, _, running = moveSnapshot(fx.c, view.ID)
		if job.Switched && job.State == moveStateFinishing && !running {
			break
		}
	}
	if !job.Switched || job.State != moveStateFinishing || fx.a.called("DELETE /v1/apps/shop") {
		t.Fatalf("state %s switched %v; the source was retired while 1.1.1.1 still returns it", job.State, job.Switched)
	}
	if !moveLogged(job, moveStepFinish, "Waiting for cached DNS to expire: about 31 minutes left (resolver 1.1.1.1 still returns the old address "+moveTestSourceIP+" for shop.example.com).") {
		t.Fatalf("finish log %+v", job.step(moveStepFinish).Log)
	}
	caches.set("shop.example.com", cachedAt(moveTestTargetIP, 1800)...)
	fx.c.moveTick(context.Background())
	job = waitMove(t, fx.c, view.ID, moveStateFinished)
	if !fx.a.called("DELETE /v1/apps/shop") || job.step(moveStepFinish).Status != "done" {
		t.Fatalf("not retired after the caches caught up: %+v", job.Steps)
	}
}
