package control

// Agent links (internal/agentlink): servers whose agent no other server
// reaches connect out to this panel's public HTTPS address, and the panel
// sends their agent calls back through those connections. A server so
// reached has the URL http://<id>.agent-link.invalid; the panel's HTTP
// transport dials a link for it instead of the network. Both the active
// panel and a standby copy accept links, so a panel move can verify the
// link to the destination before its cutover.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/agentlink"
)

// linkTransport is the default transport, with link hosts dialed through
// hub.
func linkTransport(hub *agentlink.Hub) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if id, ok := agentlink.ServerOf(address); ok {
			return hub.Dial(ctx, id)
		}
		return dial(ctx, network, address)
	}
	return transport
}

// agentLinkHandlers serves the link routes for the servers lookup returns:
// GET agentlink.Path takes a link, POST agentlink.ProbePath calls the agent
// through one (the panel move's check before its cutover).
func agentLinkHandlers(hub *agentlink.Hub, client *http.Client, lookup func(id string) (Server, bool)) (link, probe http.HandlerFunc) {
	authorize := func(w http.ResponseWriter, r *http.Request) (Server, bool) {
		id := r.Header.Get(agentlink.ServerHeader)
		s, ok := lookup(id)
		if !ok || s.URL != agentlink.URL(s.ID) || !agentlink.Authorized(r, s.Token) {
			agent.Fail(w, http.StatusUnauthorized, errors.New("unknown server or token"))
			return Server{}, false
		}
		return s, true
	}
	link = func(w http.ResponseWriter, r *http.Request) {
		if s, ok := authorize(w, r); ok {
			hub.Serve(w, r, s.ID)
		}
	}
	probe = func(w http.ResponseWriter, r *http.Request) {
		s, ok := authorize(w, r)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		var status agent.ControlHostStatus
		if err := linkCall(ctx, client, s, "/v1/host/control", &status); err != nil {
			agent.JSON(w, 200, map[string]any{"reachable": false, "detail": err.Error()})
			return
		}
		agent.JSON(w, 200, map[string]any{"reachable": true, "machine_id": status.MachineID})
	}
	return link, probe
}

// linkCall is one authenticated GET through a link.
func linkCall(ctx context.Context, client *http.Client, s Server, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return &agentHTTPError{status: res.StatusCode}
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// agentLinkRoutes registers the link routes on the active panel.
func (c *Control) agentLinkRoutes(mux *http.ServeMux) {
	link, probe := agentLinkHandlers(c.links, c.client, c.serverEntry)
	mux.HandleFunc("GET "+agentlink.Path, link)
	mux.HandleFunc("POST "+agentlink.ProbePath, probe)
}

// standbyLinks serves the link routes on a standby copy, for the servers
// of its seed file (the move wrote it with the new panel's server list).
func standbyLinks(serversFile string) (link, probe http.HandlerFunc) {
	hub := agentlink.NewHub()
	client := &http.Client{Transport: linkTransport(hub), Timeout: 30 * time.Second}
	return agentLinkHandlers(hub, client, func(id string) (Server, bool) {
		raw, err := os.ReadFile(serversFile)
		if err != nil {
			return Server{}, false
		}
		var servers []Server
		if json.Unmarshal(raw, &servers) != nil {
			return Server{}, false
		}
		for _, s := range servers {
			if s.ID == id {
				return s, true
			}
		}
		return Server{}, false
	})
}

// panelLinkSource connects the kept source host's agent to the panel on
// the destination: the agent dials the destination's address with the
// panel's hostname (DNS still points here), and the standby panel there
// answers a call through the link before the cutover.
func (c *Control) panelLinkSource(ctx context.Context, move PanelMove) (string, bool, error) {
	if !move.Plan.SourceLink || move.Source == "" {
		return "No server is kept through an agent link.", true, nil
	}
	source, ok := c.serverEntry(move.Source)
	if !ok {
		return "", false, errors.New("this panel's own server left the server list")
	}
	address := panelForwardAddress(move)
	link := agent.ControlLink{URL: c.cfg.PublicURL, Address: address, Server: move.Source}
	if err := c.moveCall(ctx, time.Minute, move.Source, http.MethodPut, "/v1/host/control/link", link, nil); err != nil {
		return "", false, fmt.Errorf("%s's agent did not take the link: %w", move.Source, err)
	}
	kept := Server{ID: source.ID, URL: agentlink.URL(source.ID), Token: source.Token}
	var last error
	for attempt := 0; attempt < 20; attempt++ {
		if last = c.panelProbeLink(ctx, address, move.Hostname, kept); last == nil {
			return fmt.Sprintf("%s's agent connects out to the panel on %s (%s) and answered through the link; it stays a server of the new panel.", cmpOrString(move.Plan.SourceName, move.Source), move.TargetName, address), false, nil
		}
		if sleepContext(ctx, c.panelLinkWait()) != nil {
			return "", false, ctx.Err()
		}
	}
	return "", false, fmt.Errorf("the panel on %s does not reach %s through its link: %w", move.TargetName, move.Source, last)
}

func (c *Control) panelLinkWait() time.Duration {
	if c.panelPollInterval > 0 && c.panelPollInterval < 3*time.Second {
		return c.panelPollInterval
	}
	return 3 * time.Second
}

// panelProbeLink asks the panel at address (as hostname) to call server's
// agent through a link.
func (c *Control) panelProbeLink(ctx context.Context, address, hostname string, server Server) error {
	if c.panelLinkProbe != nil {
		return c.panelLinkProbe(ctx, address, hostname, server)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, net.JoinHostPort(address, "443"))
		},
		TLSClientConfig: &tls.Config{ServerName: hostname, MinVersion: tls.VersionTLS12}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+hostname+agentlink.ProbePath, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+server.Token)
	req.Header.Set(agentlink.ServerHeader, server.ID)
	res, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var out struct {
		Reachable bool   `json:"reachable"`
		Detail    string `json:"detail"`
		Error     string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&out)
	switch {
	case res.StatusCode != http.StatusOK:
		return fmt.Errorf("HTTP %d %s", res.StatusCode, out.Error)
	case !out.Reachable:
		return errors.New(cmpOrString(out.Detail, "not reachable"))
	}
	return nil
}
