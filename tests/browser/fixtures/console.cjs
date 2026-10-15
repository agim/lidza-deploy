const app = { id: 'portal', repository: 'acme/portal', branch: 'main', domain: 'portal.example.com', server_id: 'local', github_installation: 42, auto_deploy: true, current: { commit: 'abcdef1234567' }, domain_status: { state: 'ready' } };
const now = '2026-10-15T14:00:00Z';
async function mockConsole(page) {
  const state = { apps: [app, { ...app, id: 'api', repository: 'acme/api', domain: 'api.example.com', auto_deploy: false, stopped: true }], calls: [], granted: false, consoleGranted: false, offline: false, unknown: [], maintenance: { policy: { enabled: true, interval_hours: 24, build_cache: true, logs: true, older_than_days: 7 }, running: false, next_run: '2099-10-15T14:00:00Z', next_backup_cleanup: '2099-10-15T14:00:00Z', messages: [] } };
  await page.addInitScript(() => { window.consoleCopied = ''; Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async value => { window.consoleCopied = value; } } }); });
  await page.route('**/api/**', async route => {
    const request = route.request(), path = new URL(request.url()).pathname.replace('/api/', ''), method = request.method();
    const body = request.postData() ? request.postDataJSON() : undefined;
    state.calls.push({ path, method, body });
    const send = json => route.fulfill({ json });
    if (path === 'control/servers/local/maintenance') { if (method === 'PATCH') state.maintenance.policy = body; if (state.maintenance.running && Date.now() - state.maintenanceStarted > 100) { state.maintenance.running = false; state.maintenance.messages = ['Removed 2 older local backups.', 'Total reclaimed space: 2 GB']; state.maintenance.last_run = now; } return send(state.maintenance); }
    if (path === 'control/servers/local/maintenance/run') { state.maintenance.running = true; state.maintenanceStarted = Date.now(); return send(state.maintenance); }
    if (path === 'control/status') return send({ roles: ['admin'], github_app_connected: true, github_app_configured: true, github_public_https: true });
    if (path === 'control/apps') { if (method === 'POST') { state.apps.push({ ...app, ...body }); return send({ id: body.id }); } return send(state.apps); }
    if (path === 'control/servers') return send([{ id: 'local', name: 'Production', url: 'http://127.0.0.1:9090' }]);
    if (path === 'control/deployments') return send({ deployments: [{ id: 'build-one', app_id: 'portal', server_id: 'local', status: 'building', created: now, duration_seconds: 20, commit: app.current.commit, log: 'Preparing deployment\nBuilding image\n[fixture output]' }], unavailable_servers: [] });
    if (path === 'control/server-health') return send({ servers: [{ server_id: 'local', checked: now, cpus: 4, cpu_percent: 12.5, memory_total: 8 * 1024 ** 3, memory_available: 5 * 1024 ** 3, load1: .2, disks: [{ path: '/var/lib/docker', total: 100 * 1024 ** 3, available: 30 * 1024 ** 3, used_percent: 70 }], database_bytes: { portal: 20 * 1024 ** 2 } }], unavailable_servers: [] });
    if (path === 'control/github/repos') return send([{ full_name: 'acme/portal', default_branch: 'main', private: true, installation_id: 42 }]);
    if (path === 'control/github/branches') return send([{ name: 'main' }, { name: 'feature/' + 'long-branch-'.repeat(18) }]);
    if (path === 'control/apps/portal/settings') return send({ ...app, environment: [{ key: 'LOG_LEVEL', kind: 'setting', value: 'info' }, { key: 'API_TOKEN', kind: 'credential', preview: 'tok…', length: 18 }], database_bindings: { DATABASE_URL: 'portal' }, database: { mode: 'local' }, persistent_storage: true });
    if (path.endsWith('/environment/API_TOKEN/reveal')) return state.granted ? send({ value: 'fixture-secret-value' }) : route.fulfill({ status: 403, json: { error: 'Verify your second factor.', code: 'step_up_required' } });
    if (path === 'v1/auth/step-up') { if (body.purpose === 'credential.reveal') state.granted = true; if (body.purpose === 'server.console') state.consoleGranted = true; return send({ expiresAt: '2099-10-15T14:15:00Z' }); }
    if (path === 'control/servers/local/console/jobs') { state.consoleJobID = body.id; return send({ id: body.id, state: 'running', output: '', next_offset: 0, output_size: 0, duration_seconds: 0 }); }
    if (path.startsWith('control/servers/local/console/jobs/')) return send({ id: state.consoleJobID, state: 'finished', output: 'rows: 2', next_offset: 7, output_size: 7, exit_code: 0, duration_seconds: .2 });
    if (path === 'control/servers/local/console') return method === 'GET' ? send({ two_factor_enabled: true, unlocked: state.consoleGranted, max_runtime_seconds: 600, max_output_bytes: 8388608 }) : send({ output: 'rows: 2', exit_code: 0, duration_seconds: .2 });
    if (path === 'control/servers/local/containers') return send({ checked_at: now, containers: [{ id: 'container-one', name: 'lidza-portal-' + 'a'.repeat(48), app_id: 'portal', role: 'application', state: 'running', health: 'healthy', cpu: '12.5%', memory: '80 MiB / 1 GiB', memory_percent: 8, restartable: true }] });
    if (path === 'control/servers/local/upgrade') return send({ current: 'v0.2.83', latest: 'v0.2.84', state: 'idle', supported: true });
    if (path === 'control/servers/local/ssh-access') return send({ keys: [{ id: 'operator', name: 'Operator', fingerprint: 'SHA256:fixture-key' }] });
    if (path === 'control/apps/portal/logs') return send({ logs: 'Application started\nServing requests' });
    if (path === 'control/databases') return send({ databases: [{ app_id: 'portal', server_id: 'local', mode: 'local', ready: true, backup: { hours: 24, keep: 7, offsite: true }, backups: [{ id: 'backup-one', created: now, size: 4096, offsite: false, upload_error: 'Storage unavailable' }] }] });
    if (path === 'control/storage/backups') return send({ bucket: 'private-backups', prefix: 'lidza', checked: now, objects: [{ key: 'databases/portal/' + 'long-backup-name-'.repeat(12) + '.dump', prefix: 'lidza', size: 4096, modifiedAt: now }] });
    if (path === 'control/apps/portal/cache') return send({ mode: 'automatic', configured: true, ready: true });
    if (path === 'control/infrastructure') return send({ storage: { configured: true, endpoint: 'https://nyc3.digitaloceanspaces.com', bucket: 'private-backups', region: 'nyc3', prefix: 'lidza', access_key_stored: true, secret_key_stored: true }, mail: { configured: true, host: 'smtp.example.com', port: 587, security: 'starttls', from: 'ops@example.com', username_stored: true, password_stored: true } });
    if (path === 'control/application-defaults') return send({ values: { LOG_LEVEL: 'info' } });
    if (path === 'control/team') return send({ members: [{ id: 'owner', name: 'Operator', email: 'ops@example.com', role: 'admin', owner: true }] });
    if (path === 'control/audit') return send({ entries: [{ id: 'audit-one', action: 'server.console.unlock', actor: 'ops@example.com', created: now, target: 'local' }] });
    if (path === 'control/updates') return send({ control_version: 'v0.2.83', settings: { latest_version: 'v0.2.84', automatic: false }, servers: [{ server_id: 'local', name: 'Production', current: 'v0.2.83', state: 'idle' }] });
    if (path.match(/^control\/apps\/[^/]+\/(errors|security)$/)) return send({ status: 'ready', last_received: now, errors: [{ id: 'event-one', fingerprint: 'fixture-error', source: 'server', message: 'Fixture request rejected', method: 'GET', route: '/api/staging/.env', requestId: 'request-fixture', createdAt: now, container: 'lidza-portal', release: 'build-one', stack: 'Error: fixture diagnostic\n  at handler.go:42' }] });
    if (path === 'v1/auth/two-factor') return send({ enabled: false, backupCodesLeft: 8 });
    if (path === 'v1/auth/two-factor/setup') return send({ secret: 'FIXTUREONLY', otpauthUrl: 'otpauth://totp/Fixture?secret=FIXTUREONLY', qrCodeDataUrl: 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9WlJ9l8AAAAASUVORK5CYII=' });
    if (method !== 'GET') return send({ status: 'accepted', codes: ['fixture-recovery-code'] });
    state.unknown.push(path); return send({});
  });
  return state;
}
module.exports = { mockConsole, app };
