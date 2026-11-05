package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNativeRegistrationUsesOnlyVerifiedStaticPort(t *testing.T) {
	m, app := nativeFixture(t)
	if err := os.WriteFile(filepath.Join(app, ".asdf-vars"), []byte("PORT=9140\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := m.nativeRegistration(NativeApplication{ID: "rails", Name: "MyRails", Domain: "rails.example.com", Port: "22"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Port != "9140" {
		t.Fatal("caller-selected loopback port accepted")
	}
	if err := os.WriteFile(filepath.Join(app, ".asdf-vars"), []byte("PORT=$(invalid)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if m.nativeConfiguredPort(a.Name, a.Services) != "" {
		t.Fatal("dynamic port binding accepted")
	}
}

func TestNativeStatusNeverConnectsToSocketActivatedApplication(t *testing.T) {
	for _, tc := range []struct{ active, state, startup string }{
		{"inactive", "dead", "disabled"},
		{"inactive", "dead", "masked"},
		{"active", "running", "disabled"},
		{"active", "running", "enabled"},
	} {
		t.Run(tc.active+"/"+tc.startup, func(t *testing.T) {
			m, app := nativeFixture(t)
			var connections atomic.Int32
			// A request would activate an app through a listening systemd socket.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connections.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			if err := os.WriteFile(filepath.Join(app, ".asdf-vars"), []byte("PORT="+u.Port()+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			a, err := m.nativeRegistration(NativeApplication{ID: "rails", Name: "MyRails", Domain: "rails.example.com"})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			m.nativeCommand = func(_ context.Context, args []string) (string, error) {
				calls++
				expected := append([]string{"systemctl", "--user", "show", "--property=Id,ActiveState,SubState,UnitFileState", "--no-pager"}, a.Services...)
				if !slices.Equal(args, expected) {
					t.Fatalf("status invoked an unexpected command: %q", args)
				}
				var output strings.Builder
				for _, unit := range a.Services {
					fmt.Fprintf(&output, "Id=%s\nActiveState=%s\nSubState=%s\nUnitFileState=%s\n\n", unit, tc.active, tc.state, tc.startup)
				}
				return output.String(), nil
			}
			status, err := m.nativeStatus(context.Background(), a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || connections.Load() != 0 || status.Readiness.Ready || status.Readiness.HTTPStatus != 0 {
				t.Fatal("status performed an action or a network readiness probe")
			}
			for _, service := range status.Services {
				if service.Active != tc.active || service.State != tc.state || service.UnitFileState != tc.startup {
					t.Fatalf("running state and startup policy were conflated: %+v", service)
				}
			}
		})
	}
}
