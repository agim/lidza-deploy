package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// RestoreArchive recreates a deleted application from its archive: the
// settings, the files and databases with their logins enabled, an empty
// cache, a native application's account and units, and the routes of the
// hostnames no other application took meanwhile. The panel then deploys
// it. A restore that failed part way is finished by calling it again.
func (m *Manager) RestoreArchive(ctx context.Context, archiveID string, req ArchiveRestoreRequest) (ArchiveRestoreResult, error) {
	if m.upgradePending() {
		return ArchiveRestoreResult{}, errors.New("agent upgrade in progress; restore after it finishes")
	}
	if !m.claimArchive(archiveID) {
		return ArchiveRestoreResult{}, errors.New("an operation on this deleted application is running")
	}
	defer m.releaseArchive(archiveID)
	plan, err := m.beginRestore(archiveID, req)
	if err != nil {
		return ArchiveRestoreResult{}, err
	}
	m.archiveLog(archiveID, "Restore started as %s; %d hostnames available, %d taken by other applications.", plan.AppID, len(plan.Hostnames), len(plan.Skipped))
	if plan.Adopted {
		err = m.restoreAdopted(ctx, archiveID, plan)
	} else if plan.Native {
		err = m.restoreNative(ctx, archiveID, plan)
	} else {
		err = m.restoreContainer(ctx, archiveID, plan)
	}
	if err != nil {
		_ = m.updateArchive(archiveID, func(a *AppArchive) { a.Error = err.Error() })
		m.archiveLog(archiveID, "Restore stopped: %v. Restore again to finish.", err)
		return ArchiveRestoreResult{}, err
	}
	m.wakeDomains()
	return plan, nil
}

// beginRestore checks the restore and records its plan, or returns the
// plan of the restore that is not finished.
func (m *Manager) beginRestore(archiveID string, req ArchiveRestoreRequest) (ArchiveRestoreResult, error) {
	control := m.controlState()
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.data.AppArchives[archiveID]
	if !ok {
		return ArchiveRestoreResult{}, errArchiveNotFound
	}
	switch a.Phase {
	case archivePhaseRestoring:
		if a.Restored != nil {
			return *a.Restored, nil
		}
	case archivePhaseArchived:
	case archivePhasePurging:
		return ArchiveRestoreResult{}, errors.New("this deleted application is being purged")
	default:
		return ArchiveRestoreResult{}, errors.New("the deletion is not finished; delete the application again to finish it first")
	}
	if !time.Now().Before(a.ExpiresAt) {
		return ArchiveRestoreResult{}, errors.New("the archive expired; it is purged")
	}
	target := a.AppID
	if req.NewID != "" {
		if a.Kind == archiveKindNative || a.Kind == archiveKindAdopted {
			return ArchiveRestoreResult{}, errors.New("a native application is restored under its own id")
		}
		if !idPattern.MatchString(req.NewID) {
			return ArchiveRestoreResult{}, errors.New("id must be a lowercase slug, up to 48 characters")
		}
		target = req.NewID
	}
	if _, used := m.data.Apps[target]; used {
		return ArchiveRestoreResult{}, fmt.Errorf("an application named %s exists; restore under another id", target)
	}
	if _, used := m.data.NativeApps[target]; used {
		return ArchiveRestoreResult{}, fmt.Errorf("an application named %s exists; restore under another id", target)
	}
	plan := ArchiveRestoreResult{AppID: target, Skipped: map[string]string{}, Commit: a.Commit, Native: a.Kind == archiveKindNative || a.Kind == archiveKindAdopted, Adopted: a.Kind == archiveKindAdopted, At: time.Now().UTC()}
	owner := func(host string) string {
		if o := m.hostnameOwnerLocked(host, "", control); o != "" {
			return o
		}
		return req.Taken[strings.ToLower(host)]
	}
	primary := ""
	var own []string
	if a.App != nil {
		own = a.App.Hostnames()
	} else {
		own = append([]string{a.Native.App.Domain}, a.Native.App.AdditionalDomains...)
	}
	if req.Domain != "" {
		domain := strings.ToLower(strings.TrimSpace(req.Domain))
		if err := validateHostname(domain); err != nil {
			return ArchiveRestoreResult{}, fmt.Errorf("%s: %w", domain, err)
		}
		if o := owner(domain); o != "" {
			return ArchiveRestoreResult{}, fmt.Errorf("%s is used by %s", domain, o)
		}
		primary = domain
		plan.Hostnames = append(plan.Hostnames, domain)
	}
	for _, host := range own {
		if host == primary {
			continue
		}
		if o := owner(host); o != "" {
			plan.Skipped[host] = o
			continue
		}
		if primary == "" {
			primary = host
		}
		plan.Hostnames = append(plan.Hostnames, host)
	}
	if primary == "" {
		return ArchiveRestoreResult{}, errors.New("every hostname of the application is now used by another application; choose a new primary domain")
	}
	for _, host := range RedirectHosts(a.Redirects) {
		if o := owner(host); o != "" || slices.Contains(plan.Hostnames, host) {
			if o == "" {
				o = target
			}
			plan.Skipped[host] = o
		}
	}
	if len(plan.Skipped) == 0 {
		plan.Skipped = nil
	}
	if plan.Adopted && (req.Domain != "" || len(plan.Skipped) > 0) {
		// Its web server routes and environment name these hostnames.
		taken := slices.Sorted(maps.Keys(plan.Skipped))
		return ArchiveRestoreResult{}, fmt.Errorf("an application adopted from the host setup is restored with all its hostnames; %s %s used by other applications now", strings.Join(taken, ", "), map[bool]string{true: "is", false: "are"}[len(taken) == 1])
	}
	previous := a
	a.Phase = archivePhaseRestoring
	a.Error = ""
	a.Steps = nil
	a.RestoreRequest = &req
	a.Restored = &plan
	m.data.AppArchives[archiveID] = a
	if err := m.save(); err != nil {
		m.data.AppArchives[archiveID] = previous
		return ArchiveRestoreResult{}, err
	}
	return plan, nil
}

