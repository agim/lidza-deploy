package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseAptSimulationCountsSecurityUpdates(t *testing.T) {
	out := `NOTE: This is only a simulation!
Reading package lists...
Inst libssl3t64 [3.0.13-0ubuntu3.4] (3.0.13-0ubuntu3.5 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
Inst linux-image-6.8.0-51-generic (6.8.0-51.52 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
Inst caddy [2.8.4] (2.9.1 cloudsmith/caddy/stable:any-version/any-version [amd64])
Inst tzdata [2024a-3ubuntu1.1] (2025b-0ubuntu0.24.04 Ubuntu:24.04/noble-updates [all])
Conf libssl3t64 (3.0.13-0ubuntu3.5 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
Inst libc6 [2.36-9+deb12u8] (2.36-9+deb12u9 Debian-Security:12/stable-security [amd64])
`
	got := parseAptSimulation(out)
	want := []HostPackageUpgrade{
		{Name: "libssl3t64", Current: "3.0.13-0ubuntu3.4", Candidate: "3.0.13-0ubuntu3.5", Security: true},
		{Name: "linux-image-6.8.0-51-generic", Candidate: "6.8.0-51.52", Security: true, Disruptive: true, Effect: effectKernel},
		{Name: "caddy", Current: "2.8.4", Candidate: "2.9.1", Disruptive: true, Effect: effectCaddy},
		{Name: "tzdata", Current: "2024a-3ubuntu1.1", Candidate: "2025b-0ubuntu0.24.04"},
		{Name: "libc6", Current: "2.36-9+deb12u8", Candidate: "2.36-9+deb12u9", Security: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseAptSimulation:\n got %#v\nwant %#v", got, want)
	}
}

func TestParseAptPolicyCandidateRepository(t *testing.T) {
	out := `caddy:
  Installed: 2.8.4
  Candidate: 2.9.1
  Version table:
     2.9.1 500
        500 https://dl.cloudsmith.io/public/caddy/stable/deb/debian any-version/main amd64 Packages
 *** 2.8.4 100
        100 /var/lib/dpkg/status
redis-server:
  Installed: 5:7.0.15-1build2
  Candidate: 5:7.0.15-1build2
  Version table:
 *** 5:7.0.15-1build2 500
        500 http://archive.ubuntu.com/ubuntu noble/universe amd64 Packages
        100 /var/lib/dpkg/status
docker-ce:
  Installed: (none)
  Candidate: 5:27.3.1-1~ubuntu.24.04~noble
  Version table:
`
	got := parseAptPolicy(out)
	if c := got["caddy"]; c.Installed != "2.8.4" || c.Candidate != "2.9.1" || !reflect.DeepEqual(c.Sources, []string{"https://dl.cloudsmith.io/public/caddy/stable/deb/debian"}) || !reflect.DeepEqual(c.CandidateSources, []aptSource{{URL: "https://dl.cloudsmith.io/public/caddy/stable/deb/debian", Suite: "any-version/main"}}) {
		t.Fatalf("caddy policy = %#v", c)
	}
	if r := got["redis-server"]; r.Installed != r.Candidate || !reflect.DeepEqual(r.Sources, []string{"http://archive.ubuntu.com/ubuntu"}) {
		t.Fatalf("redis policy = %#v", r)
	}
	if d := got["docker-ce"]; d.Installed != "" || d.Candidate == "" {
		t.Fatalf("docker policy = %#v", d)
	}
}

const fixtureReleases = `Package files:
 100 /var/lib/dpkg/status
     release a=now
 500 https://download.docker.com/linux/ubuntu noble/stable amd64 Packages
     release o=Docker,a=noble,n=noble,l=Docker CE,c=stable,b=amd64
     origin download.docker.com
 500 https://dl.cloudsmith.io/public/caddy/stable/deb/debian any-version/main amd64 Packages
     release o=. any-version,a=any-version,n=any-version,l=. any-version,c=main,b=amd64
     origin dl.cloudsmith.io
 500 http://apt.postgresql.org/pub/repos/apt noble-pgdg/main amd64 Packages
     release o=apt.postgresql.org,a=noble-pgdg,n=noble-pgdg,l=PostgreSQL for Debian/Ubuntu repository,c=main,b=amd64
     origin apt.postgresql.org
 500 https://ppa.launchpadcontent.net/foo-security/ppa/ubuntu noble/main amd64 Packages
     release v=24.04,o=LP-PPA-foo-security,a=noble,n=noble,l=Foo,c=main,b=amd64
     origin ppa.launchpadcontent.net
 500 http://security.ubuntu.com/ubuntu noble-security/main amd64 Packages
     release v=24.04,o=Ubuntu,a=noble-security,n=noble,l=Ubuntu,c=main,b=amd64
     origin security.ubuntu.com
 500 http://archive.ubuntu.com/ubuntu noble-updates/main amd64 Packages
     release v=24.04,o=Ubuntu,a=noble-updates,n=noble,l=Ubuntu,c=main,b=amd64
     origin archive.ubuntu.com
Pinned packages:
`

// fixtureSimulation is a dist-upgrade on a host with the Docker and Caddy
// repositories: two security updates (one disruptive), a kernel, a Docker
// upgrade, a Caddy upgrade, a time zone update and a PPA whose owner calls
// itself "security".
const fixtureSimulation = `NOTE: This is only a simulation!
Inst libssl3t64 [3.0.13-0ubuntu3.4] (3.0.13-0ubuntu3.5 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
Inst postgresql-16 [16.4-0ubuntu0.24.04.1] (16.6-0ubuntu0.24.04.1 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
Inst docker-ce [5:27.3.1-1~ubuntu.24.04~noble] (5:28.0.1-1~ubuntu.24.04~noble Docker CE/noble [amd64])
Inst caddy [2.8.4] (2.9.1 . any-version/any-version [amd64])
Inst tzdata [2024a-3ubuntu1.1] (2025b-0ubuntu0.24.04 Ubuntu:24.04/noble-updates [all])
Inst linux-image-6.8.0-51-generic (6.8.0-51.52 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
Inst ppa-tool [1.0] (1.1 Foo/noble [amd64])
`

const fixturePolicy = `libssl3t64:
  Installed: 3.0.13-0ubuntu3.4
  Candidate: 3.0.13-0ubuntu3.5
  Version table:
     3.0.13-0ubuntu3.5 500
        500 http://archive.ubuntu.com/ubuntu noble-updates/main amd64 Packages
        500 http://security.ubuntu.com/ubuntu noble-security/main amd64 Packages
 *** 3.0.13-0ubuntu3.4 100
        100 /var/lib/dpkg/status
postgresql-16:
  Installed: 16.4-0ubuntu0.24.04.1
  Candidate: 16.6-0ubuntu0.24.04.1
  Version table:
     16.6-0ubuntu0.24.04.1 500
        500 http://archive.ubuntu.com/ubuntu noble-updates/main amd64 Packages
        500 http://security.ubuntu.com/ubuntu noble-security/main amd64 Packages
 *** 16.4-0ubuntu0.24.04.1 100
        100 /var/lib/dpkg/status
docker-ce:
  Installed: 5:27.3.1-1~ubuntu.24.04~noble
  Candidate: 5:28.0.1-1~ubuntu.24.04~noble
  Version table:
     5:28.0.1-1~ubuntu.24.04~noble 500
        500 https://download.docker.com/linux/ubuntu noble/stable amd64 Packages
 *** 5:27.3.1-1~ubuntu.24.04~noble 100
        100 /var/lib/dpkg/status
caddy:
  Installed: 2.8.4
  Candidate: 2.9.1
  Version table:
     2.9.1 500
        500 https://dl.cloudsmith.io/public/caddy/stable/deb/debian any-version/main amd64 Packages
 *** 2.8.4 100
        100 /var/lib/dpkg/status
tzdata:
  Installed: 2024a-3ubuntu1.1
  Candidate: 2025b-0ubuntu0.24.04
  Version table:
     2025b-0ubuntu0.24.04 500
        500 http://archive.ubuntu.com/ubuntu noble-updates/main amd64 Packages
 *** 2024a-3ubuntu1.1 100
        100 /var/lib/dpkg/status
linux-image-6.8.0-51-generic:
  Installed: (none)
  Candidate: 6.8.0-51.52
  Version table:
     6.8.0-51.52 500
        500 http://archive.ubuntu.com/ubuntu noble-updates/main amd64 Packages
        500 http://security.ubuntu.com/ubuntu noble-security/main amd64 Packages
ppa-tool:
  Installed: 1.0
  Candidate: 1.1
  Version table:
     1.1 500
        500 https://ppa.launchpadcontent.net/foo-security/ppa/ubuntu noble/main amd64 Packages
 *** 1.0 100
        100 /var/lib/dpkg/status
`

func fixturePending(t *testing.T) []HostPackageUpgrade {
	t.Helper()
	pkgs := parseAptSimulation(fixtureSimulation)
	classifySecurity(pkgs, parseAptPolicy(fixturePolicy), parseAptReleases(fixtureReleases))
	return pkgs
}

func TestSecurityClassificationComesFromSecurityPockets(t *testing.T) {
	pkgs := fixturePending(t)
	want := map[string][2]bool{ // security, disruptive
		"libssl3t64":                   {true, false},
		"postgresql-16":                {true, true},
		"docker-ce":                    {false, true},
		"caddy":                        {false, true},
		"tzdata":                       {false, false},
		"linux-image-6.8.0-51-generic": {true, true},
		"ppa-tool":                     {false, false},
	}
	if len(pkgs) != len(want) {
		t.Fatalf("pending = %d packages, want %d", len(pkgs), len(want))
	}
	for _, p := range pkgs {
		if flags := want[p.Name]; p.Security != flags[0] || p.Disruptive != flags[1] {
			t.Fatalf("%s: security=%v disruptive=%v, want %v", p.Name, p.Security, p.Disruptive, flags)
		}
		if p.Disruptive == (p.Effect == "") {
			t.Fatalf("%s: effect %q does not match disruptive=%v", p.Name, p.Effect, p.Disruptive)
		}
	}
	// A simulation line that names a security pocket is downgraded when the
	// repositories offering the candidate turn out not to be one.
	spoofed := parseAptSimulation("Inst docker-ce [5:27.3.1-1~ubuntu.24.04~noble] (5:28.0.1-1~ubuntu.24.04~noble Docker CE:noble/noble-security [amd64])\n")
	if !spoofed[0].Security {
		t.Fatal("fixture should start out classified as security")
	}
	classifySecurity(spoofed, parseAptPolicy(fixturePolicy), parseAptReleases(fixtureReleases))
	if spoofed[0].Security {
		t.Fatal("docker-ce must not count as a security update: Docker's repository has no security pocket")
	}
	// Without policy data the simulation's classification stands.
	unknown := parseAptSimulation("Inst libfoo [1] (2 Ubuntu:24.04/noble-security [amd64])\n")
	classifySecurity(unknown, parseAptPolicy(fixturePolicy), parseAptReleases(fixtureReleases))
	if !unknown[0].Security {
		t.Fatal("packages apt-cache policy does not describe keep the simulation's classification")
	}
}

func TestSelectUpgradesDefersDisruptivePackages(t *testing.T) {
	pkgs := fixturePending(t)
	names := func(rows []HostPackageUpgrade) []string {
		out := packageNames(rows)
		slices.Sort(out)
		return out
	}
	selected, deferred := selectUpgrades(pkgs, "security", false)
	if !reflect.DeepEqual(names(selected), []string{"libssl3t64"}) || !reflect.DeepEqual(names(deferred), []string{"linux-image-6.8.0-51-generic", "postgresql-16"}) {
		t.Fatalf("security scope: selected %v deferred %v", names(selected), names(deferred))
	}
	selected, deferred = selectUpgrades(pkgs, "security", true)
	if !reflect.DeepEqual(names(selected), []string{"libssl3t64", "linux-image-6.8.0-51-generic", "postgresql-16"}) || len(deferred) != 0 {
		t.Fatalf("security scope with disruptive: selected %v deferred %v", names(selected), names(deferred))
	}
	selected, deferred = selectUpgrades(pkgs, "all", false)
	if !reflect.DeepEqual(names(selected), []string{"libssl3t64", "ppa-tool", "tzdata"}) || !reflect.DeepEqual(names(deferred), []string{"caddy", "docker-ce", "linux-image-6.8.0-51-generic", "postgresql-16"}) {
		t.Fatalf("all scope: selected %v deferred %v", names(selected), names(deferred))
	}
	selected, deferred = selectUpgrades(pkgs, "disruptive", true)
	if !reflect.DeepEqual(names(selected), []string{"caddy", "docker-ce", "linux-image-6.8.0-51-generic", "postgresql-16"}) || len(deferred) != 0 {
		t.Fatalf("disruptive scope: selected %v deferred %v", names(selected), names(deferred))
	}
	pins := renderAptPins(pkgs, selected)
	for _, want := range []string{"Package: libssl3t64\nPin: version 3.0.13-0ubuntu3.5\nPin-Priority: -1\n", "Package: tzdata\nPin: version 2025b-0ubuntu0.24.04\nPin-Priority: -1\n", "Package: ppa-tool\n"} {
		if !strings.Contains(pins, want) {
			t.Fatalf("pins lack %q:\n%s", want, pins)
		}
	}
	for _, name := range names(selected) {
		if strings.Contains(pins, "Package: "+name+"\n") {
			t.Fatalf("selected package %s must not be pinned:\n%s", name, pins)
		}
	}
	if kept := aptKeptBack("Reading package lists...\nThe following packages have been kept back:\n  libsystemd0 systemd-sysv\n  udev\n0 upgraded, 0 newly installed.\n"); !reflect.DeepEqual(kept, []string{"libsystemd0", "systemd-sysv", "udev"}) {
		t.Fatalf("kept back = %v", kept)
	}
}

func TestDisruptiveEffects(t *testing.T) {
	for name, want := range map[string]string{
		"docker-ce": effectContainerRuntime, "docker-ce-cli": effectContainerRuntime, "containerd.io": effectContainerRuntime,
		"postgresql-16": effectPostgreSQL, "postgresql-17-pgvector": effectPostgreSQL, "redis-server": effectRedis, "caddy": effectCaddy,
		"linux-image-6.8.0-51-generic": effectKernel, "linux-generic": effectKernel, "linux-modules-extra-6.8.0-51-generic": effectKernel,
		"systemd": effectSystemd, "systemd-sysv": effectSystemd,
		"postgresql-client-16": "", "libssl3t64": "", "linux-libc-dev": "", "linux-firmware": "", "libsystemd0": "", "docker-buildx-plugin": "",
	} {
		effect, disruptive := disruptiveEffect(name)
		if effect != want || disruptive != (want != "") {
			t.Fatalf("%s: effect %q disruptive %v, want %q", name, effect, disruptive, want)
		}
	}
}

func TestManagedOriginsCoverOnlyConfiguredOfficialRepositories(t *testing.T) {
	got := managedOriginPatterns(parseAptReleases(fixtureReleases))
	want := []string{"origin=. any-version,codename=any-version", "origin=Docker,codename=noble", "origin=apt.postgresql.org,codename=noble-pgdg"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("managed origins = %#v, want %#v", got, want)
	}
	config := renderUnattendedConfig(got, true)
	for _, line := range []string{
		`APT::Periodic::Unattended-Upgrade "1";`,
		`Unattended-Upgrade::Automatic-Reboot "false";`,
		`"origin=${distro_id},archive=${distro_codename}-security";`,
		`"origin=${distro_id},codename=${distro_codename}-security";`,
		`"origin=Docker,codename=noble";`,
		`Unattended-Upgrade::Package-Blacklist {`,
		`"^docker-ce";`, `"^postgresql-[0-9]";`, `"^linux-image-";`, `"^systemd$";`,
	} {
		if !strings.Contains(config, line) {
			t.Fatalf("unattended configuration lacks %s:\n%s", line, config)
		}
	}
	if strings.Contains(config, "LP-PPA") {
		t.Fatal("unrelated repositories must not be allowed")
	}
	if plain := renderUnattendedConfig(nil, false); strings.Contains(plain, "Package-Blacklist") {
		t.Fatal("without deferral unattended-upgrades keeps its own package policy")
	}
}

func TestUnattendedConfiguredAndDpkgStatus(t *testing.T) {
	if !unattendedConfigured("APT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"1\";\n") {
		t.Fatal("enabled periodic upgrades not detected")
	}
	if unattendedConfigured("APT::Periodic::Unattended-Upgrade \"1\";\nAPT::Periodic::Unattended-Upgrade \"0\";\n") {
		t.Fatal("a later override disabling upgrades must win")
	}
	got := dpkgInstalled("caddy\tii \t2.8.4\nredis-server\tun \t\ndpkg-query: no packages found matching docker-ce\n")
	if !reflect.DeepEqual(got, map[string]string{"caddy": "2.8.4"}) {
		t.Fatalf("dpkgInstalled = %#v", got)
	}
}

func TestRebootWindowCrossesMidnight(t *testing.T) {
	w := HostRebootWindow{Weekdays: []int{0}, Hour: 23, DurationMinutes: 120}
	sunday := time.Date(2026, 11, 8, 23, 30, 0, 0, time.UTC)
	if start, ok := w.active(sunday); !ok || !start.Equal(time.Date(2026, 11, 8, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("Sunday 23:30 should be inside the window, got %v %v", start, ok)
	}
	if _, ok := w.active(sunday.Add(time.Hour)); !ok {
		t.Fatal("Monday 00:30 belongs to Sunday's window")
	}
	if _, ok := w.active(sunday.Add(2 * time.Hour)); ok {
		t.Fatal("Monday 01:30 is after the window")
	}
	if next := w.next(sunday.Add(3 * time.Hour)); !next.Equal(time.Date(2026, 11, 15, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("next window = %v", next)
	}
	daily := HostRebootWindow{Hour: 3, DurationMinutes: 60}
	if next := daily.next(time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC)); !next.Equal(time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("daily next window = %v", next)
	}
}

func TestHostUpdatesPolicyValidation(t *testing.T) {
	valid := defaultHostUpdatesPolicy()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	if !valid.deferDisruptive() {
		t.Fatal("disruptive upgrades wait for the window unless the operator turns that off")
	}
	off := false
	if (HostUpdatesPolicy{DisruptiveInWindow: &off}).deferDisruptive() {
		t.Fatal("explicit false must be honoured")
	}
	raw, _ := json.Marshal(valid.presented())
	if !strings.Contains(string(raw), `"disruptive_in_window":true`) {
		t.Fatalf("presented policy must spell the default out: %s", raw)
	}
	for name, p := range map[string]HostUpdatesPolicy{
		"hour":     {RebootWindow: HostRebootWindow{Hour: 24, DurationMinutes: 60}},
		"short":    {RebootWindow: HostRebootWindow{Hour: 3, DurationMinutes: 10}},
		"weekday":  {RebootWindow: HostRebootWindow{Weekdays: []int{7}, Hour: 3, DurationMinutes: 60}},
		"repeated": {RebootWindow: HostRebootWindow{Weekdays: []int{1, 1}, Hour: 3, DurationMinutes: 60}},
		"sources":  {ManagePackageSources: true, RebootWindow: HostRebootWindow{Hour: 3, DurationMinutes: 60}},
	} {
		if p.Validate() == nil {
			t.Fatalf("%s: invalid policy accepted", name)
		}
	}
}

func TestDockerRepositoryLine(t *testing.T) {
	line, err := dockerRepositoryLine(map[string]string{"ID": "ubuntu", "VERSION_CODENAME": "noble", "UBUNTU_CODENAME": "noble"}, "amd64")
	if err != nil || line != "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable\n" {
		t.Fatalf("ubuntu line = %q, %v", line, err)
	}
	if _, err := dockerRepositoryLine(map[string]string{"ID": "fedora", "VERSION_CODENAME": "40"}, "amd64"); err == nil {
		t.Fatal("non-Debian distributions must be refused")
	}
}

// fakeAptHost points host files at a fixture directory that passes aptHost and
// replaces hostCommand with handler, restoring both afterwards.
func fakeAptHost(t *testing.T, handler func(name string, args []string) (string, error)) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "run/systemd/system"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "etc/apt/preferences.d"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID=ubuntu\nID_LIKE=debian\nVERSION_CODENAME=noble\n"), 0644); err != nil {
		t.Fatal(err)
	}
	previousPath, previousCommand := hostPath, hostCommand
	hostPath = func(path string) string { return filepath.Join(root, path) }
	hostCommand = func(_ context.Context, name string, args ...string) (string, error) { return handler(name, args) }
	t.Cleanup(func() { hostPath, hostCommand = previousPath, previousCommand })
	return root
}

