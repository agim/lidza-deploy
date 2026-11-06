package detect

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDetectsClickHouseClients(t *testing.T) {
	for name, c := range map[string]struct {
		files  map[string]string
		reason string
	}{
		"go clickhouse-go v2": {map[string]string{
			"go.mod":  "module example.com/events\n\ngo 1.24\n\nrequire (\n\tgithub.com/ClickHouse/clickhouse-go/v2 v2.30.0\n\tgithub.com/jackc/pgx/v5 v5.7.0\n)\n",
			"main.go": "package main\n",
		}, "go.mod requires github.com/ClickHouse/clickhouse-go/v2: ClickHouse"},
		"go ch-go": {map[string]string{
			"go.mod":  "module example.com/events\n\ngo 1.24\n\nrequire github.com/ClickHouse/ch-go v0.63.0\n",
			"main.go": "package main\n",
		}, "go.mod requires github.com/ClickHouse/ch-go: ClickHouse"},
		"rails clickhouse-activerecord": {map[string]string{
			"Gemfile":               "gem 'rails'\ngem 'clickhouse-activerecord'\n",
			"Gemfile.lock":          "GEM\n  specs:\n    clickhouse-activerecord (1.2.0)\n    rails (7.2.0)\n",
			"config/application.rb": "",
		}, "clickhouse-activerecord gem: ClickHouse"},
		"rails click_house": {map[string]string{
			"Gemfile":               "gem 'rails'\ngem 'click_house'\n",
			"Gemfile.lock":          "GEM\n  specs:\n    click_house (2.1.2)\n    rails (7.2.0)\n",
			"config/application.rb": "",
		}, "click_house gem: ClickHouse"},
		"rails clickhouse-ruby": {map[string]string{
			"Gemfile":               "gem 'rails'\ngem 'clickhouse-ruby'\n",
			"Gemfile.lock":          "GEM\n  specs:\n    clickhouse-ruby (0.9.0)\n    rails (7.2.0)\n",
			"config/application.rb": "",
		}, "clickhouse-ruby gem: ClickHouse"},
		"node client": {map[string]string{
			"package.json": `{"name":"api","scripts":{"start":"node server.js"},"dependencies":{"express":"^4.21.0","@clickhouse/client":"^1.8.0"}}`,
			"server.js":    "",
		}, "@clickhouse/client package: ClickHouse"},
		"node web client": {map[string]string{
			"package.json": `{"name":"api","scripts":{"start":"node server.js"},"dependencies":{"express":"^4.21.0","@clickhouse/client-web":"^1.8.0"}}`,
			"server.js":    "",
		}, "@clickhouse/client-web package: ClickHouse"},
		"node legacy client": {map[string]string{
			"package.json": `{"name":"api","scripts":{"start":"node server.js"},"dependencies":{"express":"^4.21.0","clickhouse":"^2.6.0"}}`,
			"server.js":    "",
		}, "clickhouse package: ClickHouse"},
	} {
		t.Run(name, func(t *testing.T) {
			r := detect(t, c.files)
			if !r.Detected || r.Mode != ModeNative || !r.NeedsClickHouse {
				t.Fatalf("unexpected detection %+v", r)
			}
			expectReason(t, r, c.reason)
		})
	}
}

func TestClickHouseIsNotDetectedWithoutAClient(t *testing.T) {
	r := detect(t, map[string]string{
		"go.mod":  "module example.com/svc\n\ngo 1.24\n\nrequire (\n\tgithub.com/ClickHouse/clickhouse-go/v2 v2.30.0 // indirect\n\tgithub.com/lib/pq v1.10.9\n)\n",
		"main.go": "package main\n",
	})
	if r.NeedsClickHouse || hasReason(r, "ClickHouse") {
		t.Fatalf("indirect requirements must not count: %+v", r)
	}
	raw, _ := json.Marshal(r)
	if !strings.Contains(string(raw), `"needs_clickhouse":false`) {
		t.Fatalf("the flag is always reported: %s", raw)
	}
}

func TestComposeClickHouseServiceIsHostData(t *testing.T) {
	r := detect(t, map[string]string{
		"go.mod":             "module example.com/events\n\ngo 1.24\n\nrequire github.com/jackc/pgx/v5 v5.7.0\n",
		"main.go":            "package main\n",
		"docker-compose.yml": "services:\n  app:\n    build: .\n  db:\n    image: postgres:17\n  analytics:\n    image: clickhouse/clickhouse-server:26.8\n",
	})
	if r.Mode != ModeNative || !r.NeedsClickHouse || !r.NeedsPostgres || r.NeedsRedis {
		t.Fatalf("unexpected detection %+v", r)
	}
	for _, service := range r.Services {
		if service.Name == "analytics" && service.Role != "data" {
			t.Fatalf("ClickHouse service role %q", service.Role)
		}
	}
	expectReason(t, r, "docker-compose.yml services db, analytics are provided by the host natively")
}