// restoredRedirects are the archive's redirects whose hosts are still free.
func restoredRedirects(a AppArchive, plan ArchiveRestoreResult) []RedirectDomain {
	var out []RedirectDomain
	for _, d := range a.Redirects {
		if _, skipped := plan.Skipped[d.Host]; !skipped {
			out = append(out, d)
		}
	}
	return out
}

// restoredHostnames splits the plan's hostnames into the primary and the
// additional ones, in the archive's order.
func restoredHostnames(plan ArchiveRestoreResult) (string, []string) {
	return plan.Hostnames[0], slices.Clone(plan.Hostnames[1:])
}

func (m *Manager) restoreContainer(ctx context.Context, archiveID string, plan ArchiveRestoreResult) error {
	a := m.archiveSnapshot(archiveID)
	steps := []struct {
		name string
		run  func() error
	}{
		{"databases", func() error {
			m.mu.Lock()
			r := m.data.RetainedResources[a.Retained]
			var containers []dockerObject
			for _, dbID := range a.Databases {
				if d, ok := m.data.Databases[dbID]; ok && d.Mode == "local" && d.Network != "" && d.Removed.IsZero() {
					containers = append(containers, databaseObjects(d)[0])
				}
			}
			if r.ClickHouse.Network != "" {
				containers = append(containers, clickhouseObjects(r.ClickHouse)[0])
			}
			m.mu.Unlock()
			for _, o := range containers {
				if _, err := command(ctx, "", nil, "docker", "update", "--restart", "unless-stopped", o.name); err != nil {
					return fmt.Errorf("could not restore the restart policy of %s", o.name)
				}
				if _, err := command(ctx, "", nil, "docker", "start", o.name); err != nil {
					return fmt.Errorf("could not start %s", o.name)
				}
			}
			return nil
		}},
		{"cache", func() error {
			if a.Cache == nil || a.Cache.Mode != "local" {
				return nil
			}
			c := *a.Cache
			c.Ready = false
			if err := m.provisionCache(ctx, c); err != nil {
				return fmt.Errorf("could not recreate the cache: %w", err)
			}
			return nil
		}},
		{"state", func() error { return m.restoreContainerState(archiveID, plan) }},
	}
	for _, step := range steps {
		if err := m.archiveStep(archiveID, "restore-"+step.name, step.run); err != nil {
			return err
		}
	}
	m.finishArchiveRestore(ctx, archiveID, plan)
	return nil
}

