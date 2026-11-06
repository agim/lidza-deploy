import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { MigrationPlan, type MoveJob } from './MigrationPlan';
import type { OperationProgress } from './LiveProgress';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const servers = [{ id: 'alpha', name: 'Alpha' }, { id: 'beta', name: 'Beta' }];
const plan = {
  app_id: 'shop', source_server: 'alpha', target_server: 'beta', source_runtime: 'container', runtime: 'rails', target_runtime: 'native', runtime_choices: ['native', 'container'],
  detection: { mode: 'native', runtime: 'rails' }, repository: 'acme/shop', branch: 'main', commit: '0123456789abcdef0123456789abcdef01234567',
  hostnames: ['shop.example.com'], environment_keys: ['DATABASE_URL', 'RAILS_MASTER_KEY'], copied_keys: ['RAILS_MASTER_KEY'], destination_keys: ['DATABASE_URL'],
  databases: [{ id: 'shop', engine: 'postgresql', mode: 'local', tables: 12, size_bytes: 4 * 2 ** 20, action: 'copy' }], redis: true, volumes: ['persistent storage volume shop-files'],
  addresses: ['203.0.113.9'], warnings: ['Files are not moved automatically: persistent storage volume shop-files. Copy them to the destination before finishing.'], notes: [], blockers: [],
};
const steps = (statuses: Record<string, string>) => ['plan', 'prepare', 'environment', 'data', 'deploy', 'cutover', 'final_sync', 'finish'].map(key => ({ key, title: key, status: statuses[key] || 'pending', log: key === 'data' && statuses[key] === 'done' ? [{ time: '2026-11-05T10:00:00Z', message: 'Restored shop into shop-db0001 on beta; the SHA-256 matched on both ends.' }] : [] }));
const job = (state: string, extra: Partial<MoveJob> = {}): MoveJob => ({
  id: 'm1', app_id: 'shop', source_server: 'alpha', target_server: 'beta', target_runtime: 'native', runtime: 'rails', state, hostnames: ['shop.example.com'], addresses: ['203.0.113.9', '2001:db8::9'],
  dns: [], tls_ready: false, records: [{ hostname: 'shop.example.com', type: 'A', value: '203.0.113.9' }, { hostname: 'shop.example.com', type: 'AAAA', value: '2001:db8::9' }],
  actions: ['abort'], switched: false, steps: steps({ plan: 'done' }), ...extra,
});

function respond(status: number, body: unknown) {
  return Promise.resolve({ ok: status < 300, status, json: async () => body });
}

