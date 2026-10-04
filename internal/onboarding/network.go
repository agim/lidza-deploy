package onboarding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Network struct {
	IP    string `json:"ip"`
	DNS   string `json:"dns"`
	Zone  string `json:"zone"`
	Token string `json:"token"`
	HTTPS string `json:"https"`
}

func configureNetwork(ctx context.Context, input Input) error {
	u, _ := url.Parse(input.PublicURL)
	if u.Scheme == "http" {
		return nil
	} // Input validation limits this to local setup.
	ip := net.ParseIP(input.Network.IP)
	if ip == nil {
		return errors.New("enter this server's public IP address")
	}
	if input.Network.DNS == "cloudflare" {
		if err := configureCloudflare(ctx, u.Hostname(), ip, input.Network.Zone, input.Network.Token); err != nil {
			return err
		}
	} else if input.Network.DNS != "existing" {
		return errors.New("choose existing DNS records or Cloudflare")
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil || len(addresses) == 0 {
		return errors.New("the control-panel hostname does not resolve yet; configure DNS and retry")
	}
	for _, address := range addresses {
		if !address.IP.Equal(ip) {
			return errors.New("DNS does not point exclusively to the supplied server IP; correct stale A/AAAA records and retry")
		}
	}
	switch input.Network.HTTPS {
	case "caddy":
		if u.Port() != "" && u.Port() != "443" {
			return errors.New("automatic Caddy setup uses HTTPS port 443")
		}
		if err := configureCaddy(ctx, u.Hostname(), input.Email); err != nil {
			return err
		}
		deadline, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		for {
			if verifyHTTPS(deadline, input.PublicURL) == nil {
				return nil
			}
			select {
			case <-deadline.Done():
				return errors.New("Caddy is configured but public HTTPS is not ready; check ports 80/443 and retry")
			case <-time.After(2 * time.Second):
			}
		}
	case "existing":
		return verifyHTTPS(ctx, input.PublicURL)
	default:
		return errors.New("choose automatic local Caddy or an existing HTTPS proxy")
	}
}
func configureCloudflare(ctx context.Context, domain string, ip net.IP, zone, token string) error {
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(zone) || token == "" {
		return errors.New("enter a Cloudflare zone ID and DNS-edit API token")
	}
	client := &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	call := func(method, path string, body any, out any) error {
		var data []byte
		if body != nil {
			data, _ = json.Marshal(body)
		}
		req, _ := http.NewRequestWithContext(ctx, method, "https://api.cloudflare.com/client/v4/zones/"+zone+path, bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			return errors.New("Cloudflare request failed")
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return errors.New("Cloudflare rejected the request; check zone permissions")
		}
		var envelope struct {
			Success bool
			Result  json.RawMessage
		}
		if err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&envelope); err != nil || !envelope.Success {
			return errors.New("Cloudflare DNS operation failed")
		}
		if out != nil {
			return json.Unmarshal(envelope.Result, out)
		}
		return nil
	}
	var zoneInfo struct{ Name string }
	if err := call("GET", "", nil, &zoneInfo); err != nil {
		return err
	}
	if domain != zoneInfo.Name && !strings.HasSuffix(domain, "."+zoneInfo.Name) {
		return errors.New("hostname does not belong to the supplied DNS zone")
	}
	kind := "AAAA"
	if ip.To4() != nil {
		kind = "A"
	}
	var records []struct{ ID string }
	if err := call("GET", "/dns_records?type="+kind+"&name="+url.QueryEscape(domain), nil, &records); err != nil {
		return err
	}
	if len(records) > 1 {
		return errors.New("multiple DNS records match; consolidate them before continuing")
	}
	method, path := "POST", "/dns_records"
	if len(records) == 1 {
		method = "PUT"
		path += "/" + records[0].ID
	}
	return call(method, path, map[string]any{"type": kind, "name": domain, "content": ip.String(), "ttl": 120, "proxied": false}, nil)
}

// Caddy's local admin API validates and autosaves the complete configuration.
// The installer enables --resume so these GUI changes survive service restarts.
func configureCaddy(ctx context.Context, domain, email string) error {
	client := &http.Client{Timeout: 15 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:2019/config/", nil)
	res, err := client.Do(req)
	if err != nil {
		return errors.New("local Caddy admin API is unavailable; install the control host with the agent installer")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return errors.New("could not read Caddy configuration")
	}
	var cfg map[string]any
	if err = json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&cfg); err != nil {
		return errors.New("invalid Caddy configuration")
	}
	object := func(parent map[string]any, key string) map[string]any {
		if m, ok := parent[key].(map[string]any); ok {
			return m
		}
		m := map[string]any{}
		parent[key] = m
		return m
	}
	apps := object(cfg, "apps")
	httpApp := object(apps, "http")
	servers := object(httpApp, "servers")
	var target map[string]any
	for _, raw := range servers {
		server, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		listeners, _ := server["listen"].([]any)
		for _, v := range listeners {
			if v == ":443" {
				target = server
				break
			}
		}
	}
	if target == nil {
		return errors.New("Caddy has no managed HTTPS listener")
	}
	routeID := "lidza-control-panel"
	routes, _ := target["routes"].([]any)
	kept := []any{}
	for _, raw := range routes {
		if m, ok := raw.(map[string]any); ok && m["@id"] == routeID {
			continue
		}
		kept = append(kept, raw)
	}
	route := map[string]any{"@id": routeID, "match": []any{map[string]any{"host": []string{domain}}}, "terminal": true, "handle": []any{map[string]any{"handler": "subroute", "routes": []any{
		map[string]any{"match": []any{map[string]any{"path": []string{"/metrics", "/readyz", "/healthz"}}}, "handle": []any{map[string]any{"handler": "static_response", "status_code": 404}}, "terminal": true},
		map[string]any{"handle": []any{map[string]any{"handler": "reverse_proxy", "upstreams": []any{map[string]any{"dial": "127.0.0.1:3000"}}}}},
	}}}}
	target["routes"] = append([]any{route}, kept...)
	tls := object(apps, "tls")
	certs := object(tls, "certificates")
	automate, _ := certs["automate"].([]any)
	found := false
	for _, v := range automate {
		if v == domain {
			found = true
		}
	}
	if !found {
		certs["automate"] = append(automate, domain)
	}
	automation := object(tls, "automation")
	policies, _ := automation["policies"].([]any)
	remaining := []any{}
	for _, raw := range policies {
		if m, ok := raw.(map[string]any); ok && m["@id"] == routeID+"-tls" {
			continue
		}
		remaining = append(remaining, raw)
	}
	policy := map[string]any{"@id": routeID + "-tls", "subjects": []string{domain}, "issuers": []any{map[string]any{"module": "acme", "email": email}}}
	automation["policies"] = append([]any{policy}, remaining...)
	data, _ := json.Marshal(cfg)
	req, _ = http.NewRequestWithContext(ctx, "POST", "http://127.0.0.1:2019/load", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	result, err := client.Do(req)
	if err != nil {
		return errors.New("Caddy configuration could not be applied")
	}
	defer result.Body.Close()
	if result.StatusCode != 200 {
		return errors.New("Caddy rejected control-panel HTTPS configuration")
	}
	return nil
}

func verifyHTTPS(ctx context.Context, origin string) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", origin+"/setup.html", nil)
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return errors.New("HTTPS certificate or connection verification failed")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("X-Lidza-Setup") != "required" {
		return errors.New("HTTPS must route to this control panel's setup page")
	}
	return nil
}
