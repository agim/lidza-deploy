import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { MigrationPlan, type MoveJob } from './MigrationPlan';

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
  expect(requests.find(r => r.method === 'POST' && r.url === '/api/control/apps/shop/move')?.body).toEqual({ target_server: 'beta', acknowledge_warnings: true });

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