// restoreContainerState puts the application back into the agent's state
// with its data attached and forgets the archive.
func (m *Manager) restoreContainerState(archiveID string, plan ArchiveRestoreResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.data.AppArchives[archiveID]
	if _, used := m.data.Apps[plan.AppID]; used {
		return fmt.Errorf("an application named %s exists", plan.AppID)
	}
	before := m.resourceSnapshotLocked()
	beforeArchives := maps.Clone(m.data.AppArchives)
	beforeDatabases := maps.Clone(m.data.Databases)
	app := cloneJSON(*a.App)
	app.ID = plan.AppID
	app.Domain, app.AdditionalDomains = restoredHostnames(plan)
	app.Current, app.Previous, app.Retiring, app.Restoring = nil, nil, false, false
	app.DomainStatus, app.DomainStatuses, app.RedirectDomains = nil, nil, nil
	// The gateway identity was deleted; the next deployment issues a new one.
	for key := range app.Env {
		if strings.HasPrefix(key, gatewayEnvPrefix) {
			delete(app.Env, key)
			delete(app.EnvKinds, key)
			delete(app.EnvSources, key)
		}
	}
	if err := app.Validate(); err != nil {
		return fmt.Errorf("the archived settings are invalid: %w", err)
	}
	if m.data.StorageVolumes == nil {
		m.data.StorageVolumes = map[string]string{}
	}
	if m.data.StackVolumes == nil {
		m.data.StackVolumes = map[string][]string{}
	}
	if m.data.ClickHouses == nil {
		m.data.ClickHouses = map[string]ClickHouseResource{}
	}
	r, held := m.data.RetainedResources[a.Retained]
	if held {
		if r.StorageVolume != "" {
			m.data.StorageVolumes[app.ID] = r.StorageVolume
		}
		if len(r.StackVolumes) > 0 {
			m.data.StackVolumes[app.ID] = slices.Clone(r.StackVolumes)
		}
		if r.ClickHouse.Network != "" {
			m.data.ClickHouses[app.ID] = r.ClickHouse
		}
		delete(m.data.RetainedResources, a.Retained)
	}
	if a.Cache != nil {
		c := *a.Cache
		c.Ready = true
		if m.data.Caches == nil {
			m.data.Caches = map[string]CacheResource{}
		}
		m.data.Caches[app.ID] = c
	}
	if a.StackSelection != nil {
		if m.data.StackSelections == nil {
			m.data.StackSelections = map[string]*StackSelectionSnapshot{}
		}
		selection := cloneJSON(*a.StackSelection)
		m.data.StackSelections[app.ID] = &selection
	}
	if m.data.Tasks == nil {
		m.data.Tasks = map[string]Task{}
	}
	for _, t := range a.Tasks {
		t.AppID = app.ID
		t.Container, t.Release, t.Running, t.Launching, t.SeenKeys = "", "", false, false, nil
		m.data.Tasks[t.ID] = t
	}
	if redirects := restoredRedirects(a, plan); len(redirects) > 0 {
		if m.data.Redirects == nil {
			m.data.Redirects = map[string][]RedirectDomain{}
		}
		m.data.Redirects[app.ID] = redirects
	}
	for _, dbID := range a.Databases {
		if d, ok := m.data.Databases[dbID]; ok && (d.Backup.Hours > 0 || d.Backup.Cron != "") {
			d.NextBackup = d.Backup.NextBackupAfter(time.Now())
			m.data.Databases[dbID] = d
		}
	}
	m.data.Apps[app.ID] = app
	delete(m.data.AppArchives, archiveID)
	if err := m.save(); err != nil {
		m.data = before
		m.data.AppArchives = beforeArchives
		m.data.Databases = beforeDatabases
		return err
	}
	return nil
}

