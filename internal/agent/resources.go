package agent

// Resource accounting belongs to the deployment agent: it owns the containers,
// launch reservations and release/worker lifetimes. No app credentials are read
// from Docker to collect these measurements.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/procfs"
)

const mib = int64(1 << 20)

type AppResources struct {
	MemoryMB       int64   `json:"memory_mb"`
	WebMemoryMB    int64   `json:"web_memory_mb"`
	WorkerMemoryMB int64   `json:"worker_memory_mb"`
	CPUs           float64 `json:"cpus"`
	Weight         int64   `json:"weight"`
}

func defaultAppResources() AppResources { return AppResources{1024, 512, 256, 1, 1024} }
func (a App) resourceLimits() AppResources {
	if a.Resources != nil {
		return *a.Resources
	}
	return defaultAppResources()
}
func (l AppResources) Validate() error {
	if l.MemoryMB < 2*l.WebMemoryMB {
		return errors.New("app RAM budget must reserve two web containers for safe deployments")
	}
	if l.MemoryMB < 128 || l.MemoryMB > 1048576 || l.WebMemoryMB < 64 || l.WorkerMemoryMB < 64 || l.WebMemoryMB > l.MemoryMB || l.WorkerMemoryMB > l.MemoryMB || l.CPUs < 0.1 || l.CPUs > 128 || math.IsNaN(l.CPUs) || math.IsInf(l.CPUs, 0) || l.Weight < 128 || l.Weight > 8192 {
		return errors.New("resources need a valid RAM budget, web/worker limits of at least 64 MiB, 0.1–128 CPU cores and weight 128–8192")
	}
	return nil
}

type ServiceResources struct {
	MemoryMB int64   `json:"memory_mb"`
	CPUs     float64 `json:"cpus"`
}

func (l ServiceResources) Validate() error {
	if l.MemoryMB < 64 || l.MemoryMB > 1048576 || l.CPUs < 0.1 || l.CPUs > 128 || math.IsNaN(l.CPUs) || math.IsInf(l.CPUs, 0) {
		return errors.New("service limits require at least 64 MiB and 0.1–128 CPU cores")
	}
	return nil
}

type ResourcePolicy struct {
	MemoryMode        string                      `json:"memory_mode,omitempty"`
	HostMemoryMB      int64                       `json:"host_memory_mb"`
	HostCPUs          float64                     `json:"host_cpus"`
	BuildMemoryMB     int64                       `json:"build_memory_mb"`
	OperationMemoryMB int64                       `json:"operation_memory_mb"`
	OperationCPUs     float64                     `json:"operation_cpus"`
	BuildCPUs         float64                     `json:"build_cpus"`
	Services          map[string]ServiceResources `json:"services"`
}

