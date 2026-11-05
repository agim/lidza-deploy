package agent

// Pure parsers for apt, dpkg and unattended-upgrades output. They take command
// output as text so they can be tested without a Debian host.

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
}

var aptInstLine = regexp.MustCompile(`^Inst (\S+) (?:\[([^\]]*)\] )?\((\S+) ([^\[]*?)\s*(?:\[[^\]]*\])?\)`)

// parseAptSimulation reads `apt-get -s dist-upgrade` output. A package is a
// security update when any repository offering the candidate is a security
// pocket (Ubuntu "noble-security", Debian "Debian-Security:12/stable-security").
func parseAptSimulation(out string) []HostPackageUpgrade {
	var rows []HostPackageUpgrade
	for _, line := range strings.Split(out, "\n") {
		match := aptInstLine.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		origins := strings.ToLower(match[4])
		rows = append(rows, HostPackageUpgrade{Name: match[1], Current: match[2], Candidate: match[3], Security: strings.Contains(origins, "-security") || strings.Contains(origins, "debian-security")})
	}
	return rows
}

// aptPackagePolicy is the installed and candidate version of one package and
// the repositories that offer the candidate.
type aptPackagePolicy struct {
	Installed string
	Candidate string
	Sources   []string // repository URLs offering the candidate
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

// aptRelease is one repository from `apt-cache policy` (no arguments).
type aptRelease struct {
	URL      string
	Origin   string
	Codename string
	Archive  string
	Label    string
}

// parseAptReleases reads the "Package files" section of `apt-cache policy`.
func parseAptReleases(out string) []aptRelease {
	var rows []aptRelease
	var url string
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.Contains(fields[1], "://") {
			if _, err := strconv.Atoi(fields[0]); err == nil {
				url = fields[1]
				continue
			}
		}
		if !strings.HasPrefix(line, "release ") || url == "" {
			continue
		}
		release := aptRelease{URL: url}
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
		url = ""
	}
	return rows
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
// reboot: reboots happen only in the agent's reboot window.
func renderUnattendedConfig(extraOrigins []string) string {
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
	return b.String()
}
