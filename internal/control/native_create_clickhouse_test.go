package control

import (
	"testing"

	"github.com/agim/lidza-deploy/internal/agent"
)

func TestValidateNativeCreateWithClickHouse(t *testing.T) {
	request := agent.NativeCreateRequest{ID: "events", Repository: "acme/events", Branch: "main", Runtime: "go", Domain: "events.example.com", HealthPath: "/",
		Processes: []agent.NativeProcessSpec{{Name: "web", Command: "bin/events", Web: true}}, Data: agent.NativeDataRequest{ClickHouse: true, BackupHours: 24}}
	if err := validateNativeCreate(request); err != nil {
		t.Fatal(err)
	}
	for _, key := range agent.ClickHouseEnvironmentKeys {
		request.Env = map[string]string{key: "value"}
		if err := validateNativeCreate(request); err == nil {
			t.Fatalf("%s accepted next to a provisioned ClickHouse", key)
		}
	}
	request.Data.ClickHouse = false
	if err := validateNativeCreate(request); err != nil {
		t.Fatalf("an external ClickHouse connection is the application's own: %v", err)
	}
}
