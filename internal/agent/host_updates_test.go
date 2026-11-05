package agent

import (
	"reflect"
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
		{Name: "linux-image-6.8.0-51-generic", Candidate: "6.8.0-51.52", Security: true},
		{Name: "caddy", Current: "2.8.4", Candidate: "2.9.1"},
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
	if c := got["caddy"]; c.Installed != "2.8.4" || c.Candidate != "2.9.1" || !reflect.DeepEqual(c.Sources, []string{"https://dl.cloudsmith.io/public/caddy/stable/deb/debian"}) {
		t.Fatalf("caddy policy = %#v", c)
	}
	if r := got["redis-server"]; r.Installed != r.Candidate || !reflect.DeepEqual(r.Sources, []string{"http://archive.ubuntu.com/ubuntu"}) {
		t.Fatalf("redis policy = %#v", r)
	}
	if d := got["docker-ce"]; d.Installed != "" || d.Candidate == "" {
		t.Fatalf("docker policy = %#v", d)
	}
}

func TestManagedOriginsCoverOnlyConfiguredOfficialRepositories(t *testing.T) {
	out := `Package files:
 100 /var/lib/dpkg/status
     release a=now
 500 https://download.docker.com/linux/ubuntu noble/stable amd64 Packages
     release o=Docker,a=noble,n=noble,l=Docker CE,c=stable,b=amd64
     origin download.docker.com
 500 http://apt.postgresql.org/pub/repos/apt noble-pgdg/main amd64 Packages
     release o=apt.postgresql.org,a=noble-pgdg,n=noble-pgdg,l=PostgreSQL for Debian/Ubuntu repository,c=main,b=amd64
     origin apt.postgresql.org
 500 https://ppa.launchpadcontent.net/example/ppa/ubuntu noble/main amd64 Packages
     release v=24.04,o=LP-PPA-example,a=noble,n=noble,l=Example,c=main,b=amd64
     origin ppa.launchpadcontent.net
 500 http://security.ubuntu.com/ubuntu noble-security/main amd64 Packages
     release v=24.04,o=Ubuntu,a=noble-security,n=noble,l=Ubuntu,c=main,b=amd64
     origin security.ubuntu.com
Pinned packages:
`
	got := managedOriginPatterns(parseAptReleases(out))
	want := []string{"origin=Docker,codename=noble", "origin=apt.postgresql.org,codename=noble-pgdg"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("managed origins = %#v, want %#v", got, want)
	}
	config := renderUnattendedConfig(got)
	for _, line := range []string{
		`APT::Periodic::Unattended-Upgrade "1";`,
		`Unattended-Upgrade::Automatic-Reboot "false";`,
		`"origin=${distro_id},archive=${distro_codename}-security";`,
		`"origin=${distro_id},codename=${distro_codename}-security";`,
		`"origin=Docker,codename=noble";`,
	} {
		if !strings.Contains(config, line) {
			t.Fatalf("unattended configuration lacks %s:\n%s", line, config)
		}
	}
	if strings.Contains(config, "LP-PPA") {
		t.Fatal("unrelated repositories must not be allowed")
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
