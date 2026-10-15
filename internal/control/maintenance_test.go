package control

import "testing"

func TestMaintenancePermissions(t *testing.T) {
	for _, pattern := range []string{"PATCH /api/control/servers/{server}/maintenance", "POST /api/control/servers/{server}/maintenance/run"} {
		if routePermission(pattern) != "infrastructure.manage" {
			t.Fatal("cleanup must be admin-only", pattern)
		}
	}
	if routePermission("GET /api/control/servers/{server}/maintenance") != "fleet.read" {
		t.Fatal("cleanup status permission")
	}
}
