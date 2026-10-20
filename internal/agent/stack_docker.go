package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

func releaseContainers(r *Release) []string {
	if r == nil {
		return nil
	}
	if r.Stack == nil {
		return []string{r.Container}
	}
	names := []string{}
	for _, c := range r.Stack.Containers {
		names = append(names, c.Container)
	}
	return names
}
func (d *Docker) setReleaseRunning(ctx context.Context, r *Release, running bool) error {
	names := releaseContainers(r)
	if !running {
		slices.Reverse(names)
	}
	return d.SetRunning(ctx, names, running)
}
func (d *Docker) buildStack(ctx context.Context, a App, id, source, commit string, manifest *StackManifest) (*Release, error) {
	images := map[string]string{}
	for _, s := range manifest.Services {
		image := s.Image
		if s.Build != nil {
			image = "lidza/" + a.ID + "-" + s.Name + ":" + id
			args := []string{"build", "--progress=plain", "--tag", image, "--file", s.Build.Dockerfile}
			if s.Build.Target != "" {
				args = append(args, "--target", s.Build.Target)
			}
			args = append(args, ".")
			if _, err := command(ctx, source, nil, "docker", args...); err != nil {
				return nil, fmt.Errorf("stack %s image build failed: %w", s.Name, err)
			}
		} else {
			if _, err := command(ctx, "", nil, "docker", "pull", image); err != nil {
				return nil, fmt.Errorf("stack %s pinned image pull failed: %w", s.Name, err)
			}
		}
		images[s.Name] = image
	}
	return d.runStack(ctx, a, id, commit, *manifest, images)
}
func (d *Docker) stackCapacity(ctx context.Context, a App, p StackProfile, manifest StackManifest) error {
	raw, err := command(ctx, "", nil, "docker", "info", "--format", "{{.MemTotal}}")
	if err != nil {
		return err
	}
	total, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return errors.New("host memory capacity unavailable")
	}
	requested := stackHostAllowance(p, total)
	for _, s := range manifest.Services {
		requested += p.budget(s.Role).MemoryMiB
	}
	ids, err := command(ctx, "", nil, "docker", "ps", "-q")
	if err != nil {
		return err
	}
	if ids != "" {
		args := append([]string{"inspect", "--format", `{{json .HostConfig.Memory}} {{index .Config.Labels "io.lidza.app"}}`}, strings.Fields(ids)...)
		raw, err = command(ctx, "", nil, "docker", args...)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(raw, "\n") {
			memory, owner, _ := strings.Cut(line, " ")
			if owner == a.ID {
				continue
			}
			n, parseErr := strconv.ParseInt(memory, 10, 64)
			if parseErr != nil || n <= 0 {
				return errors.New("stack capacity cannot budget an unbounded running container; apply a Docker memory limit or use a dedicated stack server")
			}
			requested += int((n + 1048575) / 1048576)
		}
	}
	if int64(requested)*1048576 > total {
		return fmt.Errorf("stack profile needs %d MiB including other container limits and host allowance; host has %d MiB", requested, total/1048576)
	}
	return nil
}
func ensureStackResource(ctx context.Context, kind, name, owner string) error {
	raw, err := command(ctx, "", nil, "docker", kind, "ls", "--filter", "name=^"+name+"$", "--format", "{{.Name}}")
	if err != nil {
		return err
	}
	if raw == "" {
		_, err = command(ctx, "", nil, "docker", kind, "create", "--label", "io.lidza.stack="+owner, name)
		return err
	}
	raw, err = command(ctx, "", nil, "docker", kind, "inspect", "--format", `{{index .Labels "io.lidza.stack"}}`, name)
	if err != nil || raw != owner {
		return errors.New("stack resource is owned by another application")
	}
	return nil
}
func stackPolicyArgs(s StackService) []string {
	args := []string{"--cap-drop", "ALL"}
	if s.Policy == "office" {
		// The non-root vendor helper uses file capabilities; NNP would suppress
		// them. Its bounding set includes exactly these three container-local caps.
		return append(args, "--cap-add", "CHOWN", "--cap-add", "FOWNER", "--cap-add", "SYS_CHROOT")
	}
	args = append(args, "--security-opt", "no-new-privileges")
	if s.Policy == "hardened" {
		return append(args, "--read-only", "--user", "65532:65532")
	}
	args = append(args, "--user", "0:0")
	caps := []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETUID", "SETGID", "KILL"}
	if s.Policy == "mail" {
		caps = append(caps, "NET_BIND_SERVICE", "SYS_CHROOT")
	}
	for _, cap := range caps {
		args = append(args, "--cap-add", cap)
	}
	return args
}
func officeImageUserAllowed(user string) bool {
	user = strings.SplitN(strings.TrimSpace(user), ":", 2)[0]
	if user == "" || user == "root" {
		return false
	}
	if uid, err := strconv.ParseUint(user, 10, 32); err == nil {
		return uid != 0
	}
	return true
}