func (m *Manager) restoreNative(ctx context.Context, archiveID string, plan ArchiveRestoreResult) error {
	nativeCreateMu.Lock()
	defer nativeCreateMu.Unlock()
	a := m.archiveSnapshot(archiveID)
	id, native := a.AppID, a.Native
	root := native.Policy.Root
	var account NativeAppAccount
	steps := []struct {
		name string
		run  func() error
	}{
		{"files", func() error {
			if native.Files == "" {
				return nil
			}
			if _, err := os.Lstat(root); err == nil {
				return fmt.Errorf("%s exists; move it away before restoring", root)
			}
			if err := os.Rename(native.Files, root); err != nil {
				return fmt.Errorf("could not move the application files back: %w", err)
			}
			_ = os.Remove(filepath.Dir(native.Files))
			return nil
		}},
		{"port", func() error { return m.restoreNativePort(archiveID, account) }},
		{"data", func() error { return m.restoreHostData(ctx, id, account) }},
		{"units", func() error {
			a := m.archiveSnapshot(archiveID)
			specs := make([]nativeUnitSpec, 0, len(a.Native.Spec.Processes))
			for _, process := range a.Native.Spec.Processes {
				specs = append(specs, nativeUnitSpec{App: id, Process: process.Name, User: nativeAppUser(id), Root: root,
					Command: process.Command, MemoryMiB: a.Native.Spec.MemoryMiB, CPUPercent: a.Native.Spec.CPUPercent, Ruby: a.Native.Policy.Runtime == "rails"})
			}
			_, err := m.writeNativeUnits(ctx, specs)
			return err
		}},
		{"leftovers", func() error {
			leftovers := m.archiveSnapshot(archiveID).Native.Leftovers
			if err := m.restoreLeftoverFiles(ctx, leftovers); err != nil {
				return err
			}
			return m.restoreLeftoverStates(ctx, leftovers, nil)
		}},
		{"route", func() error {
			primary, additional := restoredHostnames(plan)
			_, err := m.publishNativeCaddyRoute(ctx, id, primary, additional)
			return err
		}},
		{"error-routes", func() error {
			return m.restoreErrorRoutes(ctx, m.archiveSnapshot(archiveID).Native.Leftovers, plan.Hostnames)
		}},
		{"logger-names", func() error {
			return m.restoreLoggerNames(ctx, m.archiveSnapshot(archiveID).Native.Leftovers, plan.Hostnames)
		}},
		{"reservations", func() error { return m.restoreArchiveReservations(archiveID, plan) }},
		{"state", func() error { return m.restoreNativeState(ctx, archiveID, plan) }},
	}
	m.mu.Lock()
	if m.nativeOperations[id] {
		m.mu.Unlock()
		return errors.New("an operation on this application is running")
	}
	if m.nativeOperations == nil {
		m.nativeOperations = map[string]bool{}
	}
	m.nativeOperations[id] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.nativeOperations, id); m.mu.Unlock() }()
	for i, step := range steps {
		if err := m.archiveStep(archiveID, "restore-"+step.name, step.run); err != nil {
			return err
		}
		if i == 0 {
			// The account is recreated on every attempt: ensuring it is
			// idempotent, and the later steps need it. A root left with
			// the removed account's owner is handed to the new one.
			var err error
			if account, err = ensureNativeAppAccount(ctx, m, id); err != nil {
				return err
			}
		}
	}
	m.finishArchiveRestore(ctx, archiveID, plan)
	return nil
}

