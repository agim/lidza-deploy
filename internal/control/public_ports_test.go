package control

import "testing"

func TestPublicPortsPermissionIsReadOnlyFleetAccess(t *testing.T) {
	if got := routePermission("GET /api/control/apps/{id}/ports"); got != "fleet.read" {
		t.Fatal(got)
	}
}
