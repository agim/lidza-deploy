package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestObservedInventoryCannotPersistCommandsSecretsOrManagedIdentities(t *testing.T) {
	raw := json.RawMessage(`{"schema":1,"read_only":true,"apps":[{"name":"MyRails","runtime":"rails","domain":"app.example.com","env":{"PASSWORD":"secret-value"},"processes":["server","secret argument"],"services":[{"service":"myrails-server.service","command":"secret-value"}]}]}`)
	apps, err := observedApplications("host.example.com", raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(apps)
	if strings.Contains(string(encoded), "secret") || len(apps) != 1 {
		t.Fatalf("unsafe observation: %s", encoded)
	}
	if _, ok := apps["observed:host.example.com:MyRails"]; !ok {
		t.Fatal("observation lost distinct identity")
	}
	for _, bad := range []string{"", "host/path", "host\nheader"} {
		if _, err := observedApplications(bad, raw); err == nil {
			t.Fatal("invalid source accepted")
		}
	}
}