// aptFixtureHost serves the fixture apt output and records the upgrade
// command, including the pins apt was given.
type aptFixtureHost struct {
	upgrades [][]string
	pins     []string
	copied   []string
	keptBack string
}

func (f *aptFixtureHost) handle(name string, args []string) (string, error) {
	switch {
	case name == "apt-get" && len(args) > 0 && args[0] == "update":
		return "Reading package lists...", nil
	case name == "apt-get" && slices.Contains(args, "dist-upgrade"):
		return fixtureSimulation, nil
	case name == "apt-cache" && len(args) == 1:
		return fixtureReleases, nil
	case name == "apt-cache":
		return fixturePolicy, nil
	case name == "apt-get" && slices.Contains(args, "upgrade"):
		f.upgrades = append(f.upgrades, args)
		for _, arg := range args {
			if dir, ok := strings.CutPrefix(arg, "Dir::Etc::PreferencesParts="); ok {
				pins, _ := os.ReadFile(filepath.Join(dir, "99-lidza-run"))
				f.pins = append(f.pins, string(pins))
				entries, _ := os.ReadDir(dir)
				for _, e := range entries {
					f.copied = append(f.copied, e.Name())
				}
			}
		}
		return "Reading package lists...\n" + f.keptBack + "3 upgraded, 0 newly installed, 0 to remove.\n", nil
	}
	return "", nil
}

