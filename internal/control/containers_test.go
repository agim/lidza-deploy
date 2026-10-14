package control

import "testing"

func TestContainerRoutePermissions(t *testing.T) {
	if got := routePermission("GET /api/control/servers/{server}/containers"); got != "fleet.read" {
		t.Fatal(got)
	}
	if got := routePermission("POST /api/control/servers/{server}/containers/restart"); got != "infrastructure.manage" {
		t.Fatal(got)
	}
}