// restoreNativePort keeps the application's loopback port, or gives it a
// free one (and rewrites the environment file) when another application
// took it meanwhile.
func (m *Manager) restoreNativePort(archiveID string, account NativeAppAccount) error {
	a := m.archiveSnapshot(archiveID)
	port := a.Native.Policy.Port
	if port == "" {
		return nil
	}
	m.mu.Lock()
	taken := m.reservedPortsLocked()[port]
	next := port
	var err error
	if taken {
		next, err = m.allocateNativePortLocked()
	}
	m.mu.Unlock()
	if err != nil || !taken {
		return err
	}
	envFile := filepath.Join(a.Native.Policy.Root, "shared", ".env")
	data, err := os.ReadFile(envFile)
	if err != nil {
		return fmt.Errorf("could not read the environment file: %w", err)
	}
	values := map[string]string{}
	if err := parseNativeLiteralEnvironment(data, values); err != nil {
		return err
	}
	values["PORT"] = next
	if _, ok := values["LIDZA_ADDR"]; ok {
		values["LIDZA_ADDR"] = "127.0.0.1:" + next
	}
	if err := writeNativeEnvironmentFile(envFile, account, values); err != nil {
		return err
	}
	m.archiveLog(archiveID, "Port %s is used by another application; the application listens on %s now.", port, next)
	return m.updateArchive(archiveID, func(x *AppArchive) {
		x.Native.Policy.Port, x.Native.App.Port = next, next
	})
}

// restoreHostData enables the logins of the application's host databases
// again and recreates its Redis instance, empty, with the same password.
func (m *Manager) restoreHostData(ctx context.Context, id string, account NativeAppAccount) error {
	m.mu.Lock()
	record, ok := m.data.NativeHostData[id]
	server := m.data.NativeClickHouse
	m.mu.Unlock()
	if !ok {
		return nil
	}
	h := nativeHostDataHost(m)
	if pg := record.Postgres; pg != nil {
		script, err := renderNativePGLoginSQL(pg.Name, true)
		if err != nil {
			return err
		}
		if _, err := h.psql(ctx, pg.Port, script); err != nil {
			return errors.New("enabling the PostgreSQL login failed; retry")
		}
	}
	if ch := record.ClickHouse; ch != nil && server != nil && server.Installed {
		admin, err := m.unsealHostSecret(server.Password)
		if err != nil {
			return err
		}
		client := h.clickhouse(*server, "default", admin)
		for _, statement := range renderNativeClickHouseLoginSQL(ch.Name, true) {
			if err := client.exec(ctx, statement); err != nil {
				return fmt.Errorf("enabling the ClickHouse login failed; retry: %w", err)
			}
		}
	}
	if redis := record.Redis; redis != nil {
		password, err := m.unsealHostSecret(redis.Password)
		if err != nil {
			return err
		}
		if err := h.ensureRedis(ctx, id, account, password, redis.MaxMemoryMiB, redis.Policy); err != nil {
			return fmt.Errorf("could not recreate the Redis instance: %w", err)
		}
	}
	return m.updateHostDataRecord(id, func(r *NativeHostDataRecord) {
		if r.Postgres != nil {
			r.Postgres.Ready = true
		}
		if r.ClickHouse != nil {
			r.ClickHouse.Ready = true
		}
		if r.Redis != nil {
			r.Redis.Ready = true
		}
	})
}

