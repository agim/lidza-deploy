package agent

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestDockerOwnerClaimPrivatePermissions(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1")
	}
	d := &Docker{Root: t.TempDir()}
	a := testApp("private-claim")
	ctx := context.Background()
	dir, err := d.prepareOwnerClaim(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Release only this fixture's private directory for TempDir cleanup.
		if _, err := command(context.Background(), "", nil, "docker", "run", "--rm", "--network", "none", "--mount", "type=bind,src="+dir+",dst=/claim", cacheImage, "sh", "-c", "rm -f /claim/token /claim/status.json && chmod 777 /claim"); err != nil {
			t.Error(err)
		}
	})
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatal("claim directory permissions are not private")
	}
	// The cloud filesystem maps host UIDs; inspect ownership in the same
	// Docker mount namespace used by the application.
	owner, err := command(ctx, "", nil, "docker", "run", "--rm", "--network", "none", "--mount", "type=bind,src="+dir+",dst=/claim,readonly", cacheImage, "stat", "-c", "%u:%g:%a", "/claim")
	if err != nil || owner != "65532:65532:700" {
		t.Fatalf("private directory ownership: %s: %v", owner, err)
	}
	// The app writes private files as UID 65532. The deploy account needs
	// the isolated reader rather than widening their host permissions.
	token := strings.Repeat("owner-token-", 6)
	if _, err = command(ctx, "", nil, "docker", "run", "--rm", "--network", "none", "--user", "65532:65532", "--mount", "type=bind,src="+dir+",dst=/claim", cacheImage, "sh", "-c", `umask 077; printf '%s' '{"state":"unclaimed"}' > /claim/status.json; printf '%s' "$1" > /claim/token`, "claim-fixture", token); err != nil {
		t.Fatal(err)
	}
	var diagnostics strings.Builder
	ctx = context.WithValue(ctx, diagnosticKey{}, func(line string) { diagnostics.WriteString(line) })
	for _, name := range []string{"status.json", "token"} {
		got, err := d.readOwnerClaimFile(ctx, a, name, 4096)
		if err != nil || len(got) == 0 {
			t.Fatalf("private file unavailable: %s: %v", name, err)
		}
		if name == "token" && string(got) != token {
			t.Fatal("token changed")
		}
	}
	if _, err = d.prepareOwnerClaim(ctx, a); err != nil {
		t.Fatal("repeat preparation failed", err)
	}
	if got, err := d.readOwnerClaimFile(ctx, a, "token", 4096); err != nil || string(got) != token {
		t.Fatal("preparation rotated token", err)
	}
	if _, err := d.readOwnerClaimFile(ctx, a, "../../secret", 4096); err == nil {
		t.Fatal("accepted arbitrary file")
	}
	if strings.Contains(diagnostics.String(), token) {
		t.Fatal("token leaked into deployment diagnostics")
	}
	if _, err = command(context.Background(), "", nil, "docker", "run", "--rm", "--network", "none", "--user", "65532:65532", "--mount", "type=bind,src="+dir+",dst=/claim", cacheImage, "sh", "-c", "rm /claim/token && ln -s status.json /claim/token"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.readOwnerClaimFile(ctx, a, "token", 4096); err == nil {
		t.Fatal("read symbolic token file")
	}
}
