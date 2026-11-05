package migrate

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

var hostnamePattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// Requirements describes what the target host must provide.
type Requirements struct {
	// FreeBytes is the minimum free space under the application root's filesystem.
	FreeBytes uint64
	// Paths must exist on the target, for example the pinned Ruby and Node
	// installations (~/.asdf/installs/ruby/3.3.5) and the build tools.
	Paths []string
	// Ports must be free on 127.0.0.1.
	Ports []string
	// Units must not be known to the owner's service manager yet.
	Units []string
}

// Finding is one preflight observation. Blocking findings stop the move.
type Finding struct {
	Check    string `json:"check"`
	Detail   string `json:"detail"`
	Blocking bool   `json:"blocking"`
}

// Report collects findings for one host.
type Report struct {
	Host     string    `json:"host"`
	Checked  time.Time `json:"checked"`
	Findings []Finding `json:"findings"`
}

// Blockers returns the blocking findings.
func (r Report) Blockers() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Blocking {
			out = append(out, f)
		}
	}
	return out
}

func (r *Report) add(check, detail string, blocking bool) {
	r.Findings = append(r.Findings, Finding{Check: check, Detail: detail, Blocking: blocking})
}

// PreflightSource checks that the source application can be exported.
func PreflightSource(ctx context.Context, app App) Report {
	r := Report{Host: "source", Checked: time.Now().UTC()}
	if err := app.validate(); err != nil {
		r.add("application", err.Error(), true)
		return r
	}
	current, err := app.Layout.CurrentID()
	switch {
	case err != nil:
		r.add("current", err.Error(), true)
	case current == "":
		r.add("current", "application has never been published", true)
	default:
		commit, err := app.Layout.Revision(current)
		if err != nil {
			r.add("revision", "current release records no REVISION; redeploy before moving", true)
		} else {
			r.add("current", fmt.Sprintf("release %s at commit %s", current, commit), false)
		}
	}
	for _, file := range app.EnvFiles {
		if info, err := os.Stat(file); err != nil || info.IsDir() {
			r.add("environment", file+" is missing", true)
		}
	}
	for _, unit := range app.Units {
		if !exists(filepath.Join(app.UnitDir, unit)) {
			r.add("units", unit+" has no unit file in "+app.UnitDir, true)
		}
	}
	if app.Domain != "" {
		r.add("dns", describeDNS(ctx, app.Domain), false)
	}
	return r
}

// PreflightTarget checks that the target host can receive the application.
func PreflightTarget(ctx context.Context, app App, services ServiceControl, req Requirements) Report {
	r := Report{Host: "target", Checked: time.Now().UTC()}
	if err := app.validate(); err != nil {
		r.add("application", err.Error(), true)
		return r
	}
	if exists(app.Layout.Current()) {
		r.add("root", app.Layout.Root+" already has a published release", true)
	}
	if free, err := freeBytes(app.Layout.Root); err != nil {
		r.add("disk", err.Error(), true)
	} else if free < req.FreeBytes {
		r.add("disk", fmt.Sprintf("%d MiB free, need %d MiB", free>>20, req.FreeBytes>>20), true)
	} else {
		r.add("disk", fmt.Sprintf("%d MiB free", free>>20), false)
	}
	for _, path := range req.Paths {
		if !exists(path) {
			r.add("toolchain", path+" is missing", true)
		}
	}
	ports := slices.Clone(req.Ports)
	if app.Port != "" && !slices.Contains(ports, app.Port) {
		ports = append(ports, app.Port)
	}
	for _, port := range ports {
		if portInUse(ctx, port) {
			r.add("ports", "127.0.0.1:"+port+" is already listening", true)
		}
	}
	units := slices.Clone(req.Units)
	for _, unit := range app.Units {
		if !slices.Contains(units, unit) {
			units = append(units, unit)
		}
	}
	for _, unit := range units {
		if exists(filepath.Join(app.UnitDir, unit)) {
			r.add("units", unit+" already exists in "+app.UnitDir, true)
			continue
		}
		if services != nil && strings.HasSuffix(unit, ".service") {
			if state, err := services.State(ctx, unit); err == nil && state.Active != "" && state.Active != "inactive" {
				r.add("units", unit+" is "+state.Active+" on the target", true)
			}
		}
	}
	if app.Domain != "" {
		r.add("dns", describeDNS(ctx, app.Domain), false)
	}
	return r
}

func freeBytes(root string) (uint64, error) {
	probe := root
	for !exists(probe) && probe != "/" {
		probe = filepath.Dir(probe)
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(probe, &stat); err != nil {
		return 0, fmt.Errorf("migrate: statfs %s: %w", probe, err)
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

func portInUse(ctx context.Context, port string) bool {
	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// describeDNS records where a name resolves today so the operator can see
// which host serves it before and after the switch. Time-to-live values are
// not available through the standard resolver; read them from the DNS
// provider before planning the window.
func describeDNS(ctx context.Context, domain string) string {
	lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(lookup, domain)
	if err != nil {
		return domain + " does not resolve: " + err.Error()
	}
	slices.Sort(addrs)
	return domain + " resolves to " + strings.Join(addrs, ", ")
}