func defaultResourcePolicy(memory int64, cpus float64) ResourcePolicy {
	return ResourcePolicy{MemoryMode: "automatic", HostMemoryMB: max(512, (memory/5/128)*128), HostCPUs: max(0.1, cpus/8), BuildMemoryMB: max(1024, min(4096, ((memory/4+127)/128)*128)), BuildCPUs: min(1, cpus/3), OperationMemoryMB: 512, OperationCPUs: min(0.5, cpus/4), Services: map[string]ServiceResources{}}
}
func (p ResourcePolicy) Validate() error {
	if p.MemoryMode != "" && p.MemoryMode != "automatic" && p.MemoryMode != "strict" {
		return errors.New("memory management must be automatic or strict")
	}
	if p.OperationMemoryMB < 512 || p.OperationMemoryMB > 1048576 || p.OperationCPUs < 0.1 || p.OperationCPUs > 128 || math.IsNaN(p.OperationCPUs) || math.IsInf(p.OperationCPUs, 0) || p.OperationCPUs*64/float64(p.OperationMemoryMB) < 0.01 {
		return errors.New("operational headroom requires at least 512 MiB and enough CPU for bounded helper commands")
	}
	if p.HostMemoryMB < 256 || p.HostMemoryMB > 1048576 || p.BuildMemoryMB < 256 || p.BuildMemoryMB > 1048576 || p.HostCPUs < 0.1 || p.HostCPUs > 128 || p.BuildCPUs < 0.1 || p.BuildCPUs > 128 || math.IsNaN(p.HostCPUs+p.BuildCPUs) || math.IsInf(p.HostCPUs+p.BuildCPUs, 0) || len(p.Services) > 1000 {
		return errors.New("invalid server resource reservations")
	}
	for _, l := range p.Services {
		if err := l.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type ContainerResource struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	AppID       string   `json:"app_id,omitempty"`
	Kind        string   `json:"kind"`
	State       string   `json:"state"`
	Running     bool     `json:"running"`
	CPUPercent  *float64 `json:"cpu_percent,omitempty"`
	MemoryBytes *int64   `json:"memory_bytes,omitempty"`
	MemoryLimit int64    `json:"memory_limit"`
	CPUQuota    float64  `json:"cpu_quota"`
	CPUShares   int64    `json:"cpu_shares"`
	Restarts    int64    `json:"restarts"`
	OOMKilled   bool     `json:"oom_killed"`
	CPUPeriod   int64    `json:"-"`
	RawQuota    int64    `json:"-"`
	NanoCPUs    int64    `json:"-"`
	SwapLimit   int64    `json:"swap_limit"`
}
type AppResourceView struct {
	ID               string              `json:"id"`
	Limits           AppResources        `json:"limits"`
	RequiredMemoryMB int64               `json:"required_memory_mb"`
	AllocatedCPUs    float64             `json:"allocated_cpus"`
	CPUPercent       *float64            `json:"cpu_percent,omitempty"`
	MemoryBytes      *int64              `json:"memory_bytes,omitempty"`
	Stopped          bool                `json:"stopped"`
	Applied          bool                `json:"applied"`
	Containers       []ContainerResource `json:"containers"`
}
type ResourceReport struct {
	CapacityMode      string              `json:"capacity_mode"`
	HostAvailableMB   *int64              `json:"host_available_mb,omitempty"`
	PendingMemoryMB   int64               `json:"pending_memory_mb"`
	StartingWork      bool                `json:"-"`
	Supported         bool                `json:"supported"`
	Checked           time.Time           `json:"checked"`
	Policy            ResourcePolicy      `json:"policy"`
	MemoryMB          int64               `json:"memory_mb"`
	CPUs              float64             `json:"cpus"`
	ReservedMemoryMB  int64               `json:"reserved_memory_mb"`
	AvailableMemoryMB int64               `json:"available_memory_mb"`
	Building          bool                `json:"building"`
	Apps              []AppResourceView   `json:"apps"`
	Services          []ContainerResource `json:"services"`
	Errors            []string            `json:"errors"`
}
type resourceLaunch struct {
	Name, AppID, Kind string
	MemoryMB          int64
}
type resourceState struct {
	Apps            map[string]App
	Tasks           []Task
	Policy          *ResourcePolicy
	BuilderName     string
	HostAvailableMB *int64
	PendingMemoryMB int64
	StartingWork    bool
}

func (m *Manager) resourceState() resourceState {
	m.mu.Lock()
	defer m.mu.Unlock()
	apps := map[string]App{}
	for id, a := range m.data.Apps {
		if m.resourceStarting[id] {
			a.Stopped = false
		}
		apps[id] = a
	}
	tasks := []Task{}
	for _, t := range m.data.Tasks {
		tasks = append(tasks, t)
	}
	builder := ""
	if d, ok := m.runtime.(*Docker); ok {
		builder = "buildx_buildkit_" + resourceBuilderName(d.Root) + "0"
	}
	return resourceState{Apps: apps, Tasks: tasks, Policy: m.data.ResourcePolicy, BuilderName: builder}
}
func resourceHost() (int64, float64, error) {
	fs, err := procfs.NewFS("/proc")
	if err != nil {
		return 0, 0, err
	}
	mem, err := fs.Meminfo()
	if err != nil || mem.MemTotalBytes == nil {
		return 0, 0, errors.New("host RAM measurement unavailable")
	}
	cpus := runtime.NumCPU()
	if cpus == 0 {
		return 0, 0, errors.New("host CPU measurement unavailable")
	}
	return int64(*mem.MemTotalBytes) / mib, float64(cpus), nil
}

// Strictly selected inspect fields: Config.Env, command arguments, mounts and
// arbitrary container labels never enter an API response or diagnostics.
func resourceCommand(ctx context.Context, args ...string) (string, error) {
	b, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return "", errors.New("Docker resource inspection/update unavailable")
	}
	if len(b) > 4<<20 {
		return "", errors.New("Docker inventory exceeds resource inspection limit")
	}
	return strings.TrimSpace(string(b)), nil
}
func resourceControllers(ctx context.Context) error {
	raw, err := resourceCommand(ctx, "info", "--format", `{"memory":{{json .MemoryLimit}},"cpu":{{json .CPUCfsQuota}},"shares":{{json .CPUShares}}}`)
	if err != nil {
		return err
	}
	var v struct{ Memory, CPU, Shares bool }
	if json.Unmarshal([]byte(raw), &v) != nil || !v.Memory || !v.CPU || !v.Shares {
		return errors.New("Docker host cannot enforce CPU/RAM limits; enable its cgroup memory, CPU quota and CPU shares controllers")
	}
	return nil
}
func resourceInventory(ctx context.Context) ([]ContainerResource, error) {
	ids, err := resourceCommand(ctx, "ps", "-aq")
	if err != nil {
		return nil, err
	}
	if ids == "" {
		return []ContainerResource{}, nil
	}
	list := strings.Fields(ids)
	if len(list) > 1000 {
		return nil, errors.New("too many containers to account safely")
	}
	format := `{"id":{{json .Id}},"name":{{json .Name}},"app":{{json (index .Config.Labels "io.lidza.app")}},"task":{{json (index .Config.Labels "io.lidza.task")}},"database":{{json (index .Config.Labels "io.lidza.database")}},"cache":{{json (index .Config.Labels "io.lidza.cache")}},"clickhouse":{{json (index .Config.Labels "io.lidza.clickhouse")}},"operation":{{json (index .Config.Labels "io.lidza.operation")}},"running":{{.State.Running}},"restarting":{{.State.Restarting}},"status":{{json .State.Status}},"memory":{{.HostConfig.Memory}},"swap":{{.HostConfig.MemorySwap}},"nano":{{.HostConfig.NanoCpus}},"quota":{{.HostConfig.CpuQuota}},"period":{{.HostConfig.CpuPeriod}},"shares":{{.HostConfig.CpuShares}},"restarts":{{.RestartCount}},"oom":{{.State.OOMKilled}}}`
	raw, err := resourceCommand(ctx, append([]string{"inspect", "--format", format}, list...)...)
	if err != nil {
		return nil, err
	}
	out := []ContainerResource{}
	for _, line := range strings.Split(raw, "\n") {
		var v struct {
			ID, Name, App, Task, Database, Cache, Status, Operation, Clickhouse string
			Running, Restarting                                                 bool
			Memory, Swap, Nano, Quota, Period, Shares, Restarts                 int64
			OOM                                                                 bool
		}
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			return nil, errors.New("invalid Docker resource inventory")
		}
		c := ContainerResource{ID: v.ID, Name: strings.TrimPrefix(v.Name, "/"), Running: v.Running || v.Restarting, State: v.Status, MemoryLimit: v.Memory, SwapLimit: v.Swap, CPUPeriod: v.Period, RawQuota: v.Quota, NanoCPUs: v.Nano, CPUShares: v.Shares, Restarts: v.Restarts, OOMKilled: v.OOM, Kind: "other"}
		if v.Nano > 0 {
			c.CPUQuota = float64(v.Nano) / 1e9
		} else if v.Quota > 0 && v.Period > 0 {
			c.CPUQuota = float64(v.Quota) / float64(v.Period)
		}
		switch {
		case v.Operation != "":
			c.Kind = "operation"
		case v.Task != "":
			c.AppID = v.Task
			c.Kind = "worker"
		case v.App != "":
			c.AppID = v.App
			c.Kind = "web"
		case v.Clickhouse != "":
			c.Kind = "clickhouse"
		case v.Database != "":
			c.Kind = "database"
		case v.Cache != "":
			c.Kind = "cache"
		case strings.HasPrefix(c.Name, "buildx_buildkit_lidza-"):
			c.Kind = "build"
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b ContainerResource) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}
func parseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && ((s[i] >= '0' && s[i] <= '9') || s[i] == '.') {
		i++
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return 0, errors.New("invalid memory measurement")
	}
	units := map[string]float64{"B": 1, "kB": 1000, "MB": 1e6, "GB": 1e9, "TB": 1e12, "KiB": 1024, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}
	scale, ok := units[strings.TrimSpace(s[i:])]
	if !ok || n*scale > math.MaxInt64 {
		return 0, errors.New("invalid memory units")
	}
	return int64(n * scale), nil
}
func resourceStats(ctx context.Context, containers []ContainerResource) error {
	names := []string{}
	indexes := map[string]int{}
	for i, c := range containers {
		if c.Running {
			names = append(names, c.ID)
			indexes[c.ID] = i
			indexes[c.Name] = i
		}
	}
	if len(names) == 0 {
		return nil
	}
	raw, err := resourceCommand(ctx, append([]string{"stats", "--no-stream", "--no-trunc", "--format", `{"id":{{json .ID}},"name":{{json .Name}},"cpu":{{json .CPUPerc}},"memory":{{json .MemUsage}}}`}, names...)...)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(raw, "\n") {
		var v struct{ ID, Name, CPU, Memory string }
		if json.Unmarshal([]byte(line), &v) != nil {
			return errors.New("invalid container resource statistics")
		}
		i, ok := indexes[v.ID]
		if !ok {
			i, ok = indexes[v.Name]
		}
		if !ok {
			continue
		}
		cpu, err := strconv.ParseFloat(strings.TrimSuffix(v.CPU, "%"), 64)
		if err == nil && cpu >= 0 && !math.IsNaN(cpu) && !math.IsInf(cpu, 0) {
			containers[i].CPUPercent = &cpu
		}
		used, _, _ := strings.Cut(v.Memory, "/")
		memory, err := parseMemory(used)
		if err == nil {
			containers[i].MemoryBytes = &memory
		}
	}
	return nil
}
func appRequiredMemory(a App, tasks []Task) int64 {
	l := a.resourceLimits()
	required := 2 * l.WebMemoryMB
	for _, t := range tasks {
		if t.AppID == a.ID && t.Enabled {
			required += l.WorkerMemoryMB
		}
	}
	return required
}