it('reviews, starts and finishes a move with manual DNS', async () => {
  let current = job('running');
  const requests: Array<{ url: string; method: string; body?: unknown }> = [];
  const fetch = vi.fn((url: string, init: RequestInit = {}) => {
    const method = init.method || 'GET';
    requests.push({ url, method, body: init.body ? JSON.parse(String(init.body)) : undefined });
    if (url === '/api/control/apps/shop/move' && method === 'GET') return respond(404, { error: 'no move recorded for this application' });
    if (url.startsWith('/api/control/apps/shop/move-plan?')) return respond(200, plan);
    if (url === '/api/control/apps/shop/move' && method === 'POST') return respond(202, current);
    if (url === '/api/control/moves/m1' && method === 'GET') return respond(200, current);
    if (url === '/api/control/moves/m1/recheck-dns') {
      current = job('ready', { actions: ['recheck-dns', 'finish', 'final-sync', 'abort'], tls_ready: true, tls_detail: 'HTTPS works on the destination for every hostname.', dns: [{ hostname: 'shop.example.com', addresses: ['203.0.113.9'], ready: true }], steps: steps({ plan: 'done', prepare: 'done', environment: 'done', data: 'done', deploy: 'done', cutover: 'done', final_sync: 'skipped' }) });
      return respond(202, current);
    }
    if (url === '/api/control/moves/m1/finish') {
      current = job('finished', { actions: [], switched: true, steps: steps({ plan: 'done', prepare: 'done', environment: 'done', data: 'done', deploy: 'done', cutover: 'done', final_sync: 'skipped', finish: 'done' }) });
      return respond(202, job('finishing', { actions: ['continue'], switched: true }));
    }
    return respond(500, { error: `unexpected ${method} ${url}` });
  });
  vi.stubGlobal('fetch', fetch);
  const changed = vi.fn();
  const { container } = render(<MigrationPlan appID="shop" serverID="alpha" servers={servers} onChanged={changed} pollMs={15} />);
  const details = container.querySelector('details')!;
  details.open = true;
  fireEvent(details, new Event('toggle'));

  // Review: the current server is not a destination.
  const destination = await screen.findByRole('combobox', { name: 'Destination server' });
  expect([...destination.querySelectorAll('option')].map(option => option.textContent)).toEqual(['Beta']);
  fireEvent.click(screen.getByRole('button', { name: 'Review move' }));
  await screen.findByText(/RAILS_MASTER_KEY/);
  expect(screen.getByText(/repository detected as native rails/)).toBeInTheDocument();
  expect(screen.getByText(/12 tables/)).toBeInTheDocument();
  expect(requests.find(r => r.url.includes('move-plan'))?.url).toContain('target_server=beta');
  const start = screen.getByRole('button', { name: 'Start move' });
  expect(start).toBeDisabled();
  fireEvent.click(screen.getByRole('checkbox', { name: /I have read these warnings/ }));
  expect(start).not.toBeDisabled();
  fireEvent.click(start);
  await screen.findByText('Moving to Beta');
  expect(requests.find(r => r.method === 'POST' && r.url === '/api/control/apps/shop/move')?.body).toEqual({ target_server: 'beta', acknowledge_warnings: true, max_attempts: 6 });

  // The job is polled; the cutover shows the exact records to set.
  current = job('cutover', { actions: ['recheck-dns', 'finish', 'final-sync', 'abort'], dns: [{ hostname: 'shop.example.com', addresses: ['198.51.100.7'], ready: false }], steps: steps({ plan: 'done', prepare: 'done', environment: 'done', data: 'done', deploy: 'done', cutover: 'waiting', final_sync: 'skipped' }) });
  await screen.findByRole('region', { name: 'DNS records' });
  expect(screen.getByRole('button', { name: 'Copy A value for shop.example.com' })).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Copy AAAA value for shop.example.com' })).toBeInTheDocument();
  expect(screen.getByText(/resolves to 198.51.100.7/)).toBeInTheDocument();
  expect(screen.getByText('Log (1)')).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Finish move' })).toBeDisabled();

  fireEvent.click(screen.getByRole('button', { name: "I've switched DNS — check again" }));
  await screen.findByText('points to the destination');
  const finish = screen.getByRole('button', { name: 'Finish move' });
  await waitFor(() => expect(finish).not.toBeDisabled());
  fireEvent.click(finish);
  await screen.findByText(/now runs on Beta/);
  await waitFor(() => expect(changed).toHaveBeenCalled());
  expect(requests.filter(r => r.method === 'POST').map(r => r.url)).toEqual(['/api/control/apps/shop/move', '/api/control/moves/m1/recheck-dns', '/api/control/moves/m1/finish']);
  // The console's CSP forbids inline styles.
  expect(container.querySelectorAll('[style]')).toHaveLength(0);
});

it('shows blockers and keeps the move from starting', async () => {
  const fetch = vi.fn((url: string, init: RequestInit = {}) => {
    if (url === '/api/control/apps/shop/move' && !init.method) return respond(404, { error: 'none' });
    if (url.startsWith('/api/control/apps/shop/move-plan?')) return respond(200, { ...plan, warnings: [], blockers: ['The destination already has an application with this id.'] });
    return respond(500, { error: 'unexpected' });
  });
  vi.stubGlobal('fetch', fetch);
  const { container } = render(<MigrationPlan appID="shop" serverID="alpha" servers={servers} />);
  const details = container.querySelector('details')!;
  details.open = true;
  fireEvent(details, new Event('toggle'));
  fireEvent.click(await screen.findByRole('button', { name: 'Review move' }));
  expect(await screen.findByRole('alert')).toHaveTextContent('already has an application with this id');
  expect(screen.getByRole('button', { name: 'Start move' })).toBeDisabled();
  expect(fetch.mock.calls.some(([, init]) => (init as RequestInit | undefined)?.method === 'POST')).toBe(false);
});

