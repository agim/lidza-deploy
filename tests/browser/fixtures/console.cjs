const app = { id: 'portal', repository: 'acme/portal', branch: 'main', domain: 'portal.example.com', server_id: 'local', github_installation: 42, auto_deploy: true, current: { commit: 'abcdef1234567' }, domain_status: { state: 'ready' } };
const ports = [{app_id:'portal',service:'app',container:'lidza-portal-'+'a'.repeat(48),release:'build-one',declared:true,protocol:'tcp',host_ip:'0.0.0.0',host_port:25,container_port:25,publication:'published',firewall:'allow_rule_installed',external:'unverified'}, {app_id:'portal',service:'app',container:'lidza-portal-'+'a'.repeat(48),release:'build-one',declared:false,protocol:'tcp',host_ip:'127.0.0.1',host_port:32790,container_port:3000,publication:'private_proxy',firewall:'not_required',external:'not_applicable'}];
const now = '2026-10-15T14:00:00Z';
const archive = { id: 'archive-one', app_id: 'legacy', kind: 'container', repository: 'acme/legacy', branch: 'main', deleted_at: now, deleted_by: 'ops@example.com', expires_at: '2027-01-08T14:00:00Z', phase: 'archived', hostnames: ['legacy.example.com'], kept: [{ kind: 'database', name: 'legacy', size_bytes: 4096 }], deleted: [{ kind: 'container', name: 'lidza-legacy' }], kept_bytes: 4096, id_available: true, server_id: 'local', server_name: 'Production', days_left: 84 };
const panelView = { hostname: 'deploy.example.com', public_url: 'https://deploy.example.com', version: 'v1.0.0-private.56', read_only: false, servers: [{ id: 'local', name: 'Production' }, { id: 'combined', name: 'Combined host' }] };
const panelPlan = {
  input: { target: 'combined' }, hostname: 'control-panel.' + 'very-long-subdomain-'.repeat(3) + 'example.com', target_name: 'Combined host', source_server: 'local', source_name: 'Production',
  servers: [
    { id: 'combined', name: 'Combined host', role: 'destination', url: 'http://127.0.0.1:19092', new_url: 'http://127.0.0.1:9092', reachable: true, apps: 4, detail: "Becomes this panel's own server." },
    { id: 'local', name: 'Production', role: 'source', url: 'http://127.0.0.1:9090', reachable: true, retire: true, apps: 0, detail: 'Holds no applications.' },
  ],
  addresses: ['203.0.113.9', '2001:0db8:85a3:0000:0000:8a2e:0370:7334'], data_files: 5, data_bytes: 20480, database: 'managed', database_bytes: 8 * 1024 * 1024,
  ingress: 'caddy', ingress_detail: 'a route through the Caddy admin API on the destination', certificate: 'The current certificate is copied, so HTTPS answers immediately after the switch.', certificate_ok: true, version: 'v1.0.0-private.56',
  checks: [
    { name: 'version', ok: true, detail: 'The destination runs the same release v1.0.0-private.56.' },
    { name: 'disk', ok: true, detail: 'The destination has 42 GiB free, enough for the state and the database.' },
    { name: 'reach', ok: false, detail: 'The destination reaches every paired agent except one over its current URL; the plan rewrites that one to the loopback address.' },
  ],
  blockers: [], warnings: ['Scheduled jobs pause during the cutover.'], ready: true,
};
async function mockConsole(page) {
  const state = { apps: [app, { ...app, id: 'api', repository: 'acme/api', domain: 'api.example.com', auto_deploy: false, stopped: true }], calls: [], granted: false, consoleGranted: false, offline: false, unknown: [], maintenance: { policy: { enabled: true, interval_hours: 24, build_cache: true, logs: true, older_than_days: 7 }, running: false, next_run: '2099-10-15T14:00:00Z', next_backup_cleanup: '2099-10-15T14:00:00Z', messages: [] } };
  await page.addInitScript(() => { window.consoleCopied = ''; Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async value => { window.consoleCopied = value; } } }); });
  await page.route('**/api/**', async route => {
    const request = route.request(), path = new URL(request.url()).pathname.replace('/api/', ''), method = request.method();
    const body = request.postData() ? request.postDataJSON() : undefined;
    state.calls.push({ path, method, body });
    const send = json => route.fulfill({ json });
    if (path === 'v1/auth/preferences') {
      if (method === 'PATCH') state.preferences = { ...(state.preferences || {}), ...body.values };
      return send({ subject: 'fixture-operator', values: state.preferences || {} });
    }
    if (path === 'control/servers/local/maintenance') { if (method === 'PATCH') state.maintenance.policy = body; if (state.maintenance.running && Date.now() - state.maintenanceStarted > 100) { state.maintenance.running = false; state.maintenance.messages = ['Removed 2 older local backups.', 'Total reclaimed space: 2 GB']; state.maintenance.last_run = now; } return send(state.maintenance); }
    if (path === 'control/servers/local/maintenance/run') { state.maintenance.running = true; state.maintenanceStarted = Date.now(); return send(state.maintenance); }
    if (path === 'control/status') return send({ roles: ['admin'], github_app_connected: true, github_app_configured: true, github_public_https: true, hostname: 'deploy.example.com', control_version: 'v0.2.83', user: { subject: 'owner', email: 'ada@example.com', name: 'Ada Lovelace' } });
    if (path === 'control/apps') { if (method === 'POST') { state.apps.push({ ...app, ...body }); return send({ id: body.id }); } return send(state.apps); }
    if (path === 'control/servers') return send([{ id: 'local', name: 'Production', url: 'http://127.0.0.1:9090' }]);
    if (path === 'control/deployments/build-one') return send({id:'build-one',app_id:'portal',server_id:'local',status:'building',created:now,duration_seconds:20,log:'Preparing deployment\nBuilding image\n[fixture output]'});
    if (path === 'control/deployments') return send({ deployments: [{ id: 'build-one', app_id: 'portal', server_id: 'local', status: 'building', created: now, duration_seconds: 20, commit: app.current.commit, log: 'Preparing deployment\nBuilding image\n[fixture output]' }], unavailable_servers: [] });
    if (path === 'control/server-health') return send({ servers: [{ server_id: 'local', checked: now, cpus: 4, cpu_percent: 12.5, memory_total: 8 * 1024 ** 3, memory_available: 5 * 1024 ** 3, load1: .2, disks: [{ path: '/var/lib/docker', total: 100 * 1024 ** 3, available: 30 * 1024 ** 3, used_percent: 70 }], database_bytes: { portal: 20 * 1024 ** 2 } }], unavailable_servers: [] });
    if (path === 'control/github/repos') return send([{ full_name: 'acme/portal', default_branch: 'main', private: true, installation_id: 42 }]);
    if (path === 'control/github/branches') return send([{ name: 'main' }, { name: 'feature/' + 'long-branch-'.repeat(18) }]);
    if (path === 'control/apps/portal/ports') return send({ports});
    if (path === 'control/apps/portal/settings' && method === 'PATCH') {
      // The control panel's answer to a save (internal/agent/settings_apply.go): a
      // build change starts a deployment, a variable restarts, hostnames apply at once.
      const build = body.branch !== app.branch || (body.runtime_profile || 'minimum') !== 'minimum';
      const apply = build ? { applied: 'deploy', deployment_id: 'deploy-settings', deployment: { id: 'deploy-settings', app_id: 'portal', status: 'queued', kind: 'deploy', created: now }, message: 'Settings saved. Deploying them.' }
        : Object.keys(body.env_changes || {}).length ? { applied: 'restart', deployment_id: 'reload-settings', deployment: { id: 'reload-settings', app_id: 'portal', status: 'queued', kind: 'reload', created: now }, message: 'Settings saved. Restarting the application with them.' }
          : { applied: 'immediate', message: 'Settings saved.' };
      return send({ ...app, apply });
    }
    if (path === 'control/deployments/reload-settings') return send({ id: 'reload-settings', app_id: 'portal', server_id: 'local', kind: 'reload', status: 'live', created: now, duration_seconds: 4 });
    if (path === 'control/deployments/deploy-settings') return send({ id: 'deploy-settings', app_id: 'portal', server_id: 'local', kind: 'deploy', status: 'building', created: now, duration_seconds: 2, log: 'Preparing deployment' });
    if (path === 'control/apps/portal/settings') return send({ ...app, apply_effects: { env: 'restart', fields: { branch: 'deploy', build: 'deploy', astro: 'deploy', runtime_profile: 'deploy', toolchains: 'deploy', database_bindings: 'restart', domain: 'restart', additional_domains: 'immediate', customer_hosts: 'immediate', backup_before_deploy: 'immediate', env_kind: 'immediate' } }, public_ports_supported:true, master_key_generation_supported:true,stack_profiles_supported:true,runtime_profile:"minimum",runtime_deployed:true,additional_domains_supported:true, additional_domains:state.aliases||[], domain_statuses:state.domainStatuses||[], environment: [{ key: 'LOG_LEVEL', kind: 'setting', value: 'info' }, { key: 'API_TOKEN', kind: 'credential', preview: 'tok…', length: 18 }, ...(state.masterKey ? [state.masterKey] : [])], database_bindings: { DATABASE_URL: 'portal' }, database: { mode: 'local' }, persistent_storage: true });
    if (path.endsWith('/environment/API_TOKEN/reveal')) return state.granted ? send({ value: 'fixture-secret-value' }) : route.fulfill({ status: 403, json: { error: 'Verify your second factor.', code: 'step_up_required' } });
    if (path === 'v1/auth/step-up') { if (body.purpose === 'credential.reveal') state.granted = true; if (body.purpose === 'server.console') state.consoleGranted = true; return send({ expiresAt: '2099-10-15T14:15:00Z' }); }
    if (path === 'control/servers/local/console/jobs') { state.consoleJobID = body.id; return send({ id: body.id, state: 'running', output: '', next_offset: 0, output_size: 0, duration_seconds: 0 }); }
    if (path.startsWith('control/servers/local/console/jobs/')) return send({ id: state.consoleJobID, state: 'finished', output: 'rows: 2', next_offset: 7, output_size: 7, exit_code: 0, duration_seconds: .2 });
    if (path === 'control/servers/local/console') return method === 'GET' ? send({ two_factor_enabled: true, unlocked: state.consoleGranted, max_runtime_seconds: 600, max_output_bytes: 8388608 }) : send({ output: 'rows: 2', exit_code: 0, duration_seconds: .2 });
    if (path === 'control/servers/local/containers') return send({ checked_at: now, containers: [{ id: 'container-one', ports, name: 'lidza-portal-' + 'a'.repeat(48), app_id: 'portal', role: 'application', state: 'running', health: 'healthy', cpu: '12.5%', memory: '80 MiB / 1 GiB', memory_percent: 8, restartable: true }] });
    if (path === 'control/servers/local/host/resources' || path === 'control/servers/local/host/resources/overrides') { const policy = { max_use_bytes: 4294967296, keep_free_bytes: 19299041280, max_file_bytes: 536870912, retention: '30day' }; return send({ supported: true, host: { cpus: 8, memory_mib: 32768, disk_gib: 359 }, apps: 4, computed: now, overrides: {}, changes: [], journal: { disk_path: '/var/log/journal', disk_bytes: 386000000000, computed: policy, target: policy, drop_in: '/etc/systemd/journald.conf.d/zz-lidza-journal.conf', usage_bytes: 4100000000, last_applied: now, neutralized: [{ file: '/etc/systemd/journald.conf.d/99-zz-retention-30d.conf', lines: ['SystemMaxUse=100G', 'SystemKeepFree=50G', 'MaxRetentionSec=30day'], backup: '/var/lib/lidza-agent/journald-backups/99-zz-retention-30d.conf.20261106T140000Z', at: now }] }, jobs: [{ id: 'journal-policy-boot-1', kind: 'journal-policy', trigger: 'boot', status: 'succeeded', started: now, summary: 'System log size set to 4G.' }] }); }
    if (path === 'control/servers/local/host/service-control') { state.serviceControl ||= { supported: true, enabled: false, source: 'config', config_default: false, account: 'deploy' }; if (method === 'PUT') state.serviceControl = { ...state.serviceControl, enabled: body.enabled, source: 'setting' }; return send(state.serviceControl); }
    if (path === 'control/servers/local/host/log-access') { state.logAccess ||= { supported: true, enabled: false, source: 'config', account: 'deploy', group: 'systemd-journal', member: false }; if (method === 'PUT') state.logAccess = { ...state.logAccess, enabled: body.enabled, member: body.enabled, source: 'setting' }; return send(state.logAccess); }
    if (path === 'control/servers/local/upgrade') return send({ current: 'v0.2.83', latest: 'v0.2.84', state: 'idle', supported: true });
    if (path === 'control/servers/local/ssh-access') return send({ keys: [{ id: 'operator', name: 'Operator', fingerprint: 'SHA256:fixture-key' }] });
    if (path === 'control/apps/portal/master-key') { if (state.masterKey) return route.fulfill({status:409,json:{error:'Master key already configured'}}); state.masterKey={key:'LIDZA_MASTER_KEY',kind:'credential',preview:'abcd…',length:64,source:'built-in'}; return send({ ...state.masterKey, apply: { applied: 'restart', deployment_id: 'reload-settings', message: 'Settings saved. Restarting the application with them.' } }); }
    if (path === 'control/apps/portal/logs') return send({ logs: 'Application started\nServing requests' });
    if (path === 'control/databases') return send({ databases: [{ app_id: 'portal', server_id: 'local', mode: 'local', ready: true, excluded_tables_supported: true, excluded_tables: [], attachments: [{ app_id: 'portal', env_key: 'DATABASE_URL' }], backup: { hours: 24, keep: 7, offsite: true }, backups: [{ id: 'backup-one', created: now, size: 4096, offsite: false, upload_error: 'Storage unavailable' }] }] });
    if (path === 'control/storage/backups') return send({ bucket: 'private-backups', prefix: 'lidza', checked: now, objects: [{ key: 'databases/portal/' + 'long-backup-name-'.repeat(12) + '.dump', prefix: 'lidza', size: 4096, modifiedAt: now }] });
    if (path === 'control/apps/portal/cache') return send({ mode: 'automatic', configured: true, ready: true });
    if (path === 'control/infrastructure') return send({ storage: { configured: true, endpoint: 'https://nyc3.digitaloceanspaces.com', bucket: 'private-backups', region: 'nyc3', prefix: 'lidza', access_key_stored: true, secret_key_stored: true }, mail: { configured: true, host: 'smtp.example.com', port: 587, security: 'starttls', from: 'ops@example.com', username_stored: true, password_stored: true } });
    if (path === 'control/application-defaults') return send({ values: { LOG_LEVEL: 'info' } });
    if (path === 'control/storage/retention' && method === 'GET') return send({ settings: { daily: 7, weekly: 4, monthly: 0, orphan_grace_days: 30, automatic: true }, reviewed: true, defaults: { daily: 7, weekly: 4, monthly: 0, orphan_grace_days: 30, automatic: true } });
    if (method === 'GET' && /^control\/servers\/[^/]+\/databases\/[^/]+\/tables$/.test(path)) return send({ tables: [{ name: 'users', rows: 1200, bytes: 4 * 1024 ** 2, excludable: false, excluded: false }, { name: 'request_logs', rows: 2400000, bytes: 3 * 1024 ** 3, excludable: true, excluded: false }], total: 2, excluded: [] });
    if (path === 'control/activity') return send({ items: [], generated: now, unavailable_servers: [] });
    if (method === 'GET' && /^control\/apps\/[^/]+\/redirects$/.test(path)) return send({ redirects: [], hostnames: ['portal.example.com'] });
    if (path === 'control/repos/detect') return send({ repository: 'acme/portal', branch: 'main', default_branch: 'main', branches: ['main', 'release'], commit: 'abcdef1', installation_id: 42, detected: true, mode: 'container', needs_postgres: false, needs_redis: false, reasons: ['go.mod: Līdza (Go) application'] });
    if (path === 'control/team') return send({ members: [{ id: 'owner', name: 'Operator', email: 'ops@example.com', role: 'admin', owner: true }] });
    if (path === 'control/audit') return send({ entries: [{ id: 'audit-one', action: 'server.console.unlock', actor: 'ops@example.com', created: now, target: 'local' }] });
    if (path === 'control/updates') return send({ control_version: 'v0.2.83', settings: { latest_version: 'v0.2.84', automatic: false }, servers: [{ server_id: 'local', name: 'Production', current: 'v0.2.83', state: 'idle' }] });
    if (path.match(/^control\/apps\/[^/]+\/(errors|security)$/)) return send({ status: 'ready', last_received: now, errors: [{ id: 'event-one', fingerprint: 'fixture-error', source: 'server', message: 'Fixture request rejected', method: 'GET', route: '/api/staging/.env', requestId: 'request-fixture', createdAt: now, container: 'lidza-portal', release: 'build-one', stack: 'Error: fixture diagnostic\n  at handler.go:42' }] });
    if (path === 'v1/auth/two-factor') return send({ enabled: false, backupCodesLeft: 8 });
    if (path === 'v1/auth/two-factor/setup') return send({ secret: 'FIXTUREONLY', otpauthUrl: 'otpauth://totp/Fixture?secret=FIXTUREONLY', qrCodeDataUrl: 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9WlJ9l8AAAAASUVORK5CYII=' });
    // No move recorded: the control answers 204 (not 404, which browsers log as a console error).
    if (method === 'GET' && /^control\/apps\/[^/]+\/move$/.test(path)) return route.fulfill({ status: 204 });
    if (method === 'GET' && /^control\/apps\/[^/]+\/processes$/.test(path)) return send({ processes: [] });
    // Control panel move: two paired servers; the plan review shows checks, the summary and the server table.
    if (path === 'control/panel-move') return send(panelView);
    if (path === 'control/panel-move/plan') return send(panelPlan);
    if (path === 'control/archives') return send({ archives: [archive], purged: [], retention_days: 90, unavailable_servers: [] });
    if (/^control\/servers\/[^/]+\/host\/ruby\/bundlers$/.test(path)) return send({ groups: [{ user: 'deploy', ruby: '3.3.6', rubygems: '3.5.22', default_bundler: '2.5.22', apps: ['portal'], remove: ['2.4.1'], keep: [] }] });
    if (method !== 'GET') return send({ status: 'accepted', codes: ['fixture-recovery-code'] });
    state.unknown.push(path); return send({});
  });
  return state;
}
module.exports = { mockConsole, app };