// Weighted max-min allocation keeps equal-priority apps equal even if an app
// has more workers. Caps are then divided among that app's active containers.
func fairCPU(pool float64, wanted map[string]float64, weights map[string]int64) map[string]float64 {
	out := map[string]float64{}
	pending := map[string]bool{}
	for k := range wanted {
		pending[k] = true
	}
	for len(pending) > 0 && pool > 0 {
		sum := int64(0)
		for k := range pending {
			sum += weights[k]
		}
		if sum <= 0 {
			break
		}
		fixed := false
		for k := range pending {
			share := pool * float64(weights[k]) / float64(sum)
			if wanted[k] <= share {
				out[k] = wanted[k]
				pool -= wanted[k]
				delete(pending, k)
				fixed = true
				break
			}
		}
		if !fixed {
			for k := range pending {
				out[k] = pool * float64(weights[k]) / float64(sum)
			}
			break
		}
	}
	return out
}
func makeResourceReport(state resourceState, containers []ContainerResource, pending map[string]resourceLaunch, building bool, memory int64, cpus float64) (ResourceReport, error) {
	policy := defaultResourcePolicy(memory, cpus)
	if state.Policy != nil {
		policy = *state.Policy
	}
	out := ResourceReport{Supported: true, Checked: time.Now().UTC(), Policy: policy, MemoryMB: memory, CPUs: cpus, Building: building, Apps: []AppResourceView{}, Services: []ContainerResource{}, Errors: []string{}}
	out.CapacityMode = "automatic"
	if policy.MemoryMode == "strict" {
		out.CapacityMode = "strict"
	}
	out.HostAvailableMB = state.HostAvailableMB
	out.PendingMemoryMB = state.PendingMemoryMB
	out.StartingWork = state.StartingWork
	actualActive := map[string]bool{}
	for _, c := range containers {
		if c.Running {
			actualActive[c.AppID] = true
		}
	}
	wanted := map[string]float64{}
	weights := map[string]int64{}
	out.ReservedMemoryMB = policy.HostMemoryMB + policy.BuildMemoryMB + policy.OperationMemoryMB
	for id, a := range state.Apps {
		if a.Retiring {
			continue
		}
		l := a.resourceLimits()
		if err := l.Validate(); err != nil {
			return out, err
		}
		v := AppResourceView{ID: id, Limits: l, Stopped: a.Stopped, RequiredMemoryMB: appRequiredMemory(a, state.Tasks), Containers: []ContainerResource{}}
		out.Apps = append(out.Apps, v)
		if !a.Stopped || actualActive[id] {
			out.ReservedMemoryMB += l.MemoryMB
			if out.CapacityMode != "automatic" || actualActive[id] {
				wanted["app:"+id] = l.CPUs
				weights["app:"+id] = l.Weight
			}
		}
	}
	for _, c := range containers {
		if _, ok := state.Apps[c.AppID]; ok && (c.Kind == "web" || c.Kind == "worker") {
			continue
		}
		if !c.Running {
			continue
		}
		out.Services = append(out.Services, c)
		if c.Kind == "operation" {
			continue
		}
		if c.Kind == "build" && c.Name == state.BuilderName {
			out.Building = true
			continue
		}
		limit := c.MemoryLimit / mib
		cpu := c.CPUQuota
		if c.Kind == "database" || c.Kind == "cache" || c.Kind == "clickhouse" {
			l := serviceLimit(policy, c.Name, c.Kind)
			limit = l.MemoryMB
			cpu = l.CPUs
		}
		if limit == 0 {
			if c.MemoryBytes != nil {
				limit = (*c.MemoryBytes + mib - 1) / mib
			}
			out.Errors = append(out.Errors, "Unbounded external container "+c.Name+"; reserve its growth in host headroom.")
		}
		out.ReservedMemoryMB += limit
		if cpu <= 0 {
			cpu = cpus
		}
		wanted["service:"+c.Name] = cpu
		weights["service:"+c.Name] = 256
	}
	pool := cpus - policy.HostCPUs - policy.OperationCPUs
	if out.Building {
		pool -= policy.BuildCPUs
	}
	allocated := fairCPU(pool, wanted, weights)
	for i := range out.Apps {
		v := &out.Apps[i]
		v.AllocatedCPUs = allocated["app:"+v.ID]
		for _, c := range containers {
			if c.AppID == v.ID && (c.Kind == "web" || c.Kind == "worker") {
				v.Containers = append(v.Containers, c)
			}
		}
		used := int64(0)
		cpu := float64(0)
		complete := true
		active := 0
		memoryLimits := int64(0)
		cpuLimits := float64(0)
		for _, c := range v.Containers {
			if !c.Running {
				continue
			}
			active++
			memoryLimits += c.MemoryLimit
			cpuLimits += c.CPUQuota
			if c.MemoryBytes == nil || c.CPUPercent == nil {
				complete = false
				continue
			}
			used += *c.MemoryBytes
			cpu += *c.CPUPercent
		}
		if active == 0 || complete {
			v.MemoryBytes = &used
			v.CPUPercent = &cpu
		}
		v.Applied = active > 0 && memoryLimits <= v.Limits.MemoryMB*mib && cpuLimits <= v.AllocatedCPUs+0.001
		for _, c := range v.Containers {
			cap := v.Limits.WebMemoryMB
			if c.Kind == "worker" {
				cap = v.Limits.WorkerMemoryMB
			}
			if c.Running && (c.MemoryLimit == 0 || c.CPUQuota == 0 || c.MemoryLimit > cap*mib) {
				v.Applied = false
			}
		}
	}
	slices.SortFunc(out.Apps, func(a, b AppResourceView) int { return strings.Compare(a.ID, b.ID) })
	out.AvailableMemoryMB = memory - out.ReservedMemoryMB
	if out.CapacityMode == "automatic" && out.HostAvailableMB != nil {
		buildHeadroom := int64(0)
		if out.Building {
			buildHeadroom = policy.BuildMemoryMB
		}
		out.AvailableMemoryMB = *out.HostAvailableMB - policy.HostMemoryMB - buildHeadroom - policy.OperationMemoryMB - out.PendingMemoryMB
	}
	if policy.HostCPUs+policy.BuildCPUs+policy.OperationCPUs >= cpus || pool <= 0 {
		return out, errors.New("CPU reservations leave no capacity for applications")
	}
	return out, nil
}
func serviceLimit(p ResourcePolicy, name, kind string) ServiceResources {
	if l, ok := p.Services[name]; ok {
		return l
	}
	if kind == "clickhouse" {
		return ServiceResources{2048, 1}
	}
	if kind == "database" {
		return ServiceResources{1024, 0.5}
	}
	return ServiceResources{256, 0.5}
}
func checkResourceCapacity(report ResourceReport, state resourceState) error {
	if report.CapacityMode == "strict" && report.AvailableMemoryMB < 0 {
		return fmt.Errorf("RAM budgets reserve %d MiB but this host has %d MiB; adjust app/service budgets or host/build reservations", report.ReservedMemoryMB, report.MemoryMB)
	}
	if report.CapacityMode == "automatic" && report.StartingWork {
		if report.HostAvailableMB == nil {
			return errors.New("live host RAM measurement unavailable; cannot safely start new work")
		}
		if report.AvailableMemoryMB < 0 {
			return &resourceMemoryPressure{ShortfallMB: -report.AvailableMemoryMB}
		}
	}
	for _, v := range report.Apps {
		if v.Stopped && !slices.ContainsFunc(v.Containers, func(c ContainerResource) bool { return c.Running }) {
			continue
		}
		if v.RequiredMemoryMB > v.Limits.MemoryMB {
			return fmt.Errorf("%s needs %d MiB for two web releases and enabled workers; its app budget is %d MiB", v.ID, v.RequiredMemoryMB, v.Limits.MemoryMB)
		}
	}
	return nil
}

