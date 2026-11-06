package agent

// Pure parsers and classifiers for apt, dpkg and unattended-upgrades output.
// They take command output as text so they can be tested without a Debian
// host. Two classifications matter for safety:
//
//   - security: the candidate version is offered by a security repository
//     (a suite or label containing "security"); third-party repositories such
//     as Docker's, Caddy's or PGDG have no security pocket, so their updates
//     never qualify however urgent they are.
//   - disruptive: installing the package restarts a service every hosted
//     application depends on, or needs a reboot. Those install only on
//     request or inside the maintenance window.

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// HostPackageUpgrade is one package apt would upgrade or newly install.
type HostPackageUpgrade struct {
	Name      string `json:"name"`
	Current   string `json:"current,omitempty"` // empty for a new dependency, such as a kernel
	Candidate string `json:"candidate"`
	Security  bool   `json:"security"`
	// Disruptive packages restart shared services or need a reboot; Effect
	// says what happens when they install.
	Disruptive bool   `json:"disruptive,omitempty"`
	Effect     string `json:"effect,omitempty"`
}

var aptInstLine = regexp.MustCompile(`^Inst (\S+) (?:\[([^\]]*)\] )?\((\S+) ([^\[]*?)\s*(?:\[[^\]]*\])?\)`)

// parseAptSimulation reads `apt-get -s dist-upgrade` output. The release
// string after the version names the repositories offering the candidate
// ("Ubuntu:24.04/noble-security", "Debian-Security:12/stable-security"); it
// is a first classification that classifySecurity confirms against
// `apt-cache policy`.
func parseAptSimulation(out string) []HostPackageUpgrade {
	var rows []HostPackageUpgrade
	for _, line := range strings.Split(out, "\n") {
		match := aptInstLine.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		origins := strings.ToLower(match[4])
		row := HostPackageUpgrade{Name: match[1], Current: match[2], Candidate: match[3], Security: strings.Contains(origins, "-security") || strings.Contains(origins, "debian-security")}
		row.Effect, row.Disruptive = disruptiveEffect(row.Name)
		rows = append(rows, row)
	}
	return rows
}

// aptSource is one repository index: its URL and suite/component
// ("noble-security/main").
type aptSource struct {
	URL   string
	Suite string
}

// aptPackagePolicy is the installed and candidate version of one package and
// the repositories that offer the candidate.
type aptPackagePolicy struct {
	Installed string
	Candidate string
	Sources   []string // repository URLs offering the candidate
	// CandidateSources are the same repositories with their suite, so the
	// pocket a candidate comes from can be told apart on a mirror that
	// serves several.
	CandidateSources []aptSource
}

// parseAptPolicy reads `apt-cache policy <packages...>`.
func parseAptPolicy(out string) map[string]aptPackagePolicy {
	result := map[string]aptPackagePolicy{}
	var name string
	var current aptPackagePolicy
	inCandidate := false
	flush := func() {
		if name != "" {
			result[name] = current
		}
	}
	for _, raw := range strings.Split(out, "\n") {
		if raw == "" {
			continue
		}
		if !strings.HasPrefix(raw, " ") && strings.HasSuffix(raw, ":") {
			flush()
			name = strings.TrimSuffix(raw, ":")
			current = aptPackagePolicy{}
			inCandidate = false
			continue
		}
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "Installed:"):
			current.Installed = aptVersionValue(strings.TrimPrefix(line, "Installed:"))
		case strings.HasPrefix(line, "Candidate:"):
			current.Candidate = aptVersionValue(strings.TrimPrefix(line, "Candidate:"))
		case line == "Version table:":
		default:
			fields := strings.Fields(strings.TrimPrefix(line, "*** "))
			if len(fields) == 0 {
				continue
			}
			// Version rows are indented five spaces ("     2.9.1 500" or " *** 2.8.4 100");
			// source rows under them are indented eight ("        500 https://... ").
			if strings.HasPrefix(raw, "        ") {
				if inCandidate && len(fields) >= 2 && strings.Contains(fields[1], "://") {
					current.Sources = append(current.Sources, fields[1])
					source := aptSource{URL: fields[1]}
					if len(fields) >= 3 {
						source.Suite = fields[2]
					}
					current.CandidateSources = append(current.CandidateSources, source)
				}
				continue
			}
			if len(fields) >= 2 {
				if _, err := strconv.Atoi(fields[1]); err == nil {
					inCandidate = fields[0] == current.Candidate
				}
			}
		}
	}
	flush()
	return result
}

func aptVersionValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "(none)" {
		return ""
	}
	return v
}

// aptRelease is one repository index from `apt-cache policy` (no arguments)
// with the fields of its Release file.
type aptRelease struct {
	URL      string
	Suite    string // "noble-security/main" as the index row prints it
	Origin   string
	Codename string
	Archive  string
	Label    string
}

// parseAptReleases reads the "Package files" section of `apt-cache policy`.
func parseAptReleases(out string) []aptRelease {
	var rows []aptRelease
	var url, suite string
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.Contains(fields[1], "://") {
			if _, err := strconv.Atoi(fields[0]); err == nil {
				url, suite = fields[1], ""
				if len(fields) >= 3 {
					suite = fields[2]
				}
				continue
			}
		}
		if !strings.HasPrefix(line, "release ") || url == "" {
			continue
		}
		release := aptRelease{URL: url, Suite: suite}
		for _, part := range strings.Split(strings.TrimPrefix(line, "release "), ",") {
			key, value, _ := strings.Cut(part, "=")
			switch key {
			case "o":
				release.Origin = value
			case "n":
				release.Codename = value
			case "a":
				release.Archive = value
			case "l":
				release.Label = value
			}
		}
		rows = append(rows, release)
		url, suite = "", ""
	}
	return rows
}

// securityRelease reports whether a repository is a security pocket: its
// suite, archive or label contains "security" (Ubuntu "noble-security",
// Debian "stable-security" / "Debian-Security"). Origin names are not
// consulted: a third-party repository cannot qualify by calling itself one.
func securityRelease(r aptRelease) bool {
	for _, v := range []string{r.Suite, r.Archive, r.Label} {
		if strings.Contains(strings.ToLower(v), "security") {
			return true
		}
	}
	return false
}

// classifySecurity confirms each pending package's security flag against
// `apt-cache policy`: a package is a security update only when a repository
// offering its candidate version is a security pocket. Packages the policy
// output does not describe keep the simulation's classification.
func classifySecurity(pkgs []HostPackageUpgrade, policies map[string]aptPackagePolicy, releases []aptRelease) {
	index := map[aptSource]aptRelease{}
	for _, r := range releases {
		index[aptSource{URL: r.URL, Suite: r.Suite}] = r
	}
	for i := range pkgs {
		policy, ok := policies[pkgs[i].Name]
		if !ok || len(policy.CandidateSources) == 0 || (policy.Candidate != "" && policy.Candidate != pkgs[i].Candidate) {
			continue
		}
		security := false
		for _, source := range policy.CandidateSources {
			if strings.Contains(strings.ToLower(source.Suite), "security") {
				security = true
				break
			}
			if release, ok := index[source]; ok && securityRelease(release) {
				security = true
				break
			}
		}
		pkgs[i].Security = security
	}
}

// Effects of upgrading a disruptive package, shown to the operator before
// the upgrade is confirmed or scheduled.
const (
	effectContainerRuntime = "restarts the container runtime: all containers restart unless Docker live-restore is on"
	effectPostgreSQL       = "restarts PostgreSQL: every database connection drops until the server is back"
	effectRedis            = "restarts Redis: caches and queues reconnect"
	effectCaddy            = "restarts Caddy: HTTPS traffic pauses while it comes back"
	effectKernel           = "installs a new kernel: the server must reboot to run it"
	effectSystemd          = "re-executes systemd: units, sockets and timers blip and a reboot is recommended"
)