func (m *Manager) restoreNativeState(ctx context.Context, archiveID string, plan ArchiveRestoreResult) error {
	m.mu.Lock()
	a := m.data.AppArchives[archiveID]
	native := cloneJSON(*a.Native)
	id := a.AppID
	primary, additional := restoredHostnames(plan)
	native.App.Domain, native.App.AdditionalDomains = primary, additional
	native.Policy.Domain = primary
	native.Spec.Domain, native.Spec.AdditionalDomains = primary, slices.Clone(additional)
	for _, table := range []func(){
		func() {
			if m.data.NativeCreated == nil {
				m.data.NativeCreated = map[string]NativeReleasePolicy{}
			}
		},
		func() {
			if m.data.NativeApps == nil {
				m.data.NativeApps = map[string]NativeApplication{}
			}
		},
		func() {
			if m.data.NativeSettings == nil {
				m.data.NativeSettings = map[string]NativeSettingsOverride{}
			}
		},
		func() {
			if m.data.NativeCreatedSpecs == nil {
				m.data.NativeCreatedSpecs = map[string]NativeCreateRequest{}
			}
		},
		func() {
			if m.data.NativeReleases == nil {
				m.data.NativeReleases = map[string]NativeReleaseHistory{}
			}
		},
		func() {
			if m.data.NativeBindings == nil {
				m.data.NativeBindings = map[string]map[string]string{}
			}
		},
		func() {
			if m.data.Tasks == nil {
				m.data.Tasks = map[string]Task{}
			}
		},
	} {
		table()
	}
	before := m.resourceSnapshotLocked()
	keep := struct {
		created  map[string]NativeReleasePolicy
		apps     map[string]NativeApplication
		settings map[string]NativeSettingsOverride
		specs    map[string]NativeCreateRequest
		releases map[string]NativeReleaseHistory
		bindings map[string]map[string]string
		archives map[string]AppArchive
	}{maps.Clone(m.data.NativeCreated), maps.Clone(m.data.NativeApps), maps.Clone(m.data.NativeSettings), maps.Clone(m.data.NativeCreatedSpecs),
		maps.Clone(m.data.NativeReleases), maps.Clone(m.data.NativeBindings), maps.Clone(m.data.AppArchives)}
	m.data.NativeCreated[id] = native.Policy
	m.data.NativeApps[id] = native.App
	m.data.NativeCreatedSpecs[id] = native.Spec
	if native.Settings != nil {
		m.data.NativeSettings[id] = *native.Settings
	}
	if native.Releases != nil {
		m.data.NativeReleases[id] = *native.Releases
	}
	if native.Bindings != nil {
		m.data.NativeBindings[id] = native.Bindings
	}
	for _, t := range a.Tasks {
		t.Container, t.Release, t.Running, t.Launching, t.SeenKeys = "", "", false, false, nil
		m.data.Tasks[t.ID] = t
	}
	redirects := restoredRedirects(a, plan)
	if len(redirects) > 0 {
		if m.data.Redirects == nil {
			m.data.Redirects = map[string][]RedirectDomain{}
		}
		m.data.Redirects[id] = redirects
	}
	delete(m.data.RetainedResources, a.Retained)
	delete(m.data.AppArchives, archiveID)
	err := m.save()
	if err != nil {
		m.data = before
		m.data.NativeCreated, m.data.NativeApps, m.data.NativeSettings, m.data.NativeCreatedSpecs = keep.created, keep.apps, keep.settings, keep.specs
		m.data.NativeReleases, m.data.NativeBindings, m.data.AppArchives = keep.releases, keep.bindings, keep.archives
	}
	m.mu.Unlock()
	if err == nil && len(redirects) > 0 {
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, syncErr := m.syncRedirectIngress(bounded, id); syncErr != nil {
			return fmt.Errorf("restored; the redirect domains are published by the next reconcile: %w", syncErr)
		}
	}
	return err
}

// finishArchiveRestore publishes redirects of a restored container application
// and records the restore in the agent log (the archive is gone).
func (m *Manager) finishArchiveRestore(ctx context.Context, archiveID string, plan ArchiveRestoreResult) {
	if !plan.Native {
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		if _, err := m.syncRedirectIngress(bounded, plan.AppID); err != nil {
			log.Printf("archive %s: redirects of %s are published by the next reconcile: %v", archiveID, plan.AppID, err)
		}
		cancel()
	}
	skipped := slices.Sorted(maps.Keys(plan.Skipped))
	log.Printf("archive %s: restored as %s; hostnames %s; skipped %s", archiveID, plan.AppID, strings.Join(plan.Hostnames, ", "), strings.Join(skipped, ", "))
}