func stackContainerName(a App, id, name string) string {
	return "lidza-" + a.ID + "-" + id + "-" + name
}
func (d *Docker) runStack(ctx context.Context, a App, id, commit string, manifest StackManifest, images map[string]string) (release *Release, err error) {
	if err = manifest.validate(); err != nil {
		return nil, err
	}
	profile, ok := profileByID(a.RuntimeProfile)
	if !ok {
		return nil, errors.New("choose a stack profile")
	}
	if err = d.stackCapacity(ctx, a, profile, manifest); err != nil {
		return nil, err
	}
	for _, service := range manifest.Services {
		if service.Policy == "office" {
			user, e := command(ctx, "", nil, "docker", "image", "inspect", "--format", "{{.Config.User}}", images[service.Name])
			if e != nil {
				return nil, e
			}
			if !officeImageUserAllowed(user) {
				return nil, errors.New("office policy requires a vendor image with an explicit non-root USER")
			}
		}
	}
	bootstrapEligible := manifest.BootstrapReadiness != nil && (a.Current == nil || a.Current.Stack != nil && a.Current.Stack.Bootstrap)
	if bootstrapEligible && a.Current != nil {
		if _, readyErr := d.probeReady(ctx, a.Current); readyErr == nil {
			bootstrapEligible = false
		}
	}
	stack := &StackRelease{Bootstrap: bootstrapEligible, Profile: a.RuntimeProfile, Network: "lidza-stack-" + a.resourceID() + "-" + id, Manifest: manifest}
	candidate := &Release{ID: id, Commit: commit, Created: time.Now().UTC(), Stack: stack}
	planned := *candidate
	plannedStack := *stack
	planned.Stack = &plannedStack
	for _, s := range manifest.Services {
		plannedStack.Containers = append(plannedStack.Containers, StackContainer{Service: s, Container: stackContainerName(a, id, s.Name), Image: images[s.Name]})
	}
	if d.stackTransition != nil {
		if err = d.stackTransition(a.ID, a.Current, &planned, false); err != nil {
			return nil, err
		}
	}
	success := false
	defer func() {
		if success {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		for _, c := range stack.Containers {
			d.captureCandidateFailure(cleanup, &Release{Container: c.Container})
		}
		cleanErr := d.Remove(cleanup, candidate)
		restored := true
		if a.Current != nil {
			if e := d.setReleaseRunning(cleanup, a.Current, true); e != nil {
				restored = false
				commandDiagnostic(cleanup, "stack", "Previous stack restart failed; inspect the server container inventory.")
			}
		}
		if cleanErr == nil && restored && d.stackTransition != nil {
			_ = d.stackTransition(a.ID, nil, nil, false)
		}
	}()
	// Stop all previous writers only after images and durable recovery metadata exist.
	commandDiagnostic(ctx, "stack", "Images ready; stopping previous stack writers before candidate activation.")
	for _, old := range []*Release{a.Previous, a.Current} {
		if err = d.setReleaseRunning(ctx, old, false); err != nil {
			return nil, err
		}
	}
	if err = ensureStackResource(ctx, "network", stack.Network, a.resourceID()); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(d.Root, "stack-env-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	services := slices.Clone(manifest.Services)
	slices.SortStableFunc(services, func(a, b StackService) int {
		if a.Role == "app" {
			return -1
		}
		if b.Role == "app" {
			return 1
		}
		return 0
	})
	var appName string
	for _, s := range services {
		name := stackContainerName(a, id, s.Name)
		image := images[s.Name]
		if image == "" {
			return nil, errors.New("stack service image unavailable")
		}
		env, e := stackEnvironment(a, s)
		if e != nil {
			return nil, e
		}
		if s.Role == "app" {
			appName = name
			candidate.Container = name
			candidate.Image = image
			env["LIDZA_ADDR"] = "0.0.0.0:3000"
			env["LIDZA_MODE"] = "production"
			if env["APP_URL"] == "" {
				env["APP_URL"] = "https://" + a.Domain
			}
		}
		var content strings.Builder
		for _, k := range slices.Sorted(maps.Keys(env)) {
			fmt.Fprintf(&content, "%s=%s\n", k, env[k])
		}
		envfile := filepath.Join(dir, s.Name+".env")
		if err = os.WriteFile(envfile, []byte(content.String()), 0600); err != nil {
			return nil, err
		}
		budget := profile.budget(s.Role)
		args := []string{"create", "--name", name, "--label", "io.lidza.managed=true", "--label", "io.lidza.app=" + a.ID, "--label", "io.lidza.stack=" + a.resourceID(), "--label", "io.lidza.service=" + s.Name, "--restart", "unless-stopped", "--stop-timeout", "50", "--memory", fmt.Sprintf("%dm", budget.MemoryMiB), "--memory-swap", fmt.Sprintf("%dm", budget.MemoryMiB), "--cpus", budget.CPUs, "--pids-limit", "512", "--log-opt", "max-size=10m", "--log-opt", "max-file=3", "--env-file", envfile, "--tmpfs", "/tmp:rw,nosuid,size=128m", "--tmpfs", "/run:rw,nosuid,size=32m"}
		args = append(args, stackPolicyArgs(s)...)
		if s.ShareAppNetwork {
			args = append(args, "--network", "container:"+appName)
		} else {
			args = append(args, "--network", stack.Network)
			for _, alias := range append([]string{s.Name}, s.Aliases...) {
				args = append(args, "--network-alias", alias)
			}
		}
		if s.Role == "app" {
			args = append(args, "--publish", "127.0.0.1::3000")
			args = append(args, storageMountArgs(a)...)
			for _, p := range s.PublicPorts {
				args = append(args, "--publish", fmt.Sprintf("0.0.0.0:%d:%d/%s", p.Port, p.Port, p.Protocol))
			}
			seen := map[int]bool{}
			for _, r := range s.HTTPRoutes {
				if !seen[r.Port] {
					args = append(args, "--publish", fmt.Sprintf("127.0.0.1::%d", r.Port))
					seen[r.Port] = true
				}
			}
		}
		for _, v := range s.Volumes {
			volume := "lidza-stack-" + a.resourceID() + "-" + v.Name
			if err = ensureStackResource(ctx, "volume", volume, a.resourceID()); err != nil {
				return nil, err
			}
			args = append(args, "--mount", "type=volume,src="+volume+",dst="+v.Target+",volume-nocopy")
		}
		args = append(args, image)
		args = append(args, s.Command...)
		if _, err = command(ctx, "", nil, "docker", args...); err != nil {
			return nil, fmt.Errorf("create stack service %s: %w", s.Name, err)
		}
		stack.Containers = append(stack.Containers, StackContainer{Service: s, Container: name, Image: image})
		if s.Role == "app" {
			for _, network := range a.Networks {
				if _, err = command(ctx, "", nil, "docker", "network", "connect", network, name); err != nil {
					return nil, err
				}
			}
		}
	}
	// Docker requires the namespace owner running before shared-net sidecars can
	// start. The app supervisor waits for Valkey rather than HTTP dependencies.
	for _, c := range stack.Containers {
		if _, err = command(ctx, "", nil, "docker", "start", c.Container); err != nil {
			return nil, fmt.Errorf("start stack service %s (check public port conflicts): %w", c.Service.Name, err)
		}
	}
	candidate.Port, err = d.stackPublishedPort(ctx, appName, 3000)
	if err != nil {
		return nil, err
	}
	for _, s := range services {
		for _, r := range s.HTTPRoutes {
			r.Published, err = d.stackPublishedPort(ctx, appName, r.Port)
			if err != nil {
				return nil, err
			}
			stack.Routes = append(stack.Routes, r)
		}
	}
	if err = d.waitStackReady(ctx, candidate); err != nil {
		return nil, err
	}
	success = true
	return candidate, nil
}
func (d *Docker) stackPublishedPort(ctx context.Context, name string, port int) (string, error) {
	raw, err := command(ctx, "", nil, "docker", "port", name, strconv.Itoa(port)+"/tcp")
	if err != nil {
		return "", err
	}
	if !regexp.MustCompile(`^127\.0\.0\.1:[0-9]+$`).MatchString(raw) {
		return "", errors.New("unexpected stack loopback port mapping")
	}
	return strings.TrimPrefix(raw, "127.0.0.1:"), nil
}
func (d *Docker) probeStackServices(ctx context.Context, r *Release) error {
	for _, c := range r.Stack.Containers {
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, err := command(bounded, "", nil, "docker", append([]string{"exec", c.Container}, c.Service.HealthCommand...)...)
		cancel()
		if err != nil {
			return fmt.Errorf("stack service %s is not ready", c.Service.Name)
		}
	}
	return nil
}
func (d *Docker) waitStackReady(ctx context.Context, r *Release) error {
	timeout := 8 * time.Minute
	if d.readinessTimeout > 0 {
		timeout = d.readinessTimeout
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	nextReport := time.Time{}
	for {
		body, err := d.probeReady(bounded, r)
		if err == nil {
			if err = d.probeStackServices(bounded, r); err == nil {
				r.Stack.Bootstrap = false
				return nil
			}
		}
		if r.Stack.Bootstrap && r.Stack.Manifest.BootstrapReadiness != nil {
			b := r.Stack.Manifest.BootstrapReadiness
			var status *readinessHTTPError
			if errors.As(err, &status) && status.Status == 503 && strings.Contains(body, b.ReadyzBodyContains) {
				if _, bootstrapErr := d.probeAppPath(bounded, r, b.Path); bootstrapErr == nil {
					if serviceErr := d.probeStackServices(bounded, r); serviceErr == nil {
						commandDiagnostic(ctx, "stack-bootstrap", "First-install setup UI is available; application /readyz remains gated until setup is published and reloaded.")
						return nil
					}
				}
			}
		}
		if time.Now().After(nextReport) {
			commandDiagnostic(ctx, "stack-readiness", fmt.Sprintf("Waiting for app and stack service readiness: %v\n%s", err, body))
			nextReport = time.Now().Add(15 * time.Second)
		}
		select {
		case <-bounded.Done():
			return errors.New("stack candidate failed app/service readiness; recovering the previous stack")
		case <-time.After(time.Second):
		}
	}
}
func (d *Docker) removeStack(ctx context.Context, r *Release) error {
	owner := strings.TrimSuffix(strings.TrimPrefix(r.Stack.Network, "lidza-stack-"), "-"+r.ID)
	if !strings.HasPrefix(r.Stack.Network, "lidza-stack-") || !strings.HasSuffix(r.Stack.Network, "-"+r.ID) || !idPattern.MatchString(owner) {
		return errors.New("invalid stack cleanup identity")
	}
	var first error
	for i := len(r.Stack.Containers) - 1; i >= 0; i-- {
		c := r.Stack.Containers[i]
		exists, inspectErr := command(ctx, "", nil, "docker", "container", "ls", "--all", "--filter", "name=^/"+c.Container+"$", "--format", "{{.ID}}")
		if inspectErr != nil {
			return inspectErr
		}
		if exists != "" {
			label, e := command(ctx, "", nil, "docker", "inspect", "--format", `{{index .Config.Labels "io.lidza.stack"}}`, c.Container)
			if e != nil {
				return e
			}
			if label != owner {
				continue
			}
		}
		if err := d.Remove(ctx, &Release{Container: c.Container, Image: func() string {
			if c.Service.Build != nil && strings.HasSuffix(c.Image, ":"+r.ID) {
				return c.Image
			}
			return ""
		}()}); err != nil && first == nil {
			first = err
		}
	}
	raw, err := command(ctx, "", nil, "docker", "network", "ls", "--filter", "name=^"+r.Stack.Network+"$", "--format", "{{.Name}}")
	if err != nil {
		return err
	}
	if raw != "" {
		label, e := command(ctx, "", nil, "docker", "network", "inspect", "--format", `{{index .Labels "io.lidza.stack"}}`, r.Stack.Network)
		if e != nil {
			return e
		}
		if label == owner {
			_, err = command(ctx, "", nil, "docker", "network", "rm", r.Stack.Network)
		}
	}
	if first != nil {
		return first
	}
	return err
}
func stackImages(r *Release) map[string]string {
	out := map[string]string{}
	for _, c := range r.Stack.Containers {
		out[c.Service.Name] = c.Image
	}
	return out
}

// Linux reports usable RAM after reserving kernel memory. Charge a bounded
// nominal-host difference to the profile's host allowance, never to services.
// At least half the allowance remains for userspace; smaller hosts still fail.
func stackHostAllowance(p StackProfile, totalBytes int64) int {
	missing := p.HostMiB - int(totalBytes/1048576)
	if missing > 0 && missing <= min(256, p.ReserveMiB/2) {
		return p.ReserveMiB - missing
	}
	return p.ReserveMiB
}