it('shows the build memory estimate and a memory blocker', async () => {
  const memory = { estimate: { mib: 2900, basis: ['peak of earlier builds on Alpha: 2.8 GiB'], learned: true }, total_mib: 2048, available_mib: 1500, reserve_mib: 512, level: 'blocker',
    message: 'Building needs about 2.8 GiB of memory, but Beta has 2.0 GiB in total and keeps 512 MiB free for running applications. Choose a server with more memory.' };
  vi.stubGlobal('fetch', vi.fn((url: string, init: RequestInit = {}) => {
    if (url === '/api/control/apps/shop/move' && !init.method) return respond(404, { error: 'none' });
    if (url.startsWith('/api/control/apps/shop/move-plan?')) return respond(200, { ...plan, warnings: [], build_memory: memory, blockers: [memory.message] });
    return respond(500, { error: 'unexpected' });
  }));
  const { container } = render(<MigrationPlan appID="shop" serverID="alpha" servers={servers} />);
  const details = container.querySelector('details')!;
  details.open = true;
  fireEvent(details, new Event('toggle'));
  fireEvent.click(await screen.findByRole('button', { name: 'Review move' }));
  expect(await screen.findByText('about 2.8 GiB (peak of earlier builds on Alpha: 2.8 GiB) · server 2.0 GiB RAM, 1.5 GiB free')).toHaveClass('move-build-memory-blocker');
  expect(screen.getByRole('alert')).toHaveTextContent('Choose a server with more memory');
  expect(screen.getByRole('button', { name: 'Start move' })).toBeDisabled();
});

it('resumes a move in progress and aborts it after confirmation', async () => {
  let current = job('failed', { error: 'copying shop failed: the dump does not match its recorded SHA-256; nothing was restored', actions: ['continue', 'abort'], steps: steps({ plan: 'done', prepare: 'done', environment: 'done', data: 'failed' }) });
  const fetch = vi.fn((url: string, init: RequestInit = {}) => {
    if (url === '/api/control/apps/shop/move') return respond(200, current);
    if (url === '/api/control/moves/m1/abort') { current = job('aborted', { actions: [] }); return respond(202, current); }
    if (url === '/api/control/moves/m1') return respond(200, current);
    return respond(500, { error: `unexpected ${init.method} ${url}` });
  });
  vi.stubGlobal('fetch', fetch);
  const { container } = render(<MigrationPlan appID="shop" serverID="alpha" servers={servers} pollMs={15} />);
  const details = container.querySelector('details')!;
  details.open = true;
  fireEvent(details, new Event('toggle'));
  expect(await screen.findByText(/does not match its recorded SHA-256/)).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Abort move' }));
  const dialog = await screen.findByRole('alertdialog');
  fireEvent.click(within(dialog).getByRole('button', { name: 'Abort move' }));
  await screen.findByText(/keeps running on Alpha/);
});

const open = (container: HTMLElement) => {
  const details = container.querySelector('details')!;
  details.open = true;
  fireEvent(details, new Event('toggle'));
};

it('lists file sets with size and transfer method, and warns not to change DNS early', async () => {
  const withFiles = {
    ...plan, warnings: [], volumes: [],
    files: [
      { id: 'storage', path: 'storage', source: 'volume lidza-storage-shop (storage)', files: 1240, bytes: 3 * 2 ** 30, action: 'copy', method: 's3' },
      { id: 'public-uploads', path: 'public/uploads', source: 'volume lidza-storage-shop (public-uploads)', files: 12, bytes: 5 * 2 ** 20, action: 'copy', method: 'direct' },
      { id: 'public-system', path: 'public/system', source: 'volume lidza-storage-shop (public-system)', files: 0, bytes: 0, action: 'skip_empty' },
    ],
  };
  vi.stubGlobal('fetch', vi.fn((url: string, init: RequestInit = {}) => {
    if (url === '/api/control/apps/shop/move' && !init.method) return respond(404, { error: 'none' });
    if (url.startsWith('/api/control/apps/shop/move-plan?')) return respond(200, withFiles);
    return respond(500, { error: 'unexpected' });
  }));
  const { container } = render(<MigrationPlan appID="shop" serverID="alpha" servers={servers} />);
  open(container);
  fireEvent.click(await screen.findByRole('button', { name: 'Review move' }));
  const files = await screen.findByText('Files');
  const list = files.nextElementSibling as HTMLElement;
  expect(within(list).getAllByRole('listitem').map(item => item.textContent)).toEqual([
    'storage volume lidza-storage-shop (storage) (1,240 files, 3.0 GiB) · copied through workspace storage',
    'public/uploads volume lidza-storage-shop (public-uploads) (12 files, 5.0 MiB) · copied directly through the control panel',
    'public/system volume lidza-storage-shop (public-system) (0 files, 0.0 KiB) · skipped: no files',
  ]);
  expect(screen.getByRole('note')).toHaveTextContent("Don't change DNS until the move reaches Switch DNS.");
  // No warning to acknowledge: the files are moved.
  expect(screen.queryByRole('checkbox', { name: /I have read these warnings/ })).toBeNull();
  expect(screen.getByRole('button', { name: 'Start move' })).not.toBeDisabled();
});