func (m *Manager) resourceReport(ctx context.Context, stats bool) (ResourceReport, error) {
	m.resourceMu.Lock()
	defer m.resourceMu.Unlock()
	if stats && time.Since(m.resourceChecked) < 10*time.Second && m.resourceCached != nil {
		return *m.resourceCached, nil
	}
	memory, cpus, err := resourceHost()
	if err != nil {
		return ResourceReport{}, err
	}
	containers, err := resourceInventory(ctx)
	if err != nil {
		return ResourceReport{}, err
	}
	var statsErr error
	if stats {
		statsErr = resourceStats(ctx, containers)
	}
	state, err := liveResourceCapacityState(m.resourceState(), containers, m.resourcePending, m.resourceBuilding)
	if err != nil {
		return ResourceReport{}, err
	}
	report, err := makeResourceReport(state, containers, m.resourcePending, m.resourceBuilding, memory, cpus)
	if supportErr := resourceControllers(ctx); supportErr != nil {
		report.Supported = false
		report.Errors = append(report.Errors, supportErr.Error())
	}
	if statsErr != nil {
		report.Errors = append(report.Errors, statsErr.Error())
	}
	if err == nil && stats {
		m.resourceCached = &report
		m.resourceChecked = time.Now()
	}
	return report, err
}
func (m *Manager) invalidateResources() { m.resourceCached = nil; m.resourceChecked = time.Time{} }
func resourceLimitsArgs(memory int64, cpu float64, shares int64) []string {
	return []string{"--memory", strconv.FormatInt(memory, 10) + "m", "--memory-swap", strconv.FormatInt(memory*2, 10) + "m", "--cpus", strconv.FormatFloat(math.Floor(cpu*100)/100, 'f', 2, 64), "--cpu-shares", strconv.FormatInt(shares, 10)}
}