// disruptiveEffect reports what installing a package interrupts. The list is
// deliberately short and covers what hosted applications share: the
// container runtime, the databases, the web server, the kernel and systemd.
func disruptiveEffect(name string) (string, bool) {
	switch {
	case slices.Contains([]string{"docker-ce", "docker-ce-cli", "docker-ce-rootless-extras", "containerd.io", "docker.io", "containerd"}, name):
		return effectContainerRuntime, true
	case name == "postgresql" || strings.HasPrefix(name, "postgresql-") && !strings.HasPrefix(name, "postgresql-client") && !strings.HasPrefix(name, "postgresql-doc"):
		return effectPostgreSQL, true
	case slices.Contains([]string{"redis-server", "redis", "valkey", "valkey-server"}, name):
		return effectRedis, true
	case name == "caddy":
		return effectCaddy, true
	case slices.Contains([]string{"systemd", "systemd-sysv", "systemd-resolved", "systemd-timesyncd", "systemd-networkd"}, name):
		return effectSystemd, true
	case kernelPackage(name):
		return effectKernel, true
	}
	return "", false
}

// kernelPackage matches kernel images, modules and the meta packages that
// pull a new kernel in; headers, firmware and userland helpers are left out.
func kernelPackage(name string) bool {
	for _, prefix := range []string{"linux-image-", "linux-modules", "linux-generic", "linux-virtual", "linux-signed-image", "linux-image-generic", "linux-kvm", "linux-aws", "linux-gcp", "linux-azure", "linux-oracle"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// unattendedDisruptiveBlacklist keeps unattended-upgrades away from the
// disruptive packages while the agent installs them in the maintenance
// window instead. Entries are Python regular expressions anchored at the
// start of the package name, as unattended-upgrades reads them.
var unattendedDisruptiveBlacklist = []string{
	"^docker-ce", "^containerd[.]io$", "^docker[.]io$", "^containerd$",
	"^postgresql-[0-9]", "^postgresql$",
	"^redis-server$", "^redis$", "^valkey",
	"^caddy$",
	"^linux-image-", "^linux-modules", "^linux-generic", "^linux-virtual", "^linux-signed-image",
	"^systemd$", "^systemd-sysv$",
}

// selectUpgrades picks the packages one run installs: scope "security" takes
// confirmed security updates, "all" every pending package and "disruptive"
// only the disruptive ones. Disruptive packages leave the first two scopes
// unless includeDisruptive is set. The second list names what was left out
// because it is disruptive, so the job can say what waits for the window.
func selectUpgrades(pending []HostPackageUpgrade, scope string, includeDisruptive bool) (selected, deferred []HostPackageUpgrade) {
	for _, p := range pending {
		switch scope {
		case "disruptive":
			if p.Disruptive {
				selected = append(selected, p)
			}
			continue
		case "security":
			if !p.Security {
				continue
			}
		}
		if p.Disruptive && !includeDisruptive {
			deferred = append(deferred, p)
			continue
		}
		selected = append(selected, p)
	}
	return selected, deferred
}

// renderAptPins writes an apt preferences fragment that forbids the
// candidate versions of every pending package outside the selection. With
// it, `apt-get upgrade` installs the selection and keeps back anything that
// would need an excluded package, instead of failing or pulling it in.
func renderAptPins(pending, selected []HostPackageUpgrade) string {
	keep := map[string]bool{}
	for _, p := range selected {
		keep[p.Name] = true
	}
	var b strings.Builder
	b.WriteString("# Written by the Līdza Deploy agent for one package run; not kept.\n")
	for _, p := range pending {
		if keep[p.Name] || p.Candidate == "" {
			continue
		}
		b.WriteString("\nPackage: " + p.Name + "\nPin: version " + p.Candidate + "\nPin-Priority: -1\n")
	}
	return b.String()
}

// aptKeptBack reads the packages `apt-get upgrade` listed as kept back.
func aptKeptBack(out string) []string {
	var names []string
	collecting := false
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "The following packages have been kept back:"):
			collecting = true
		case collecting && strings.HasPrefix(line, " "):
			names = append(names, strings.Fields(line)...)
		case collecting:
			collecting = false
		}
	}
	return names
}

