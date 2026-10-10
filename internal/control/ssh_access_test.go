package control

import "testing"

func TestSSHAccessRequiresAdministrator(t *testing.T) {
	for _, p := range []string{"GET /api/control/servers/{server}/ssh-access", "POST /api/control/servers/{server}/ssh-access/keys", "DELETE /api/control/servers/{server}/ssh-access/keys/{key}", "PATCH /api/control/servers/{server}/ssh-access"} {
		if routePermission(p) != "infrastructure.manage" {
			t.Fatalf("SSH access exposed to non-admin: %s", p)
		}
	}
}