// Serialize inspection, admission and updates with container launch reservations.
// Memory caps are never automatically reduced as another worker/app is added.
func (m *Manager) reconcileResources(ctx context.Context, projected *resourceState, extra *resourceLaunch) ([]string, error) {
	return m.reconcileResourcePlan(ctx, projected, extra, true)
}
func (m *Manager) reconcileResourcePlan(ctx context.Context, projected *resourceState, extra *resourceLaunch, admit bool) ([]string, error) {
	if err := resourceControllers(ctx); err != nil {
		return nil, err
	}
	state := m.resourceState()
	if projected != nil {
		state = *projected
	}
	memory, cpus, err := resourceHost()
	if err != nil {
		return nil, err
	}
	containers, err := resourceInventory(ctx)
	if err != nil {
		return nil, err
	}
	if slices.ContainsFunc(containers, func(c ContainerResource) bool {
		_, owned := state.Apps[c.AppID]
		return c.Running && !owned && c.Kind == "other" && c.MemoryLimit == 0
	}) {
		if e := resourceStats(ctx, containers); e != nil {
			return nil, errors.New("cannot account for an unbounded external container's current memory usage")
		}
	}
	names := map[string]bool{}
	for _, c := range containers {
		names[c.Name] = true
	}
	virtual := slices.Clone(containers)
	pending := map[string]resourceLaunch{}
	for k, v := range m.resourcePending {
		pending[k] = v
	}
	if extra != nil {
		pending[extra.Name] = *extra
	}
	for _, p := range pending {
		if names[p.Name] {
			for i := range virtual {
				if virtual[i].Name == p.Name {
					virtual[i].Running = true
				}
			}
		} else {
			virtual = append(virtual, ContainerResource{Name: p.Name, AppID: p.AppID, Kind: p.Kind, Running: true, MemoryLimit: p.MemoryMB * mib})
		}
	}
	state, err = liveResourceCapacityState(state, containers, pending, m.resourceBuilding)
	if err != nil {
		return nil, err
	}
	report, err := makeResourceReport(state, virtual, pending, m.resourceBuilding, memory, cpus)
	if err != nil {
		return nil, err
	}
	if err = checkResourceCapacity(report, state); err != nil && admit {
		oldReport, oldErr := makeResourceReport(m.resourceState(), containers, pending, m.resourceBuilding, memory, cpus)
		correcting := report.CapacityMode == "strict" && projected != nil && extra == nil && oldErr == nil && oldReport.CapacityMode == "strict" && oldReport.AvailableMemoryMB < 0 && report.ReservedMemoryMB < oldReport.ReservedMemoryMB
		if !correcting {
			return nil, err
		}
		for _, v := range report.Apps {
			if !v.Stopped && v.RequiredMemoryMB > v.Limits.MemoryMB {
				return nil, err
			}
		}
	}
	desired := map[string]struct {
		memory int64
		cpu    float64
		shares int64
	}{}
	for _, v := range report.Apps {
		active := []ContainerResource{}
		sum := int64(0)
		for _, c := range v.Containers {
			if c.Running {
				active = append(active, c)
				if c.Kind == "worker" {
					sum += v.Limits.WorkerMemoryMB
				} else {
					sum += v.Limits.WebMemoryMB
				}
			}
		}
		if sum > v.Limits.MemoryMB && admit {
			return nil, fmt.Errorf("%s active containers require %d MiB, exceeding its %d MiB budget", v.ID, sum, v.Limits.MemoryMB)
		}
		for _, c := range active {
			if v.Limits.Weight < int64(len(active))*2 {
				return nil, errors.New("too many running processes for this CPU priority; raise the priority or stop workers")
			}
			cpu := v.AllocatedCPUs / float64(len(active))
			if cpu < 0.01 {
				return nil, errors.New("CPU budget too small for the number of running app processes")
			}
			ram := v.Limits.WebMemoryMB
			if c.Kind == "worker" {
				ram = v.Limits.WorkerMemoryMB
			}
			desired[c.Name] = struct {
				memory int64
				cpu    float64
				shares int64
			}{ram, cpu, max(2, v.Limits.Weight/int64(len(active)))}
		}
	}
	wanted := map[string]float64{}
	weights := map[string]int64{}
	for _, v := range report.Apps {
		if (report.CapacityMode != "automatic" && !v.Stopped) || v.AllocatedCPUs > 0 {
			wanted["app:"+v.ID] = v.Limits.CPUs
			weights["app:"+v.ID] = v.Limits.Weight
		}
	}
	for _, c := range report.Services {
		if c.Kind == "operation" {
			continue
		}
		if c.Kind == "build" && c.Name == state.BuilderName {
			continue
		}
		cpu := c.CPUQuota
		if c.Kind == "database" || c.Kind == "cache" || c.Kind == "clickhouse" {
			cpu = serviceLimit(report.Policy, c.Name, c.Kind).CPUs
		}
		if cpu <= 0 {
			cpu = cpus
		}
		wanted["service:"+c.Name] = cpu
		weights["service:"+c.Name] = 256
	}
	pool := cpus - report.Policy.HostCPUs - report.Policy.OperationCPUs
	if report.Building {
		pool -= report.Policy.BuildCPUs
	}
	alloc := fairCPU(pool, wanted, weights)
	for _, c := range report.Services {
		if c.Kind == "database" || c.Kind == "cache" || c.Kind == "clickhouse" {
			l := serviceLimit(report.Policy, c.Name, c.Kind)
			desired[c.Name] = struct {
				memory int64
				cpu    float64
				shares int64
			}{l.MemoryMB, alloc["service:"+c.Name], 256}
		}
	}
	for _, c := range containers {
		if c.Kind == "build" && c.Name == state.BuilderName {
			desired[c.Name] = struct {
				memory int64
				cpu    float64
				shares int64
			}{report.Policy.BuildMemoryMB, report.Policy.BuildCPUs, 256}
		}
	}
	// Refuse an explicit reduction below observed usage; do not kill a healthy
	// process to make a requested configuration fit. Old limits can be restored.
	shrink := false
	for _, c := range containers {
		if l, ok := desired[c.Name]; ok && c.Running && (c.MemoryLimit == 0 || l.memory*mib < c.MemoryLimit) {
			shrink = true
		}
	}
	if shrink && admit {
		if err = resourceStats(ctx, containers); err != nil {
			return nil, errors.New("cannot verify memory usage before lowering a container limit")
		}
		for _, c := range containers {
			if l, ok := desired[c.Name]; ok && c.Running && (c.MemoryLimit == 0 || l.memory*mib < c.MemoryLimit) {
				if c.MemoryBytes == nil || *c.MemoryBytes > l.memory*mib*9/10 {
					return nil, fmt.Errorf("%s is too close to the requested memory limit; stop it or choose more RAM", c.Name)
				}
			}
		}
	}
	updated := []ContainerResource{}
	rollback := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, c := range updated {
			args := []string{"update", "--memory", strconv.FormatInt(c.MemoryLimit, 10), "--memory-swap", strconv.FormatInt(c.SwapLimit, 10), "--cpu-shares", strconv.FormatInt(c.CPUShares, 10)}
			if c.NanoCPUs == 0 && c.CPUPeriod > 0 {
				args = append(args, "--cpu-period", strconv.FormatInt(c.CPUPeriod, 10), "--cpu-quota", strconv.FormatInt(c.RawQuota, 10))
			} else {
				args = append(args, "--cpus", strconv.FormatFloat(c.CPUQuota, 'f', 4, 64))
			}
			args = append(args, c.ID)
			_, _ = resourceCommand(cleanup, args...)
		}
	}
	// Lower quotas first, then raise them. Intermediate allocations cannot give
	// an existing process a new app's/worker's share while the old share remains.
	slices.SortStableFunc(containers, func(a, b ContainerResource) int {
		la, oka := desired[a.Name]
		lb, okb := desired[b.Name]
		da, db := false, false
		if oka {
			da = la.cpu < a.CPUQuota
		}
		if okb {
			db = lb.cpu < b.CPUQuota
		}
		if da && !db {
			return -1
		}
		if !da && db {
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	for _, c := range containers {
		l, ok := desired[c.Name]
		if !ok || (!c.Running && pending[c.Name].Name == "") {
			continue
		}
		if (!admit || c.MemoryLimit == l.memory*mib) && math.Abs(c.CPUQuota-math.Floor(l.cpu*100)/100) < 0.001 && c.CPUShares == l.shares {
			continue
		}
		limitArgs := resourceUpdateArgs(c, l.memory, l.cpu, l.shares)
		if !admit {
			keep := []string{}
			for i := 0; i < len(limitArgs); i++ {
				if limitArgs[i] == "--memory" || limitArgs[i] == "--memory-swap" {
					i++
					continue
				}
				keep = append(keep, limitArgs[i])
			}
			limitArgs = keep
		}
		args := append([]string{"update"}, limitArgs...)
		args = append(args, c.ID)
		if _, err = resourceCommand(ctx, args...); err != nil {
			rollback()
			return nil, fmt.Errorf("could not apply resource limits to %s; previous limits restored where possible", c.Name)
		}
		updated = append(updated, c)
	}
	m.invalidateResources()
	if extra != nil {
		l, ok := desired[extra.Name]
		if !ok {
			return nil, errors.New("launch resource allocation unavailable")
		}
		return resourceLimitsArgs(l.memory, l.cpu, l.shares), nil
	}
	return nil, nil
}
func (m *Manager) prepareResourceLaunch(ctx context.Context, a App, name, kind string) ([]string, func(), error) {
	return waitResourceCapacity(ctx, func(wait context.Context) ([]string, func(), error) {
		return m.prepareResourceLaunchOnce(wait, a, name, kind)
	})
}
func (m *Manager) prepareResourceLaunchOnce(ctx context.Context, a App, name, kind string) ([]string, func(), error) {
	m.resourceMu.Lock()
	defer m.resourceMu.Unlock()
	l := a.resourceLimits()
	ram := l.WebMemoryMB
	if kind == "worker" {
		ram = l.WorkerMemoryMB
	}
	launch := resourceLaunch{name, a.ID, kind, ram}
	state := m.resourceState()
	effective := state.Apps[a.ID]
	effective.Stopped = false
	state.Apps[a.ID] = effective
	args, err := m.reconcileResources(ctx, &state, &launch)
	if err != nil {
		return nil, nil, err
	}
	if m.resourcePending == nil {
		m.resourcePending = map[string]resourceLaunch{}
	}
	m.resourcePending[name] = launch
	return args, func() {
		m.resourceMu.Lock()
		defer m.resourceMu.Unlock()
		delete(m.resourcePending, name)
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = m.reconcileResources(cleanup, nil, nil)
		m.invalidateResources()
	}, nil
}
func (m *Manager) prepareServiceResources(ctx context.Context, name, kind string) ([]string, func(), error) {
	return waitResourceCapacity(ctx, func(wait context.Context) ([]string, func(), error) {
		return m.prepareServiceResourcesOnce(wait, name, kind)
	})
}
func (m *Manager) prepareServiceResourcesOnce(ctx context.Context, name, kind string) ([]string, func(), error) {
	m.resourceMu.Lock()
	defer m.resourceMu.Unlock()
	memory, cpus, err := resourceHost()
	if err != nil {
		return nil, nil, err
	}
	state := m.resourceState()
	policy := defaultResourcePolicy(memory, cpus)
	if state.Policy != nil {
		policy = *state.Policy
	}
	l := serviceLimit(policy, name, kind)
	launch := resourceLaunch{Name: name, Kind: kind, MemoryMB: l.MemoryMB}
	args, err := m.reconcileResources(ctx, nil, &launch)
	if err != nil {
		return nil, nil, err
	}
	if m.resourcePending == nil {
		m.resourcePending = map[string]resourceLaunch{}
	}
	m.resourcePending[name] = launch
	return args, func() {
		m.resourceMu.Lock()
		defer m.resourceMu.Unlock()
		delete(m.resourcePending, name)
		m.invalidateResources()
	}, nil
}
func (m *Manager) prepareResourceBuild(ctx context.Context, a App) (ResourcePolicy, func(), error) {
	return waitResourceCapacity(ctx, func(wait context.Context) (ResourcePolicy, func(), error) { return m.prepareResourceBuildOnce(wait, a) })
}
func (m *Manager) prepareResourceBuildOnce(ctx context.Context, a App) (ResourcePolicy, func(), error) {
	// The old rollback container can be restarted on rollback; it need not run
	// alongside the live release and a third candidate throughout compilation.
	if a.Previous != nil {
		ids, err := resourceCommand(ctx, "ps", "-aq", "--filter", "name=^/"+a.Previous.Container+"$")
		if err != nil {
			return ResourcePolicy{}, nil, err
		}
		if ids != "" {
			if _, err := resourceCommand(ctx, "stop", "--time", "10", a.Previous.Container); err != nil {
				return ResourcePolicy{}, nil, errors.New("could not stop idle rollback container before reserving deployment capacity")
			}
		}
	}
	m.resourceMu.Lock()
	defer m.resourceMu.Unlock()
	m.resourceBuilding = true
	if m.resourceStarting == nil {
		m.resourceStarting = map[string]bool{}
	}
	m.resourceStarting[a.ID] = true
	if _, err := m.reconcileResources(ctx, nil, nil); err != nil {
		m.resourceBuilding = false
		delete(m.resourceStarting, a.ID)
		return ResourcePolicy{}, nil, err
	}
	memory, cpus, err := resourceHost()
	if err != nil {
		m.resourceBuilding = false
		delete(m.resourceStarting, a.ID)
		return ResourcePolicy{}, nil, err
	}
	state := m.resourceState()
	policy := defaultResourcePolicy(memory, cpus)
	if state.Policy != nil {
		policy = *state.Policy
	}
	return policy, func() {
		m.resourceMu.Lock()
		defer m.resourceMu.Unlock()
		m.resourceBuilding = false
		delete(m.resourceStarting, a.ID)
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if state.BuilderName != "" {
			ids, e := resourceCommand(cleanup, "ps", "-q", "--filter", "name=^/"+state.BuilderName+"$")
			if e == nil && ids != "" {
				_, _ = resourceCommand(cleanup, "stop", "--time", "5", state.BuilderName)
			}
		}
		_, _ = m.reconcileResources(cleanup, nil, nil)
	}, nil
}
func (m *Manager) saveAppResources(ctx context.Context, id string, l AppResources) error {
	if err := l.Validate(); err != nil {
		return err
	}
	m.resourceMu.Lock()
	defer m.resourceMu.Unlock()
	m.mu.Lock()
	a, ok := m.data.Apps[id]
	busy := m.busy(id) || a.Restoring || a.Retiring
	for _, t := range m.data.Tasks {
		if t.AppID == id && t.Launching {
			busy = true
		}
	}
	for _, p := range m.resourcePending {
		if p.AppID == id {
			busy = true
		}
	}
	if ok && !busy {
		m.lifecycle[id] = true
	}
	m.mu.Unlock()
	if !ok || busy {
		return errors.New("application unavailable or busy")
	}
	defer func() { m.mu.Lock(); delete(m.lifecycle, id); m.mu.Unlock() }()
	state := m.resourceState()
	a.Resources = &l
	state.Apps[id] = a
	if _, err := m.reconcileResources(ctx, &state, nil); err != nil {
		return err
	}
	m.mu.Lock()
	old := m.data.Apps[id]
	m.data.Apps[id] = a
	err := m.save()
	if err != nil {
		m.data.Apps[id] = old
	}
	m.mu.Unlock()
	if err != nil {
		_, _ = m.reconcileResources(ctx, nil, nil)
	}
	m.invalidateResources()
	return err
}
func (m *Manager) saveResourcePolicy(ctx context.Context, p ResourcePolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	m.resourceMu.Lock()
	defer m.resourceMu.Unlock()
	m.helperMu.Lock()
	defer m.helperMu.Unlock()
	if m.helperMemory > 0 {
		return errors.New("wait for active backup/restore/monitoring commands before changing operational headroom")
	}
	if m.resourceBuilding || len(m.resourcePending) > 0 {
		return errors.New("wait for the active build or container launch before changing reservations")
	}
	containers, err := resourceInventory(ctx)
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, c := range containers {
		if c.Kind == "database" || c.Kind == "cache" || c.Kind == "clickhouse" {
			allowed[c.Name] = true
			if l, ok := p.Services[c.Name]; ok && c.Kind == "clickhouse" && l.MemoryMB < 1024 {
				return errors.New("managed ClickHouse requires at least 1024 MiB")
			}
			if l, ok := p.Services[c.Name]; ok && l.MemoryMB < 256 {
				return errors.New("managed PostgreSQL and Valkey services require at least 256 MiB")
			}
		}
	}
	for name := range p.Services {
		if !allowed[name] {
			return errors.New("service overrides must name a managed database or cache on this server")
		}
	}
	state := m.resourceState()
	state.Policy = &p
	if _, err = m.reconcileResources(ctx, &state, nil); err != nil {
		return err
	}
	m.mu.Lock()
	old := m.data.ResourcePolicy
	m.data.ResourcePolicy = &p
	err = m.save()
	if err != nil {
		m.data.ResourcePolicy = old
	}
	m.mu.Unlock()
	if err != nil {
		_, _ = m.reconcileResources(ctx, nil, nil)
	}
	m.invalidateResources()
	return err
}
func (m *Manager) resourceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/resources", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		v, err := m.resourceReport(ctx, true)
		if err != nil {
			Fail(w, 503, err)
			return
		}
		JSON(w, 200, v)
	})
	mux.HandleFunc("PUT /v1/resources", func(w http.ResponseWriter, r *http.Request) {
		var p ResourcePolicy
		if err := Decode(w, r, &p); err != nil {
			Fail(w, 400, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		if err := m.saveResourcePolicy(ctx, p); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "applied"})
	})
	mux.HandleFunc("PUT /v1/apps/{id}/resources", func(w http.ResponseWriter, r *http.Request) {
		var l AppResources
		if err := Decode(w, r, &l); err != nil {
			Fail(w, 400, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		if err := m.saveAppResources(ctx, r.PathValue("id"), l); err != nil {
			Fail(w, 409, err)
			return
		}
		JSON(w, 200, map[string]string{"status": "applied"})
	})
}

func replaceResourceArgs(args, limits []string) []string {
	out := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--memory", "--memory-swap", "--cpus", "--cpu-shares":
			i++
		default:
			out = append(out, args[i])
		}
	}
	return append(out, limits...)
}

