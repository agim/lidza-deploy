package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agim/lidza"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza/packs/audit"
)

// Redirect domains of an application (agent/redirects.go): hostnames that
// answer with a redirect to one of its hostnames or any other. The agent
// hosting the application stores and serves them; the panel keeps a copy on
// the application record for fleet-wide conflict checks and moves.

// redirectsView is the panel's view of an application's redirect domains.
type redirectsView struct {
	agent.RedirectView
	ServerID string `json:"server_id"`
	// Addresses are the public addresses of the application's server that
	// each redirect domain's A/AAAA records point to.
	Addresses []string `json:"addresses"`
	// Records are the DNS records to set for the redirect domains.
	Records  []MoveDNSRecord `json:"records"`
	Warnings []string        `json:"warnings"`
}

// redirectHostOwner returns the application other than except that serves
// or redirects one of hosts, and the hostname.
func redirectHostOwner(apps map[string]Application, hosts []string, except string) (string, string) {
	for id, a := range apps {
		if id == except || a.Retiring {
			continue
		}
		for _, host := range hosts {
			if a.Domain == host || slices.Contains(a.AdditionalDomains, host) || slices.ContainsFunc(a.RedirectDomains, func(d agent.RedirectDomain) bool { return d.Host == host }) {
				return id, host
			}
		}
	}
	return "", ""
}

// redirectedHostOwner returns the application that redirects one of hosts,
// for checks of new application hostnames.
func redirectedHostOwner(apps map[string]Application, hosts []string) string {
	for id, a := range apps {
		for _, d := range a.RedirectDomains {
			if slices.Contains(hosts, d.Host) {
				return id
			}
		}
	}
	return ""
}

func applicationHostnames(a Application) []string {
	return append([]string{a.Domain}, a.AdditionalDomains...)
}

// redirectWarnings names redirects to hostnames the application does not
// serve: allowed, but worth a second look.
func redirectWarnings(a Application, list []agent.RedirectDomain) []string {
	out := []string{}
	own := applicationHostnames(a)
	for _, d := range list {
		if d.Target != "" && !slices.Contains(own, d.Target) {
			out = append(out, fmt.Sprintf("%s redirects to %s, which this application does not serve.", d.Host, d.Target))
		}
	}
	return out
}

func (c *Control) redirectsView(a Application, view agent.RedirectView) redirectsView {
	out := redirectsView{RedirectView: view, ServerID: a.ServerID, Addresses: []string{}, Records: []MoveDNSRecord{}, Warnings: redirectWarnings(a, view.Redirects)}
	for _, server := range c.servers() {
		if server.ID == a.ServerID {
			out.Addresses = moveTargetAddresses(agent.MoveHost{IPv4: view.IPv4, IPv6: view.IPv6}, server, nil)
		}
	}
	out.Records = redirectRecords(agent.RedirectHosts(view.Redirects), out.Addresses, view.Redirects, a.Domain)
	if view.Ingress == "caddyfile" && len(view.Redirects) > 0 {
		out.Warnings = append(out.Warnings, view.Detail)
	}
	return out
}

// redirectRecords are the A/AAAA records of redirect hosts, noted as
// redirects.
func redirectRecords(hosts, addresses []string, list []agent.RedirectDomain, primary string) []MoveDNSRecord {
	out := []MoveDNSRecord{}
	for _, host := range hosts {
		note := "redirect"
		for _, d := range list {
			if d.Host == host {
				note = fmt.Sprintf("redirect (%d to %s)", d.Status, d.RedirectTarget(primary))
			}
		}
		for _, address := range addresses {
			out = append(out, MoveDNSRecord{Hostname: host, Type: moveRecordType(address), Value: address, Note: note})
		}
	}
	return out
}

func (c *Control) redirects(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a, ok := c.app(r.PathValue("id"))
	if !ok || a.Retiring {
		http.NotFound(w, r)
		return
	}
	var view agent.RedirectView
	if err := c.agentCall(r, a.ServerID, http.MethodGet, "/v1/apps/"+a.ID+"/redirects", nil, &view); err != nil {
		var refused *agentHTTPError
		if errors.As(err, &refused) && refused.status == http.StatusNotFound {
			// Agents before redirect domains have no such route.
			agent.Fail(w, http.StatusNotFound, errors.New("update the hosting agent to manage redirect domains"))
			return
		}
		failAgent(w, err)
		return
	}
	agent.JSON(w, 200, c.redirectsView(a, view))
}

func (c *Control) updateRedirects(w http.ResponseWriter, r *http.Request) {
	var in agent.RedirectRequest
	if err := agent.Decode(w, r, &in); err != nil {
		agent.Fail(w, 400, err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old, ok := c.data.Apps[r.PathValue("id")]
	if !ok || old.Retiring {
		http.NotFound(w, r)
		return
	}
	normalized, err := agent.NormalizeRedirectDomains(applicationHostnames(old), in.Redirects)
	if err != nil {
		agent.Fail(w, 400, err)
		return
	}
	if owner, host := redirectHostOwner(c.data.Apps, agent.RedirectHosts(normalized), old.ID); owner != "" {
		agent.Fail(w, 409, fmt.Errorf("%s is already registered to %s", host, owner))
		return
	}
	var view agent.RedirectView
	if err := c.agentCall(r, old.ServerID, http.MethodPut, "/v1/apps/"+old.ID+"/redirects", agent.RedirectRequest{Redirects: normalized}, &view); err != nil {
		failAgent(w, err)
		return
	}
	next := old
	next.RedirectDomains = slices.Clone(view.Redirects)
	if len(next.RedirectDomains) == 0 {
		next.RedirectDomains = nil
	}
	c.data.Apps[old.ID] = next
	if err := c.save(); err != nil {
		c.data.Apps[old.ID] = old
		agent.Fail(w, 500, errors.New("the server saved the redirect domains but the panel could not; save them again to synchronize"))
		return
	}
	recordRedirectAudit(r.Context(), old.ID, old.RedirectDomains, next.RedirectDomains)
	agent.JSON(w, 200, c.redirectsView(next, view))
}

// recordRedirectAudit records which redirect domains changed.
func recordRedirectAudit(ctx context.Context, appID string, before, after []agent.RedirectDomain) {
	log, ok := lidza.Optional[*audit.Audit](ctx)
	if !ok || log == nil {
		return
	}
	added, removed, changed := []string{}, []string{}, []string{}
	for _, d := range after {
		i := slices.IndexFunc(before, func(o agent.RedirectDomain) bool { return o.Host == d.Host })
		switch {
		case i < 0:
			added = append(added, d.Host)
		case before[i] != d:
			changed = append(changed, d.Host)
		}
	}
	for _, d := range before {
		if !slices.ContainsFunc(after, func(o agent.RedirectDomain) bool { return o.Host == d.Host }) {
			removed = append(removed, d.Host)
		}
	}
	bounded := func(hosts []string) string { return boundedAuditList(strings.Join(hosts, ","), 480) }
	meta := map[string]string{"count": strconv.Itoa(len(after)), "added": bounded(added), "removed": bounded(removed), "changed": bounded(changed)}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = log.Record(writeCtx, audit.Event{Action: "app.redirects.update", Scope: fleetScope, Resource: "app/" + appID, Meta: meta})
}

func boundedAuditList(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
