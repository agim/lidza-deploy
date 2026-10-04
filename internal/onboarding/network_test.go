package onboarding

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestCaddyPreservesAppRoutingAndRetries(t *testing.T) {
	cfg := `{"apps":{"http":{"servers":{"https":{"listen":[":443"],"routes":[{"@id":"app-routing","handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"127.0.0.1:8081"}]}]}]}}},"tls":{"automation":{"policies":[{"on_demand":true}]}}}}`
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	loads := 0
	http.DefaultTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "127.0.0.1:2019" {
			t.Fatal("unexpected destination")
		}
		body := cfg
		if r.Method == "POST" {
			data, _ := io.ReadAll(r.Body)
			cfg = string(data)
			loads++
			body = ""
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	for range 2 {
		if err := configureCaddy(context.Background(), "deploy.example.com", "owner@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(cfg), &parsed); err != nil {
		t.Fatal(err)
	}
	if loads != 2 || strings.Count(cfg, `"@id":"lidza-control-panel"`) != 1 || strings.Count(cfg, `"@id":"app-routing"`) != 1 || !strings.Contains(cfg, `"on_demand":true`) {
		t.Fatal("lost or duplicated app/control routing")
	}
	if !strings.Contains(cfg, `"dial":"127.0.0.1:3000"`) || !strings.Contains(cfg, `"subjects":["deploy.example.com"]`) {
		t.Fatal("missing control route/certificate")
	}
}
func TestCloudflareZoneOwnership(t *testing.T) {
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	writes := 0
	http.DefaultTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.cloudflare.com" || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Fatal("bad provider request")
		}
		body := `{"success":true,"result":{"name":"example.com"}}`
		if strings.Contains(r.URL.Path, "dns_records") {
			body = `{"success":true,"result":[]}`
		}
		if r.Method == "POST" {
			writes++
			var record map[string]any
			json.NewDecoder(r.Body).Decode(&record)
			if record["name"] != "deploy.example.com" || record["proxied"] != false {
				t.Fatal("wrong DNS change")
			}
			body = `{"success":true,"result":{}}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	zone := strings.Repeat("a", 32)
	if err := configureCloudflare(context.Background(), "other.example.net", net.ParseIP("203.0.113.1"), zone, "fixture"); err == nil || writes != 0 {
		t.Fatal("changed unrelated zone")
	}
	if err := configureCloudflare(context.Background(), "deploy.example.com", net.ParseIP("203.0.113.1"), zone, "fixture"); err != nil || writes != 1 {
		t.Fatal("DNS change failed", err)
	}
}