func (m *Manager) rollbackResources(ctx context.Context, id string, d *Docker) error {
	m.mu.Lock()
	a, ok := m.data.Apps[id]
	if !ok || a.Retiring || a.Stopped || a.Previous == nil || m.busy(id) || a.Restoring {
		m.mu.Unlock()
		return errors.New("no available previous release")
	}
	m.lifecycle[id] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.lifecycle, id); m.mu.Unlock() }()
	_, done, err := m.prepareResourceLaunch(ctx, a, a.Previous.Container, "web")
	if err != nil {
		return err
	}
	defer done()
	if err = d.SetRunning(ctx, []string{a.Previous.Container}, true); err != nil {
		return err
	}
	ports, err := d.PublishedPorts(ctx, []string{a.Previous.Container})
	if err != nil {
		return err
	}
	previous := *a.Previous
	previous.Port = ports[previous.Container]
	if err = d.waitReady(ctx, &previous); err != nil {
		_, _ = resourceCommand(ctx, "stop", "--time", "10", previous.Container)
		return fmt.Errorf("previous release is unhealthy: %w", err)
	}
	m.mu.Lock()
	old := m.data.Apps[id]
	a = old
	a.Current = &previous
	a.Previous = old.Current
	m.data.Apps[id] = a
	err = m.save()
	if err != nil {
		m.data.Apps[id] = old
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if a.Previous != nil {
		_, _ = resourceCommand(ctx, "stop", "--time", "10", a.Previous.Container)
	}
	return nil
}