it('notes skipped runtime sockets on a file set without blocking the move', async () => {
  const withQueues = {
    ...plan, warnings: [], volumes: [],
    files: [
      { id: 'stack-postfix-queue', path: 'stack/postfix-queue', source: 'stack volume lidza-stack-shop-postfix-queue', files: 48, bytes: 2 * 2 ** 20, action: 'copy', method: 's3', skipped: 21 },
      { id: 'stack-rspamd-state', path: 'stack/rspamd-state', source: 'stack volume lidza-stack-shop-rspamd-state', files: 3, bytes: 2 ** 20, action: 'copy', method: 's3', skipped: 1 },
    ],
    notes: ['Sockets and pipes in the file sets are runtime state that the services recreate when they start on the destination; they are not copied.'],
  };
  vi.stubGlobal('fetch', vi.fn((url: string, init: RequestInit = {}) => {
    if (url === '/api/control/apps/shop/move' && !init.method) return respond(404, { error: 'none' });
    if (url.startsWith('/api/control/apps/shop/move-plan?')) return respond(200, withQueues);
    return respond(500, { error: 'unexpected' });
  }));
  const { container } = render(<MigrationPlan appID="shop" serverID="alpha" servers={servers} />);
  open(container);
  fireEvent.click(await screen.findByRole('button', { name: 'Review move' }));
  const list = (await screen.findByText('Files')).nextElementSibling as HTMLElement;
  expect(within(list).getAllByRole('listitem').map(item => item.textContent)).toEqual([
    'stack/postfix-queue stack volume lidza-stack-shop-postfix-queue (48 files, 2.0 MiB) · copied through workspace storage · 21 runtime sockets skipped',
    'stack/rspamd-state stack volume lidza-stack-shop-rspamd-state (3 files, 1.0 MiB) · copied through workspace storage · 1 runtime socket skipped',
  ]);
  expect(screen.getByText(/runtime state that the services recreate/)).toBeInTheDocument();
  // Runtime files are not blockers: nothing to resolve, nothing to acknowledge.
  expect(screen.queryByRole('alert')).toBeNull();
  expect(screen.queryByRole('checkbox', { name: /I have read these warnings/ })).toBeNull();
  expect(screen.getByRole('button', { name: 'Start move' })).not.toBeDisabled();
});

