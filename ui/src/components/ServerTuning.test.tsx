import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { ServerTuning } from './ServerTuning';
import type { ResourceTuningView } from '../lib/types';

const api = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), post: vi.fn(), patch: vi.fn() }));
vi.mock('../lib/api', () => api);
vi.mock('./ToastProvider', () => ({ useToast: () => () => {} }));

afterEach(() => { cleanup(); vi.clearAllMocks(); });

const view: ResourceTuningView = {
  supported: true, host: { cpus: 8, memory_mib: 32768, disk_gib: 320 }, apps: 20, computed: '2026-11-06T14:00:00Z', overrides: {},
  postgres: { cluster: '17/main', port: 5432, app_pool: 197, headroom: 50, in_use: 108, conf_file: '/etc/postgresql/17/main/conf.d/90-lidza-tuning.conf', settings: [
    { name: 'shared_buffers', current: '128MB', computed: '3610MB', target: '3610MB', restart: true, pending: true, formula: 'postgres budget/4' },
    { name: 'work_mem', current: '4MB', computed: '17MB', target: '17MB' },
  ] },
  caps: [
    { app_id: 'shop', role: 'app_shop', current: -1, computed: 42, target: 42, in_use: 12, peak: 30, pool: 50, pool_source: 'DB_POOL 10 × WEB_CONCURRENCY 5', warning: 'The application\'s configured pool (50) exceeds its cap (42); lower its pool or raise the cap.' },
    { app_id: 'legacy', role: 'deploy', current: -1, computed: -1, target: -1, in_use: 40, peak: 45, superuser: true, warning: 'This role is a superuser: PostgreSQL ignores connection limits for it.' },
  ],
  redis: [{ id: 'shared', kind: 'shared', used_bytes: 1100 * 1048576, max_bytes: 0, policy: 'noeviction', target_mib: 1375, target_policy: 'noeviction', alert: false, note: 'Caches share this instance with queues or persistent data, so nothing is evicted; move them to their own Redis.',
    apps: [{ app_id: 'blog', dbs: [3], use: 'cache', use_source: 'probe', can_move: true }, { app_id: 'jobs', dbs: [4], use: 'queue', use_source: 'probe', can_move: false, move_blocked: 'database 4 is also used by api' }] }],
  clickhouse: [{ id: 'host', kind: 'native', users: 2, current_ratio: 0.25, target_ratio: 0.2, server_mib: 6553, query_memory_mib: 3276 }],
  changes: [],
  pending_restart: ['shared_buffers'], restart_note: 'waits for the maintenance window starting 2026-11-08T03:00:00Z', next_window: '2026-11-08T03:00:00Z',
  jobs: [{ id: 'resource-tuning-boot-1', kind: 'resource-tuning', trigger: 'boot', status: 'succeeded', started: '2026-11-06T14:00:00Z', summary: 'Applied 19 tuning changes.', output: 'role app_shop CONNECTION LIMIT: unlimited → 42 (live)' }],
};

it('shows settings, caps, Redis and the pending restart with its window', async () => {
  api.get.mockResolvedValue(view);
  render(<ServerTuning serverID="edge" />);
  expect(await screen.findByText('PostgreSQL 17/main · port 5432')).toBeInTheDocument();
  expect(api.get).toHaveBeenCalledWith('control/servers/edge/host/resources');
  expect(screen.getByRole('status', { name: 'Pending restart' })).toHaveTextContent('shared_buffers');
  expect(screen.getByRole('status', { name: 'Pending restart' })).toHaveTextContent('maintenance window');
  expect(screen.getByText('3610MB', { selector: 'strong' })).toBeInTheDocument();
  expect(screen.getByText(/configured pool \(50\) exceeds its cap/)).toBeInTheDocument();
  expect(screen.getByText('not enforced')).toBeInTheDocument();
  expect(screen.getByText(/ignores connection limits/)).toBeInTheDocument();
  expect(screen.getByText('Cannot move: database 4 is also used by api')).toBeInTheDocument();
  expect(screen.getByText(/Memory ratio 0.250 → 0.200/)).toBeInTheDocument();
  expect(screen.getByText('Applied 19 tuning changes.')).toBeInTheDocument();
});

it('saves overrides and reviews the dry run before applying', async () => {
  api.get.mockResolvedValue(view);
  const reviewed = { ...view, overrides: { postgres: { shared_buffers: '4GB' }, connection_caps: { shop: 60 }, redis_use: { blog: 'persistent' } }, changes: [{ resource: 'postgresql 17/main', setting: 'shared_buffers', old: '128MB', new: '4GB', apply: 'restart' }, { resource: 'role app_shop', setting: 'CONNECTION LIMIT', old: 'unlimited', new: '60', apply: 'live' }] };
  api.put.mockResolvedValue(reviewed);
  api.post.mockResolvedValue(reviewed);
  render(<ServerTuning serverID="edge" />);
  fireEvent.change(await screen.findByLabelText('Override shared_buffers'), { target: { value: '4GB' } });
  fireEvent.change(screen.getByLabelText('Connection cap shop'), { target: { value: '60' } });
  fireEvent.change(screen.getByLabelText('Redis use blog'), { target: { value: 'persistent' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save overrides' }));
  await waitFor(() => expect(api.put).toHaveBeenCalledWith('control/servers/edge/host/resources/overrides', expect.objectContaining({ postgres: { shared_buffers: '4GB' }, connection_caps: { shop: 60 }, redis_use: { blog: 'persistent' } })));
  const changes = await screen.findByRole('list', { name: 'Pending tuning changes' });
  expect(within(changes).getByText('maintenance-window restart')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Apply now' }));
  fireEvent.click(await screen.findByRole('button', { name: 'Apply tuning' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('control/servers/edge/host/resources/apply', {}));
});

it('moves a shared-Redis application to its own instance after confirmation', async () => {
  api.get.mockResolvedValue(view);
  api.post.mockResolvedValue({ ...view, migrations: [{ app_id: 'blog', dbs: [3], use: 'cache', status: 'running', step: 'Provisioning', started: '2026-11-06T14:05:00Z' }] });
  render(<ServerTuning serverID="edge" />);
  fireEvent.click(await screen.findByRole('button', { name: 'Move to its own Redis' }));
  expect(await screen.findByText(/starts empty/)).toBeInTheDocument();
  fireEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Move to its own Redis' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('control/servers/edge/host/resources/redis/blog/migrate', { confirm: 'blog' }));
  expect(await screen.findByText(/Provisioning/)).toBeInTheDocument();
});