func (m *Manager) resourceLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
			m.resourceMu.Lock()
			_, _ = m.reconcileResourcePlan(ctx, nil, nil, false)
			m.resourceMu.Unlock()
			cancel()
		}
	}
}

func resourceUpdateArgs(c ContainerResource, memory int64, cpu float64, shares int64) []string {
	args := resourceLimitsArgs(memory, cpu, shares)
	if c.NanoCPUs == 0 && c.CPUPeriod > 0 {
		out := []string{}
		for i := 0; i < len(args); i++ {
			if args[i] == "--cpus" {
				i++
				continue
			}
			out = append(out, args[i])
		}
		args = append(out, "--cpu-period", "100000", "--cpu-quota", strconv.FormatInt(int64(math.Floor(cpu*100)*1000), 10))
	}
	return args
}

// Monitoring, backup and restore helpers share a fixed pool. CPU is apportioned
// by each helper's fixed RAM reservation; total helper caps cannot exceed it.
func (m *Manager) prepareResourceOperation(ctx context.Context, name string, memory int64) ([]string, func(), error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		m.helperMu.Lock()
		total, cpus, err := resourceHost()
		if err != nil {
			m.helperMu.Unlock()
			return nil, nil, err
		}
		m.mu.Lock()
		p := defaultResourcePolicy(total, cpus)
		if m.data.ResourcePolicy != nil {
			p = *m.data.ResourcePolicy
		}
		m.mu.Unlock()
		if memory > p.OperationMemoryMB {
			m.helperMu.Unlock()
			return nil, nil, errors.New("operational RAM pool is too small for this command")
		}
		available, availableErr := availableHostMemory()
		if availableErr != nil {
			m.helperMu.Unlock()
			return nil, nil, availableErr
		}
		if m.helperMemory+memory <= p.OperationMemoryMB && (p.MemoryMode == "strict" || available >= p.HostMemoryMB+memory) {
			m.helperMemory += memory
			m.helperMu.Unlock()
			args := resourceLimitsArgs(memory, p.OperationCPUs*float64(memory)/float64(p.OperationMemoryMB), 128)
			args = append(args, "--label", "io.lidza.operation=true", "--label", "io.lidza.agent="+resourceBuilderName(m.cfg.DataDir))
			var once sync.Once
			return args, func() {
				once.Do(func() {
					cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_, _ = resourceCommand(cleanup, "rm", "-f", name)
					m.helperMu.Lock()
					m.helperMemory -= memory
					m.helperMu.Unlock()
				})
			}, nil
		}
		m.helperMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