it('shows the file copy step and warns when DNS reaches the destination before the deploy', async () => {
  const filesSteps = (statuses: Record<string, string>) => ['plan', 'prepare', 'environment', 'data', 'files', 'deploy', 'cutover', 'final_sync', 'finish'].map(key => ({
    key, title: key === 'files' ? 'Copy files' : key, status: statuses[key] || 'pending',
    log: key === 'files' && statuses[key] === 'done' ? [{ time: '2026-11-05T10:00:00Z', message: 'Restored volume lidza-storage-shop (2 files, 1.0 KiB) into storage on beta directly; the SHA-256 matched on both ends.' }] : [],
  }));
  let current = job('running', {
    steps: filesSteps({ plan: 'done', prepare: 'done', environment: 'done', data: 'done', files: 'done', deploy: 'running' }),
    dns: [{ hostname: 'shop.example.com', addresses: ['203.0.113.9'], ready: true }], dns_ahead: true,
  });
  vi.stubGlobal('fetch', vi.fn((url: string) => {
    if (url === '/api/control/apps/shop/move' || url === '/api/control/moves/m1') return respond(200, current);
    return respond(500, { error: 'unexpected' });
  }));
  const { container } = render(<MigrationPlan appID="shop" serverID="alpha" servers={servers} pollMs={15} />);
  open(container);
  const step = await screen.findByText('Copy files');
  expect(step.closest('li')).toHaveClass('move-step-done');
  const order = [...container.querySelectorAll('.move-step-heading > span:last-child')].map(node => node.textContent);
  expect(order.indexOf('Copy files')).toBe(order.indexOf('data') + 1);
  expect(order.indexOf('deploy')).toBe(order.indexOf('Copy files') + 1);
  expect(screen.getByRole('alert')).toHaveTextContent('DNS already points to Beta, but the application is not deployed there yet.');
  expect(screen.getByRole('alert')).toHaveTextContent('The site is down for visitors who reach Beta until the deploy finishes.');

  // Once deployed, the destination serves the application: no warning.
  current = job('cutover', {
    actions: ['recheck-dns', 'finish', 'final-sync', 'abort'], dns_ahead: true,
    steps: filesSteps({ plan: 'done', prepare: 'done', environment: 'done', data: 'done', files: 'done', deploy: 'done', cutover: 'waiting' }),
    dns: [{ hostname: 'shop.example.com', addresses: ['203.0.113.9'], ready: true }],
  });
  await screen.findByRole('region', { name: 'DNS records' });
  expect(screen.queryByText(/not deployed there yet/)).toBeNull();
});

it('reminds to keep DNS unchanged while the move prepares the destination', async () => {
  vi.stubGlobal('fetch', vi.fn(() => respond(200, job('running', { steps: steps({ plan: 'done', prepare: 'running' }), dns: [{ hostname: 'shop.example.com', addresses: ['198.51.100.7'], ready: false }] }))));
  const { container } = render(<MigrationPlan appID="shop" serverID="alpha" servers={servers} pollMs={1000} />);
  open(container);
  expect(await screen.findByText('Keep DNS unchanged until the move reaches Switch DNS.')).toBeInTheDocument();
  expect(screen.queryByRole('alert')).toBeNull();
});

it('shows live progress on the running step while data streams', async () => {
  const now = Date.now();
  const iso = (offset: number) => new Date(now + offset).toISOString();
  const running = (progress: OperationProgress) => job('running', { steps: steps({ plan: 'done', prepare: 'done', environment: 'done' }).map(step => step.key === 'data' ? { ...step, status: 'running', started: iso(-125_000), progress, log: [{ time: iso(-120_000), message: 'Backup b1 is ready (3.1 GiB); streaming it to beta.' }, { time: iso(-2000), message: 'Still streaming the dump of shop to beta: 1.2 GiB of 3.1 GiB (1m58s elapsed).', heartbeat: true }] } : step) });
  let current = running({ phase: 'dumping shop on alpha', done_bytes: 1.2 * 2 ** 30, started_at: iso(-60_000), updated_at: iso(-1000) });
  vi.stubGlobal('fetch', vi.fn((url: string) => respond(200, url === '/api/control/apps/shop/move' || url === '/api/control/moves/m1' ? current : {})));
  const { container } = render(<MigrationPlan appID="shop" serverID="alpha" servers={servers} pollMs={15} />);
  open(container);
  const step = (await screen.findByText('Dumping shop on alpha')).closest('li')!;
  expect(within(step).getByRole('progressbar')).not.toHaveAttribute('aria-valuenow');
  expect(within(step).getByText(/^1\.2 GiB written/)).toBeInTheDocument();
  expect(within(step).getByText(/^2m 0\ds$/)).toBeInTheDocument();
  // The source finished the dump; the move streams it with a known size.
  current = running({ phase: 'streaming the dump of shop to beta', done_bytes: 1.55 * 2 ** 30, total_bytes: 3.1 * 2 ** 30, started_at: iso(-30_000), updated_at: iso(0) });
  const bar = await within(step).findByRole('progressbar', { name: 'Streaming the dump of shop to beta' });
  expect(bar).toHaveAttribute('aria-valuenow', '50');
  expect(within(step).getByText(/^1\.6 GiB of 3\.1 GiB/)).toBeInTheDocument();
  // Fresh bytes are activity, although the step logged only a heartbeat for two minutes.
  expect(within(step).queryByText(/Still working/, { selector: '.live-progress-note' })).toBeNull();
  // Other steps show no live progress.
  expect(container.querySelectorAll('.live-progress')).toHaveLength(1);
});

