// Host management fixtures for one server's detail page (web ingress,
// operating system updates, resource tuning, firewall, retained resources).
// mockServerHost(page, 'local') answers the agent's host endpoints with
// populated views, so layout checks measure every server tab with content:
// the tuning view has a failed change, deferred caps, two Redis instances
// (one with applications, one without), PostgreSQL settings and ClickHouse.
const now = '2026-11-06T14:00:00Z';

const ingress = {
  mode: 'caddy', source: 'detected', caddy_admin: 'http://127.0.0.1:2019', caddy_server: 'srv0', admin_api: true, web_ports: [80, 443],
  detail: 'Caddy answers on its admin API and owns ports 80 and 443', checked: now, configured: false,
  error_pages: { dir: '/var/lib/lidza/error-pages', ready: true, caddy_user: 'caddy', managed: 6, unmanaged_hosts: 2, detail: 'two hostnames still serve error pages from an old directory', unmanaged: [{ server: 'srv0', root: '/srv/old-errors', exists: true, hosts: ['legacy.example.com', 'shop.example.com'] }] },
};

const updates = {
  supported: true, checked_at: now, pending: 7, security: 2, disruptive: 1,
  packages: [{ name: 'openssl', current: '3.0.13-0ubuntu3.4', candidate: '3.0.13-0ubuntu3.5', security: true }, { name: 'curl', current: '8.5.0-2ubuntu10.4', candidate: '8.5.0-2ubuntu10.5', security: false }],
  disruptive_packages: [{ name: 'postgresql-17', current: '17.5-1.pgdg24.04+1', candidate: '17.6-1.pgdg24.04+1', security: true, disruptive: true, effect: 'restarts PostgreSQL; every application loses its database connections for a few seconds' }],
  reboot_required: true, reboot_packages: ['linux-image-6.8.0-48-generic'], reboot_reason: 'A new kernel was installed',
  unattended_installed: true, unattended_enabled: true, unattended_last_run: now, managed_origins: ['Caddy (cloudsmith)', 'PostgreSQL (apt.postgresql.org)'],
  components: [
    { name: 'Docker', package: 'docker-ce', installed: '27.3.1', candidate: '27.3.1', upgrade_available: false, source: 'package', repository: 'download.docker.com', automatic: 'security' },
    { name: 'Caddy', package: 'caddy', installed: '2.8.4', candidate: '2.9.1', upgrade_available: true, source: 'package', repository: 'cloudsmith', automatic: 'all' },
    { name: 'PostgreSQL', package: 'postgresql-17', installed: '17.5', candidate: '17.6', upgrade_available: true, source: 'package', automatic: 'none' },
  ],
  docker: { installed: true, running: true, version: '27.3.1', live_restore: true, live_restore_configured: true, managed: true },
  policy: { auto_security: true, auto_reboot: true, reboot_window: { weekdays: [0, 6], hour: 3, duration_minutes: 120 }, manage_package_sources: true, disruptive_in_window: true },
  configured: true, next_reboot_window: '2026-11-08T03:00:00Z', next_disruptive_window: '2026-11-08T03:00:00Z', last_reboot: '2026-10-20T03:04:00Z',
  jobs: [
    { id: 'security-1', kind: 'security-updates', trigger: 'schedule', status: 'succeeded', started: now, summary: 'Installed 3 security updates.', output: 'Unpacking openssl (3.0.13-0ubuntu3.4)\nSetting up openssl' },
    { id: 'reboot-1', kind: 'reboot', trigger: 'window', status: 'failed', started: '2026-10-20T03:00:00Z', summary: 'Deferred: a deployment was running.' },
  ],
};

