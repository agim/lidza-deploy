package agent

import (
	"os"
	"strings"
	"testing"
)

func TestNativeUnitRendersHardenedSystemUnit(t *testing.T) {
	got, err := renderNativeUnit(nativeUnitSpec{App: "shop", Process: "web", User: "app-shop", Root: "/srv/lidza/apps/shop",
		Command: `bin/rails server -b 127.0.0.1 -p "$PORT"`, CPUPercent: 150})
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/native_unit_web.service")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("unit differs from testdata/native_unit_web.service:\n%s", got)
	}
}

func TestNativeUnitQuotesCommandsAndCapsResources(t *testing.T) {
	got, err := renderNativeUnit(nativeUnitSpec{App: "api", Process: "worker", User: "app-api", Root: "/srv/apps/api",
		Command: `bin/worker --label "100% \ done" $QUEUE`, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`ExecStart=/bin/sh -c "exec bin/worker --label \"100%% \\ done\" $$QUEUE"`,
		"MemoryMax=512M",
		"ReadWritePaths=/srv/apps/api/shared",
		"CapabilityBoundingSet=\n",
	} {
		if !strings.Contains(got, line) {
			t.Fatalf("unit lacks %q:\n%s", line, got)
		}
	}
	if strings.Contains(got, "CPUQuota") {
		t.Fatal("a unit without a CPU limit must not set CPUQuota")
	}
}

func TestNativeUnitRejectsUnsafeSpecs(t *testing.T) {
	base := nativeUnitSpec{App: "api", Process: "web", User: "app-api", Root: "/srv/apps/api", Command: "bin/api"}
	for name, change := range map[string]func(*nativeUnitSpec){
		"root user":     func(s *nativeUnitSpec) { s.User = "root" },
		"line break":    func(s *nativeUnitSpec) { s.Command = "bin/api\nExecStartPre=/bin/evil" },
		"empty command": func(s *nativeUnitSpec) { s.Command = " " },
		"relative root": func(s *nativeUnitSpec) { s.Root = "srv/apps/api" },
		"process name":  func(s *nativeUnitSpec) { s.Process = "Web Server" },
		"memory":        func(s *nativeUnitSpec) { s.MemoryMiB = -1 },
	} {
		spec := base
		change(&spec)
		if _, err := renderNativeUnit(spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