it('shows estimates and lets the operator leave behind tables the backups skip', async () => {
  const requests: Array<{ url: string; method: string; body?: unknown }> = [];
  const reviewed = { ...plan, warnings: [], max_attempts: 6,
    databases: [{ ...plan.databases[0], excluded_tables: [{ name: 'public.events', rows: 39_900_000, bytes: 27 * 2 ** 30, choice: 'copy' }, { name: 'public.audit_log', rows: 686_000, bytes: 300 * 2 ** 20, choice: 'copy' }, { name: 'public.sessions', rows: -1, bytes: 64 * 2 ** 20, choice: 'copy' }] }],
    estimate: { basis: 'disk probe', bulk_seconds: 4000, final_seconds: 4100, notes: ['Estimates: the final sync copies every database and file set again.'],
      engines: [{ kind: 'postgresql', object: 'shop', label: 'PostgreSQL shop', bytes: 28 * 2 ** 30, objects: 12, object_label: 'tables', measured: true, bulk_seconds: 3700, final_seconds: 3700, basis: 'disk probe' }] } };
  vi.stubGlobal('fetch', vi.fn((url: string, init: RequestInit = {}) => {
    const method = init.method || 'GET';
    requests.push({ url, method, body: init.body ? JSON.parse(String(init.body)) : undefined });
    if (url === '/api/control/apps/shop/move' && method === 'GET') return respond(204, {});
    if (url.startsWith('/api/control/apps/shop/move-plan?')) return respond(200, reviewed);
    if (url === '/api/control/apps/shop/move' && method === 'POST') return respond(202, job('running'));
    return respond(200, job('running'));
  }));
  const { container } = render(<MigrationPlan appID="shop" serverID="alpha" servers={servers} pollMs={1000} />);
  open(container);
  fireEvent.click(await screen.findByRole('button', { name: 'Review move' }));
  const table = await screen.findByRole('table');
  expect(table).toHaveTextContent('PostgreSQL shop');
  expect(table).toHaveTextContent('28.0 GiB, 12 tables');
  expect(table).toHaveTextContent('≈ 1h 2m');
  expect(screen.getByText(/Estimate based on disk probe/)).toBeInTheDocument();
  // The choice is offered before the move starts, copying by default.
  const skipped = screen.getByRole('region', { name: 'Tables the backups skip' });
  expect(skipped).toHaveTextContent('Choose before starting');
  expect(within(skipped).getByText(/about 39.9M rows, 27.0 GiB · data copied/)).toBeInTheDocument();
  // An unknown row estimate shows the size only.
  expect(within(skipped).getByText('64.0 MiB · data copied')).toBeInTheDocument();
  expect(skipped).not.toHaveTextContent('rows unknown');
  const audit = within(skipped).getByRole('group', { name: 'Data of public.audit_log' });
  expect(within(audit).getByRole('button', { name: 'Copy data' })).toHaveAttribute('aria-pressed', 'true');
  fireEvent.click(within(audit).getByRole('button', { name: 'Leave behind' }));
  expect(within(audit).getByRole('button', { name: 'Leave behind' })).toHaveAttribute('aria-pressed', 'true');
  expect(within(skipped).getByText(/about 686k rows, 300.0 MiB · left behind \(schema only\)/)).toBeInTheDocument();
  fireEvent.change(screen.getByRole('combobox', { name: 'Automatic attempts per step' }), { target: { value: '3' } });
  fireEvent.click(screen.getByRole('button', { name: 'Start move' }));
  await waitFor(() => expect(requests.find(r => r.method === 'POST')?.body).toEqual({ target_server: 'beta', acknowledge_warnings: false, max_attempts: 3, excluded_tables: { shop: { 'public.events': 'copy', 'public.audit_log': 'leave', 'public.sessions': 'copy' } } }));
  expect(container.querySelectorAll('[style]')).toHaveLength(0);
});