func TestInstallHostUpdatesPinsEverythingOutsideTheSelection(t *testing.T) {
	fixture := &aptFixtureHost{}
	root := fakeAptHost(t, fixture.handle)
	if err := os.WriteFile(filepath.Join(root, "etc/apt/preferences.d/operator-pin"), []byte("Package: *\nPin: release a=noble-backports\nPin-Priority: 100\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := installHostUpdates("security", false)(context.Background())
	if err != nil {
		t.Fatalf("security run: %v\n%s", err, out)
	}
	if len(fixture.upgrades) != 1 || !slices.Contains(fixture.upgrades[0], "--with-new-pkgs") || slices.Contains(fixture.upgrades[0], "dist-upgrade") {
		t.Fatalf("upgrade command = %v", fixture.upgrades)
	}
	pins := fixture.pins[0]
	for _, held := range []string{"docker-ce", "caddy", "tzdata", "ppa-tool", "postgresql-16", "linux-image-6.8.0-51-generic"} {
		if !strings.Contains(pins, "Package: "+held+"\n") {
			t.Fatalf("%s must be pinned away in a security run:\n%s", held, pins)
		}
	}
	if strings.Contains(pins, "Package: libssl3t64\n") {
		t.Fatalf("the selected security update must not be pinned:\n%s", pins)
	}
	if !slices.Contains(fixture.copied, "operator-pin") {
		t.Fatalf("the host's own preferences must travel with the run: %v", fixture.copied)
	}
	if want := "Summary: Security updates installed: 1. Disruptive, not installed: postgresql-16, linux-image-6.8.0-51-generic."; !strings.Contains(out, want) {
		t.Fatalf("transcript lacks %q:\n%s", want, out)
	}
	if entries, _ := filepath.Glob(filepath.Join(os.TempDir(), "lidza-apt-pins-*")); len(entries) != 0 {
		t.Fatalf("pin directories must be removed after the run: %v", entries)
	}

	fixture.keptBack = "The following packages have been kept back:\n  ppa-tool\n"
	out, err = installHostUpdates("all", false)(context.Background())
	if err != nil {
		t.Fatalf("all run: %v\n%s", err, out)
	}
	pins = fixture.pins[1]
	for _, name := range []string{"libssl3t64", "tzdata", "ppa-tool"} {
		if strings.Contains(pins, "Package: "+name+"\n") {
			t.Fatalf("%s is not disruptive and must install in an all run:\n%s", name, pins)
		}
	}
	for _, held := range []string{"docker-ce", "caddy", "postgresql-16", "linux-image-6.8.0-51-generic"} {
		if !strings.Contains(pins, "Package: "+held+"\n") {
			t.Fatalf("%s must wait without include_disruptive:\n%s", held, pins)
		}
	}
	if want := "Summary: Updates installed: 2. Disruptive, not installed: postgresql-16, docker-ce, caddy, linux-image-6.8.0-51-generic. Kept back because they need a held package: ppa-tool."; !strings.Contains(out, want) {
		t.Fatalf("transcript lacks %q:\n%s", want, out)
	}

	fixture.keptBack = ""
	if _, err := installHostUpdates("all", true)(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fixture.upgrades) != 3 || len(fixture.pins) != 2 {
		t.Fatalf("a full run needs no pins: upgrades %d pins %d", len(fixture.upgrades), len(fixture.pins))
	}
	out, err = installHostUpdates("disruptive", true)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pins = fixture.pins[2]
	if strings.Contains(pins, "Package: docker-ce\n") || !strings.Contains(pins, "Package: libssl3t64\n") {
		t.Fatalf("the window run installs only disruptive packages:\n%s", pins)
	}
	if !strings.Contains(out, "Summary: Disruptive updates installed: 4.") {
		t.Fatalf("transcript lacks the window summary:\n%s", out)
	}
}

func waitHostJob(t *testing.T, m *Manager, kind string) HostJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		var jobs []HostJob
		if m.data.HostUpdates != nil {
			jobs = slices.Clone(m.data.HostUpdates.Jobs)
		}
		m.mu.Unlock()
		for _, job := range jobs {
			if job.Kind == kind && job.Status != "running" {
				return job
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("host job %s did not finish", kind)
	return HostJob{}
}

func TestDisruptiveTickInstallsOncePerWindow(t *testing.T) {
	fixture := &aptFixtureHost{}
	fakeAptHost(t, fixture.handle)
	m := testManager(t, &fakeRuntime{})
	hc := m.host()
	sunday := time.Date(2026, 11, 8, 3, 10, 0, 0, time.UTC) // inside the default Sunday 03:00 window
	observed := observeHostUpdates(context.Background())
	observed.CheckedAt = sunday
	if observed.Disruptive != 4 || len(observed.DisruptivePackages) != 4 || observed.Security != 3 {
		t.Fatalf("observation: disruptive %d security %d", observed.Disruptive, observed.Security)
	}
	hc.mu.Lock()
	hc.status = &observed
	hc.mu.Unlock()

	m.disruptiveTick(sunday)
	job := waitHostJob(t, m, "disruptive-updates")
	if job.Status != "succeeded" || job.Trigger != "schedule" || !strings.HasPrefix(job.Summary, "Disruptive updates installed: 4.") {
		t.Fatalf("window job = %#v", job)
	}
	if len(fixture.upgrades) != 1 {
		t.Fatalf("expected one upgrade, got %d", len(fixture.upgrades))
	}
	m.disruptiveTick(sunday.Add(20 * time.Minute))
	time.Sleep(50 * time.Millisecond)
	if len(fixture.upgrades) != 1 {
		t.Fatal("a window installs at most once")
	}
	m.disruptiveTick(sunday.Add(24 * time.Hour)) // Monday: outside the window
	time.Sleep(50 * time.Millisecond)
	if len(fixture.upgrades) != 1 {
		t.Fatal("outside the window nothing installs")
	}

	off := false
	m.mu.Lock()
	m.hostUpdatesStateLocked().Policy.DisruptiveInWindow = &off
	m.hostUpdatesStateLocked().LastDisruptiveRun = time.Time{}
	m.mu.Unlock()
	m.disruptiveTick(sunday)
	time.Sleep(50 * time.Millisecond)
	if len(fixture.upgrades) != 1 {
		t.Fatal("with the policy off the window installs nothing")
	}

	status := m.hostUpdatesStatus(context.Background(), false)
	if status.NextDisruptiveWindow.IsZero() == status.Policy.deferDisruptive() {
		t.Fatalf("next disruptive window %v with policy %v", status.NextDisruptiveWindow, status.Policy.deferDisruptive())
	}
}

func TestRunHostUpdatesPassesScopeAndDisruptiveChoice(t *testing.T) {
	fixture := &aptFixtureHost{}
	fakeAptHost(t, fixture.handle)
	m := testManager(t, &fakeRuntime{})
	if err := m.runHostUpdates(HostUpdatesRun{Scope: "weekly"}); err == nil {
		t.Fatal("unknown scope accepted")
	}
	if err := m.runHostUpdates(HostUpdatesRun{}); err != nil {
		t.Fatal(err)
	}
	job := waitHostJob(t, m, "security-updates")
	if !strings.Contains(job.Summary, "Disruptive, not installed: postgresql-16") {
		t.Fatalf("default run must leave disruptive packages out: %q", job.Summary)
	}
	if err := m.runHostUpdates(HostUpdatesRun{Scope: "all", IncludeDisruptive: true}); err != nil {
		t.Fatal(err)
	}
	job = waitHostJob(t, m, "all-updates")
	if job.Summary != "Updates installed: 7." || len(fixture.pins) != 1 {
		t.Fatalf("explicit include must install everything: %q (pins %d)", job.Summary, len(fixture.pins))
	}
}

// dockerFixtureHost answers the commands the live-restore reconciliation runs.
type dockerFixtureHost struct {
	reloadErr   bool
	liveRestore bool // what `docker info` reports after a reload
	commands    []string
}

func (f *dockerFixtureHost) handle(name string, args []string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	f.commands = append(f.commands, cmd)
	switch {
	case name == "dockerd":
		return "configuration OK", nil
	case cmd == "systemctl reload docker.service":
		if f.reloadErr {
			return "Job type reload is not applicable for unit docker.service.", context.DeadlineExceeded
		}
		return "", nil
	case strings.HasPrefix(cmd, "systemctl kill"):
		return "", nil
	case name == "docker" && slices.Contains(args, "{{.LiveRestoreEnabled}}"):
		if f.liveRestore {
			return "true\n", nil
		}
		return "false\n", nil
	case name == "docker" && slices.Contains(args, "{{.ServerVersion}} {{.LiveRestoreEnabled}}"):
		if f.liveRestore {
			return "27.3.1 true\n", nil
		}
		return "27.3.1 false\n", nil
	}
	return "", nil
}

func TestEnableDockerLiveRestoreMergesAndReloads(t *testing.T) {
	fixture := &dockerFixtureHost{liveRestore: true}
	root := fakeAptHost(t, fixture.handle)
	bin := t.TempDir()
	for _, tool := range []string{"docker", "dockerd"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	daemon := filepath.Join(root, "etc/docker/daemon.json")
	if err := os.MkdirAll(filepath.Dir(daemon), 0755); err != nil {
		t.Fatal(err)
	}
	original := "{\n  \"log-driver\": \"json-file\",\n  \"log-opts\": { \"max-size\": \"10m\", \"max-file\": \"3\" },\n  \"data-root\": \"/srv/docker\"\n}\n"
	if err := os.WriteFile(daemon, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	previousWait := dockerReloadWait
	dockerReloadWait = time.Millisecond
	t.Cleanup(func() { dockerReloadWait = previousWait })
	m := testManager(t, &fakeRuntime{})
	out, err := m.enableDockerLiveRestore(context.Background())
	if err != nil {
		t.Fatalf("enable: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(daemon)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("daemon.json is not JSON after the merge: %v\n%s", err, raw)
	}
	if cfg["live-restore"] != true || cfg["log-driver"] != "json-file" || cfg["data-root"] != "/srv/docker" || cfg["log-opts"].(map[string]any)["max-file"] != "3" {
		t.Fatalf("merged configuration = %s", raw)
	}
	backups, _ := filepath.Glob(daemon + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("backups = %v", backups)
	}
	if previous, _ := os.ReadFile(backups[0]); string(previous) != original {
		t.Fatalf("backup differs from the previous file:\n%s", previous)
	}
	if !slices.Contains(fixture.commands, "systemctl reload docker.service") || slices.ContainsFunc(fixture.commands, func(c string) bool { return strings.Contains(c, "restart") }) {
		t.Fatalf("live-restore must be applied with a reload, never a restart: %v", fixture.commands)
	}
	if !strings.Contains(out, "Summary: Docker live-restore is on") {
		t.Fatalf("transcript lacks the summary:\n%s", out)
	}
	m.mu.Lock()
	state := m.data.HostUpdates.Docker
	m.mu.Unlock()
	if state == nil || state.LiveRestoreApplied.IsZero() || state.RestartPending || state.ConfigBackup != backups[0] {
		t.Fatalf("docker state = %#v", state)
	}
	if status := observeDocker(context.Background()); !status.Running || !status.LiveRestore || !status.Configured || status.Version != "27.3.1" {
		t.Fatalf("observed docker = %#v", status)
	}

	// A daemon that ignores the reload is left for the maintenance window.
	fixture.liveRestore = false
	fixture.reloadErr = true
	out, err = m.enableDockerLiveRestore(context.Background())
	if err == nil || !strings.Contains(err.Error(), "maintenance window") {
		t.Fatalf("expected a deferred restart, got %v\n%s", err, out)
	}
	if !slices.ContainsFunc(fixture.commands, func(c string) bool { return strings.HasPrefix(c, "systemctl kill --kill-whom=main --signal=SIGHUP") }) {
		t.Fatalf("a unit without ExecReload gets the signal directly: %v", fixture.commands)
	}
	m.mu.Lock()
	pending := m.data.HostUpdates.Docker.RestartPending
	m.mu.Unlock()
	if !pending {
		t.Fatal("restart must be recorded as pending")
	}
	status := HostDockerStatus{Running: true, Managed: true, RestartPending: true}
	if warning := dockerWarning(status, defaultHostUpdatesPolicy(), time.Date(2026, 11, 8, 3, 0, 0, 0, time.UTC)); !strings.Contains(warning, "restarts in the maintenance window starting Sun 08 Nov 03:00 UTC") {
		t.Fatalf("warning = %q", warning)
	}
	if warning := dockerWarning(HostDockerStatus{Running: true}, defaultHostUpdatesPolicy(), time.Time{}); !strings.Contains(warning, "does not manage Docker") {
		t.Fatalf("unmanaged warning = %q", warning)
	}
	if warning := dockerWarning(HostDockerStatus{Running: true, LiveRestore: true}, defaultHostUpdatesPolicy(), time.Time{}); warning != "" {
		t.Fatalf("no warning when live-restore is on, got %q", warning)
	}

	// Malformed configuration is never replaced.
	if err := os.WriteFile(daemon, []byte("{ not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.enableDockerLiveRestore(context.Background()); err == nil || !strings.Contains(err.Error(), "untouched") {
		t.Fatalf("malformed daemon.json must be refused, got %v", err)
	}
	if raw, _ := os.ReadFile(daemon); string(raw) != "{ not json" {
		t.Fatal("malformed daemon.json was rewritten")
	}
}

func TestDockerManagedNeedsAgentInstallOrContainers(t *testing.T) {
	m := testManager(t, &fakeRuntime{})
	if m.dockerManaged() {
		t.Fatal("a host without container releases or an agent install is not managed")
	}
	m.updateDockerState(func(s *HostDockerState) { s.InstalledByAgent = true })
	if !m.dockerManaged() {
		t.Fatal("an agent-installed Docker is managed")
	}
}