const tuning = {
  supported: true, host: { cpus: 8, memory_mib: 32768, disk_gib: 320 }, apps: 4, computed: now, overrides: { connection_caps: { shop: 60 } },
  postgres: { cluster: '17/main', port: 5432, app_pool: 197, headroom: 50, in_use: 108, conf_file: '/etc/postgresql/17/main/conf.d/90-lidza-tuning.conf', note: 'Two settings were set with ALTER SYSTEM; the tuned values replace them.', settings: [
    { name: 'shared_buffers', current: '128MB', computed: '3610MB', target: '3610MB', restart: true, pending: true, formula: 'postgres budget / 4' },
    { name: 'effective_cache_size', current: '4GB', computed: '10830MB', target: '10830MB', formula: 'postgres budget × 3 / 4' },
    { name: 'work_mem', current: '4MB', computed: '17MB', target: '17MB', formula: '(budget − shared_buffers) / (max_connections × 3)' },
    { name: 'max_connections', current: '100', computed: '247', target: '247', restart: true, pending: true, formula: 'application pools + headroom' },
  ] },
  caps: [
    { app_id: 'shop', role: 'app_shop', current: -1, computed: 42, override: 60, target: 60, in_use: 12, peak: 30, pool: 50, pool_source: 'DB_POOL 10 × WEB_CONCURRENCY 5' },
    { app_id: 'portal', role: 'app_portal', current: 20, computed: 12, target: 20, in_use: 18, peak: 19, pool: 10, pool_source: 'RAILS_MAX_THREADS', deferred: true, warning: 'Lowering waits until fewer sessions are open.' },
    { app_id: 'legacy', role: 'deploy', current: -1, computed: -1, target: -1, in_use: 40, peak: 45, superuser: true, warning: 'This role is a superuser: PostgreSQL ignores connection limits for it.' },
  ],
  redis: [
    { id: 'shared', kind: 'shared', used_bytes: 1100 * 1048576, max_bytes: 0, policy: 'noeviction', target_mib: 1375, target_policy: 'noeviction', alert: false, note: 'Caches share this instance with queues, so nothing is evicted; move caches to their own Redis.',
      apps: [{ app_id: 'blog', dbs: [3], use: 'cache', use_source: 'probe', can_move: true }, { app_id: 'jobs', dbs: [4], use: 'queue', use_source: 'probe', can_move: false, move_blocked: 'database 4 is also used by api' }] },
    { id: 'lidza-redis-portal', kind: 'own', used_bytes: 20 * 1048576, max_bytes: 256 * 1048576, policy: 'allkeys-lru', target_mib: 256, target_policy: 'allkeys-lru', apps: [] },
  ],
  clickhouse: [{ id: 'host', kind: 'native', users: 2, current_ratio: 0.25, target_ratio: 0.2, server_mib: 6553, query_memory_mib: 3276 }],
  containers: [{ app_id: 'api', kind: 'postgresql', container: 'lidza-api-db', note: 'Applies at the next container restart.', settings: [{ name: 'shared_buffers', current: '128MB', computed: '512MB', target: '512MB', restart: true }, { name: 'work_mem', current: '4MB', computed: '8MB', target: '8MB' }] }],
  changes: [],
  pending_restart: ['shared_buffers', 'max_connections'], restart_note: 'wait for the maintenance window', next_window: '2026-11-08T03:00:00Z',
  warnings: ['The application pool of shop (50) exceeds its cap (42); the override raises the cap to 60.'],
  migrations: [{ app_id: 'blog', dbs: [3], use: 'cache', status: 'failed', started: '2026-11-05T10:00:00Z', error: 'The application did not become healthy on the new instance; the old setting was restored.' }],
  jobs: [
    { id: 'resource-tuning-boot-2', kind: 'resource-tuning', trigger: 'boot', status: 'failed', started: now, summary: 'Applied 3 tuning changes; 1 failed.', output: [
      'postgresql 17/main: wrote /etc/postgresql/17/main/conf.d/90-lidza-tuning.conf and reloaded the configuration',
      'postgresql 17/main work_mem: 4MB → 17MB (reload)',
      'role app_shop CONNECTION LIMIT: unlimited → 60 (live)',
      'role app_portal CONNECTION LIMIT: 20 → 12 (deferred): waits until fewer sessions are open',
      'container lidza-api-db shared_buffers: 128MB → 512MB (next-restart)',
      'FAILED Redis shared: CONFIG SET maxmemory: connection refused',
    ].join('\n') },
    { id: 'resource-tuning-hourly-1', kind: 'resource-tuning', trigger: 'hourly', status: 'succeeded', started: '2026-11-06T13:00:00Z', summary: 'Nothing to change.' },
  ],
};

const firewall = {
  supported: true, backend: 'ufw', installed: true, active: true, default_incoming: 'deny', last_applied: now,
  model: { enabled: true, default_inbound: 'deny', ssh_from: ['203.0.113.10'], rules: [{ port: '5432', proto: 'tcp', from: ['10.0.0.0/8'], comment: 'Replica access', source: 'user' }] },
  rules: [{ port: '22', proto: 'tcp', from: ['203.0.113.10'], comment: 'SSH', source: 'system' }, { port: '80,443', proto: 'tcp', comment: 'Web', source: 'system' }, { port: '25', proto: 'tcp', comment: 'Mail', source: 'app:portal', enforced: 'docker' }],
  kept: ['ALLOW 9100/tcp from 10.0.0.5'], notes: ['Docker-published ports are filtered in the DOCKER-USER chain.'],
};

const retained = [{ id: 'lidza-db-legacy', app_id: 'legacy', server_id: 'local', server_name: 'Production', kind: 'database', created: '2026-10-01T10:00:00Z', size_bytes: 512 * 1048576, reason: 'removed', databases: [{ id: 'legacy', engine: 'postgresql', volume: 'lidza-db-legacy', size_bytes: 512 * 1048576, last_local_backup: { id: 'b1', created: '2026-09-30T02:00:00Z', size: 64 * 1048576 } }] }];

async function mockServerHost(page, serverID = 'local') {
  const base = `**/api/control/servers/${serverID}`;
  const json = value => route => route.request().method() === 'GET' ? route.fulfill({ json: value }) : route.fallback();
  await page.route(`${base}/host/ingress*`, json(ingress));
  await page.route(`${base}/host/updates*`, json(updates));
  await page.route(`${base}/host/resources*`, json(tuning));
  await page.route(`${base}/host/firewall`, json(firewall));
  await page.route(`${base}/retained-resources`, json(retained));
}

module.exports = { mockServerHost, tuning, updates, ingress, firewall, retained };