it('shows the choices a move started with, without asking again', async () => {
  const current = job('running', { locked: false, phase: 'live', actions: ['abort', 'excluded-tables'],
    databases: [{ source_id: 'shop', copyable: true, excluded_tables: [{ name: 'public.events', rows: 52_000, bytes: 80 * 2 ** 20, choice: 'copy', copied_rows: 52_113 }, { name: 'public.audit_log', rows: -1, bytes: 2 ** 20, choice: 'leave' }] }],
    steps: steps({ plan: 'done', prepare: 'done', environment: 'done', data: 'running' }) });
  vi.stubGlobal('fetch', vi.fn(() => respond(200, current)));
  const { container } = render(<MigrationPlan appID="shop" serverID="alpha" servers={servers} pollMs={1000} />);
  open(container);
  const region = await screen.findByRole('region', { name: 'Tables the backups leave out' });
  expect(region).toHaveTextContent('public.events about 52k rows, 80.0 MiB · data copied · 52,113 rows copied');
  expect(region).toHaveTextContent('public.audit_log 1.0 MiB · left behind (schema only)');
  expect(within(region).queryByRole('button')).toBeNull();
  expect(region).not.toHaveTextContent('Choose for each table');
});

it('asks to confirm a final sync expected to lock the application for longer than 15 minutes', async () => {
  const posted: Array<{ url: string; body?: unknown }> = [];
  const current = job('cutover', { locked: false, phase: 'live', actions: ['recheck-dns', 'finish', 'final-sync', 'abort'], lock_estimate: { seconds: 2400, basis: "this move's bulk copy", confirm: true }, destination_stale: true,
    steps: steps({ plan: 'done', prepare: 'done', environment: 'done', data: 'done', deploy: 'done', cutover: 'waiting', final_sync: 'skipped' }) });
  vi.stubGlobal('fetch', vi.fn((url: string, init: RequestInit = {}) => {
    if (init.method === 'POST') { posted.push({ url, body: init.body ? JSON.parse(String(init.body)) : undefined }); return respond(202, { ...current, state: 'syncing', locked: true, phase: 'locked', actions: ['continue'] }); }
    return respond(200, current);
  }));
  const { container } = render(<MigrationPlan appID="shop" serverID="alpha" servers={servers} pollMs={1000} />);
  open(container);
  expect(await screen.findByText('Preparing move (app stays live)')).toBeInTheDocument();
  expect(screen.getByText(/redeploys the source's current commit at the final sync/)).toBeInTheDocument();
  expect(screen.getByText(/Expected lock during the final sync/)).toHaveTextContent('about 40m');
  const sync = screen.getByRole('button', { name: 'Final sync' });
  expect(sync).toBeDisabled();
  fireEvent.click(screen.getByRole('checkbox', { name: /Lock the application for about 40m/ }));
  expect(sync).toBeEnabled();
  fireEvent.click(sync);
  const dialog = await screen.findByRole('alertdialog');
  expect(dialog).toHaveTextContent('Expected lock: about 40m');
  fireEvent.click(within(dialog).getByRole('button', { name: 'Run final sync' }));
  await waitFor(() => expect(posted).toEqual([{ url: '/api/control/moves/m1/final-sync', body: { confirm_lock: true } }]));
  expect(await screen.findByText('Final sync — app locked')).toBeInTheDocument();
});

it('shows a scheduled retry and records choices for tables without one', async () => {
  const posted: Array<{ url: string; body?: unknown }> = [];
  const next = new Date(Date.now() + 60_000).toISOString();
  const current = job('running', { locked: false, phase: 'live', actions: ['abort', 'retry-now', 'excluded-tables'],
    failure: { step: 'data', engine: 'PostgreSQL shop', server: 'beta', class: 'transient', code: 'agent_503', message: 'Copy data failed (PostgreSQL shop) on Beta: agent restarting', attempt: 2, max_attempts: 6, next_retry: next, time: new Date().toISOString() },
    databases: [{ source_id: 'shop', copyable: true, excluded_tables: [{ name: 'public.events', rows: -1, bytes: -1, choice: '' }] }],
    steps: steps({ plan: 'done', prepare: 'done', environment: 'done', data: 'running' }) });
  vi.stubGlobal('fetch', vi.fn((url: string, init: RequestInit = {}) => {
    if (init.method === 'POST') { posted.push({ url, body: init.body ? JSON.parse(String(init.body)) : undefined }); return respond(202, current); }
    return respond(200, current);
  }));
  const { container } = render(<MigrationPlan appID="shop" serverID="alpha" servers={servers} pollMs={1000} />);
  open(container);
  expect(await screen.findByText(/Transient \(agent_503\) · attempt 2\/6/)).toBeInTheDocument();
  expect(screen.getByText('Next attempt')).toBeInTheDocument();
  const region = screen.getByRole('region', { name: 'Tables the backups leave out' });
  expect(region).toHaveTextContent('Choose for each table');
  expect(region).toHaveTextContent('not measured');
  expect(region).not.toHaveTextContent('rows unknown');
  fireEvent.click(within(region).getByRole('button', { name: 'Leave behind' }));
  await waitFor(() => expect(posted).toHaveLength(1));
  await waitFor(() => expect(screen.getByRole('button', { name: 'Retry now' })).toBeEnabled());
  fireEvent.click(screen.getByRole('button', { name: 'Retry now' }));
  await waitFor(() => expect(posted.map(request => request.url)).toEqual(['/api/control/moves/m1/excluded-tables', '/api/control/moves/m1/retry-now']));
  expect(posted[0].body).toEqual({ database: 'shop', tables: { 'public.events': 'leave' } });
});

it('offers to remove the retained data that stopped Prepare, with confirmation and a final backup', async () => {
  const posted: Array<{ url: string; body?: unknown }> = [];
  let current = job('failed', { actions: ['continue', 'remove-retained', 'abort'], steps: steps({ plan: 'done', prepare: 'failed' }),
    failure: { step: 'prepare', server: 'beta', class: 'operator', code: 'retained_data', message: 'Beta keeps retained data of shop (r-old): an application with this id ran there before.', attempt: 1, max_attempts: 6, time: new Date().toISOString(),
      remediation: 'Remove the retained data of shop on Beta from this move, or remove it under the server\'s retained resources; the move then continues.',
      retained: { id: 'r-old', server: 'beta', app_id: 'shop', databases: 1, last_backup: '2026-11-05T10:00:00Z', final_backup_required: true, label: 'Remove retained data of shop on Beta (last backup 2026-11-05 10:00 UTC)' } } });
  vi.stubGlobal('fetch', vi.fn((url: string, init: RequestInit = {}) => {
    if (init.method === 'POST') {
      posted.push({ url, body: init.body ? JSON.parse(String(init.body)) : undefined });
      current = job('running', { actions: ['continue', 'abort'], steps: steps({ plan: 'done', prepare: 'running' }) });
      return respond(202, current);
    }
    return respond(200, current);
  }));
  const { container } = render(<MigrationPlan appID="shop" serverID="alpha" servers={servers} pollMs={1000} />);
  open(container);
  expect(await screen.findByText(/Needs your action \(retained_data\)/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Remove retained data of shop on Beta (last backup 2026-11-05 10:00 UTC)' }));
  const dialog = await screen.findByRole('alertdialog');
  expect(dialog).toHaveTextContent('Remove retained data of shop on Beta?');
  const backup = within(dialog).getByRole('checkbox');
  expect(backup).toBeChecked();
  expect(backup).toBeDisabled();
  const submit = within(dialog).getByRole('button', { name: 'Remove and continue' });
  expect(submit).toBeDisabled();
  fireEvent.change(within(dialog).getByLabelText(/to confirm/), { target: { value: 'r-old' } });
  expect(submit).toBeEnabled();
  fireEvent.click(submit);
  await waitFor(() => expect(posted).toEqual([{ url: '/api/control/moves/m1/remove-retained', body: { confirm: 'r-old', final_backup: true } }]));
  await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument());
  expect(screen.queryByRole('button', { name: /Remove retained data/ })).not.toBeInTheDocument();
});
