package agentlink

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	Heartbeat, IdleTimeout = 20*time.Millisecond, 500*time.Millisecond
	m.Run()
}

const testToken = "0123456789abcdef0123456789abcdef"

func panelServer(t *testing.T, hub *Hub) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != Path || r.Header.Get(ServerHeader) != "old" || !Authorized(r, testToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		hub.Serve(w, r, "old")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func linkClient(hub *Hub) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if id, ok := ServerOf(addr); ok {
			return hub.Dial(ctx, id)
		}
		return nil, fmt.Errorf("unexpected dial %s", addr)
	}}}
}

func waitIdle(t *testing.T, hub *Hub, n int) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if hub.Idle("old") >= n {
			return
		}
	}
	t.Fatalf("idle links %d, want %d", hub.Idle("old"), n)
}

func TestLinkCarriesAgentCalls(t *testing.T) {
	hub := NewHub()
	defer hub.Close()
	panel := panelServer(t, hub)
	var calls atomic.Int32
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer agent-call" {
			w.WriteHeader(401)
			return
		}
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s %s", r.Method, r.URL.Path, body)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Agent{URL: panel.URL, Server: "old", Token: testToken, Handler: api, Idle: 2}.Run(ctx)
	waitIdle(t, hub, 2)
	time.Sleep(100 * time.Millisecond) // pings flow both ways meanwhile
	client := linkClient(hub)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", URL("old")+"/v1/apps", strings.NewReader(fmt.Sprint(i)))
			req.Header.Set("Authorization", "Bearer agent-call")
			res, err := client.Do(req)
			if err != nil {
				errs <- err
				return
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if string(body) != fmt.Sprintf("POST /v1/apps %d", i) {
				errs <- fmt.Errorf("body %q", body)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if calls.Load() != 8 {
		t.Fatalf("calls %d", calls.Load())
	}
	// The agent keeps idle links open after the calls.
	waitIdle(t, hub, 2)
}

func TestLinkRefusesAWrongToken(t *testing.T) {
	hub := NewHub()
	defer hub.Close()
	panel := panelServer(t, hub)
	_, err := Agent{URL: panel.URL, Server: "old", Token: strings.Repeat("x", 32)}.dial(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("dial: %v", err)
	}
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := hub.Dial(short, "old"); err == nil {
		t.Fatal("a call found a link")
	}
}

func TestLinkReconnectsAfterThePanelDropsIt(t *testing.T) {
	hub := NewHub()
	panel := panelServer(t, hub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Agent{URL: panel.URL, Server: "old", Token: testToken, Handler: http.NotFoundHandler(), Idle: 1}.Run(ctx)
	waitIdle(t, hub, 1)
	hub.mu.Lock()
	for _, l := range hub.idle["old"] {
		l.conn.Close() // the panel restarts
	}
	hub.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	waitIdle(t, hub, 1)
}

func TestServerOf(t *testing.T) {
	if id, ok := ServerOf("old.agent-link.invalid:80"); !ok || id != "old" {
		t.Fatal(id, ok)
	}
	if _, ok := ServerOf("agent.example.com:443"); ok {
		t.Fatal("a normal host is a link")
	}
}