func packageNames(pkgs []HostPackageUpgrade) []string {
	names := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		names = append(names, p.Name)
	}
	return names
}

// Third-party repositories whose updates unattended-upgrades may install when
// package sources are managed: the official Caddy, PostgreSQL (PGDG) and
// Docker repositories.
var managedRepositoryHosts = []string{"dl.cloudsmith.io/public/caddy/", "apt.postgresql.org/", "download.docker.com/"}

func managedRepository(url string) bool {
	for _, host := range managedRepositoryHosts {
		if strings.Contains(url, "://"+host) {
			return true
		}
	}
	return false
}

var originValue = regexp.MustCompile(`^[A-Za-z0-9 ._/:+~-]{1,100}$`)

// managedOriginPatterns turns configured official repositories into
// Unattended-Upgrade::Origins-Pattern entries. Only repositories already
// configured on the host are included; nothing is added to apt sources.
func managedOriginPatterns(releases []aptRelease) []string {
	var out []string
	for _, r := range releases {
		if !managedRepository(r.URL) || !originValue.MatchString(r.Origin) {
			continue
		}
		pattern := "origin=" + r.Origin
		switch {
		case originValue.MatchString(r.Codename):
			pattern += ",codename=" + r.Codename
		case originValue.MatchString(r.Archive):
			pattern += ",archive=" + r.Archive
		}
		if !slices.Contains(out, pattern) {
			out = append(out, pattern)
		}
	}
	slices.Sort(out)
	return out
}

// unattendedConfigured reads `apt-config dump` and reports whether periodic
// unattended upgrades are switched on.
func unattendedConfigured(dump string) bool {
	enabled := false
	for _, line := range strings.Split(dump, "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, "APT::Periodic::Unattended-Upgrade "); ok {
			value = strings.Trim(strings.TrimSuffix(value, ";"), `"`)
			enabled = value != "" && value != "0" && !strings.EqualFold(value, "false")
		}
	}
	return enabled
}

// dpkgInstalled reads `dpkg-query -W -f='${Package}\t${db:Status-Abbrev}\t${Version}\n'`.
func dpkgInstalled(out string) map[string]string {
	installed := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) == 3 && strings.HasPrefix(strings.TrimSpace(parts[1]), "ii") {
			installed[parts[0]] = parts[2]
		}
	}
	return installed
}

// renderUnattendedConfig is the agent's apt configuration drop-in. It sorts
// after the distribution's 50unattended-upgrades so its values win, adds the
// security pockets for Ubuntu (archive) and Debian (codename), optionally the
// configured official repositories, and disables unattended-upgrades' own
// reboot: reboots happen only in the agent's reboot window. When disruptive
// packages are deferred to that window, unattended-upgrades leaves them alone
// too, so a daily run never restarts the container runtime or a database.
func renderUnattendedConfig(extraOrigins []string, deferDisruptive bool) string {
	var b strings.Builder
	b.WriteString("// Managed by the Līdza Deploy agent; edit the server's update policy in the control panel.\n")
	b.WriteString("APT::Periodic::Update-Package-Lists \"1\";\n")
	b.WriteString("APT::Periodic::Unattended-Upgrade \"1\";\n")
	b.WriteString("Unattended-Upgrade::Automatic-Reboot \"false\";\n")
	b.WriteString("Unattended-Upgrade::Origins-Pattern {\n")
	b.WriteString("\t\"origin=${distro_id},archive=${distro_codename}-security\";\n")
	b.WriteString("\t\"origin=${distro_id},codename=${distro_codename}-security\";\n")
	for _, origin := range extraOrigins {
		b.WriteString("\t\"" + origin + "\";\n")
	}
	b.WriteString("};\n")
	if deferDisruptive {
		b.WriteString("// Disruptive packages install in the maintenance window instead.\n")
		b.WriteString("Unattended-Upgrade::Package-Blacklist {\n")
		for _, pattern := range unattendedDisruptiveBlacklist {
			b.WriteString("\t\"" + pattern + "\";\n")
		}
		b.WriteString("};\n")
	}
	return b.String()
}
