package agent

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRailsDockerContract(t *testing.T) {
	if os.Getenv("TEST_DOCKER") != "1" {
		t.Skip("set TEST_DOCKER=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	a := railsApp()
	a.ID = "rails-fixture-" + newID()
	a.Domain = "rails-fixture.example.com"
	a.StorageVolume = "lidza-storage-" + a.ID
	a.Env = map[string]string{"SECRET_KEY_BASE": strings.Repeat("s", 64)}
	image := "lidza-rails-contract:" + newID()
	defer func() {
		_, _ = command(context.Background(), "", nil, "docker", "image", "rm", "-f", image)
		_, _ = command(context.Background(), "", nil, "docker", "volume", "rm", a.StorageVolume)
	}()
	if _, err := command(ctx, "", nil, "docker", "build", "-t", image, "../../tests/rails-fixture"); err != nil {
		t.Fatal(err)
	}
	if err := prepareStorageVolumeAs(ctx, a.resourceID(), a.StorageVolume, a.runtimeUser()); err != nil {
		t.Fatal(err)
	}
	// Preparing an existing mounted volume must preserve its ownership and data.
	if err := prepareStorageVolumeAs(ctx, a.resourceID(), a.StorageVolume, a.runtimeUser()); err != nil {
		t.Fatal(err)
	}
	d := &Docker{Root: t.TempDir()}
	first, err := d.runImage(ctx, a, newID(), image, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Remove(context.Background(), first)
	request := func(r *Release) string {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:"+r.Port+"/persist", nil)
		req.Host = a.Domain
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 {
			t.Fatalf("HTTP %d: %s", res.StatusCode, body)
		}
		return string(body)
	}
	if got := request(first); got != "1" {
		t.Fatalf("initial persisted value %q", got)
	}
	a.Current = first
	second, err := d.Reload(ctx, a, newID())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Remove(context.Background(), second)
	defer func() { _, _ = command(context.Background(), "", nil, "docker", "image", "rm", "-f", second.Image) }()
	if got := request(second); got != "2" {
		t.Fatalf("lost Rails files across reload: %q", got)
	}
	if second.HealthPath != "/up" || second.HealthHost != a.Domain {
		t.Fatal("Rails health metadata not retained")
	}
}
