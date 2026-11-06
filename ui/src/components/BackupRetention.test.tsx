import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ToastContext } from './toast-context';
import { BackupRetentionCard, DatabaseRetentionCard, type RetentionPlan } from './BackupRetention';

const api = vi.hoisted(() => ({ get: vi.fn(), patch: vi.fn(), post: vi.fn(), put: vi.fn() }));
vi.mock('../lib/api', () => api);

const GiB = 2 ** 30;
const settings = { daily: 7, weekly: 4, monthly: 0, orphan_grace_days: 30, automatic: true };
const plan: RetentionPlan = {
  fingerprint: 'reviewed-fingerprint', keep_objects: 3, keep_bytes: 3072, delete_objects: 3, delete_bytes: 3 * GiB,
  unavailable_servers: [],
  databases: [
    { database: 'portal', status: 'retained', policy: { daily: 7, weekly: 4, monthly: 0 }, keep_bytes: 2048, delete_bytes: 2 * GiB,
      keep: [{ key: 'databases/portal/predeployment.dump', size: 1024, created: '2026-11-01T00:00:00Z', reason: 'pre-deployment copy' }, { key: 'databases/portal/new.dump', size: 1024, created: '2026-11-05T00:00:00Z', reason: 'newest backup' }],
      delete: [{ key: 'databases/portal/old-1.dump', size: GiB, created: '2026-09-01T00:00:00Z', reason: 'outside retention' }, { key: 'databases/portal/old-2.dump', size: GiB, created: '2026-09-02T00:00:00Z', reason: 'outside retention' }] },
    { database: 'native-archive-legacy', status: 'orphaned', removed: '2026-09-01T00:00:00Z', delete_after: '2026-10-01T00:00:00Z', keep_bytes: 0, delete_bytes: GiB, keep: [],
      delete: [{ key: 'databases/native-archive-legacy/a.dump', size: GiB, created: '2026-08-01T00:00:00Z', reason: 'orphaned' }] },
    { database: 'busy', status: 'protected', note: 'a backup operation is running on one', keep_bytes: 1024, delete_bytes: 0, delete: [],
      keep: [{ key: 'databases/busy/a.dump', size: 1024, created: '2026-11-05T00:00:00Z', reason: 'a backup operation is running' }] },
  ],
};

let notify = vi.fn();
afterEach(cleanup);
beforeEach(() => {
  for (const fn of Object.values(api)) fn.mockReset();
  notify = vi.fn();
  api.get.mockImplementation(async (path: string) => path === 'control/storage/retention/plan' ? plan : { settings, reviewed: false, defaults: settings });
});

const renderWithToast = (node: React.ReactNode) => render(<ToastContext.Provider value={notify}>{node}</ToastContext.Provider>);

it('reviews the workspace cleanup plan and applies exactly the reviewed fingerprint', async () => {
  api.post.mockResolvedValue({ deleted: 3, deleted_bytes: 3 * GiB });
  renderWithToast(<BackupRetentionCard />);
  expect(await screen.findByText(/Retention has not been saved yet/)).toBeInTheDocument();
  expect(screen.getByLabelText(/Daily backups/)).toHaveValue(7);
  expect(screen.getByLabelText(/Weekly backups/)).toHaveValue(4);
  fireEvent.click(screen.getByRole('button', { name: /Review cleanup/ }));
  expect(await screen.findByText('native-archive-legacy')).toBeInTheDocument();
  expect(screen.getByText(/3 objects · 3.0 GiB reclaimed/)).toBeInTheDocument();
  expect(screen.getByText(/Orphaned \(removed/)).toBeInTheDocument();
  expect(screen.getByText(/2 · 2.0 GiB/)).toBeInTheDocument();
  expect(screen.getByText('databases/portal/old-1.dump')).toBeInTheDocument();
  expect(screen.queryByText('databases/portal/new.dump')).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: /Apply cleanup/ }));
  const dialog = await screen.findByRole('alertdialog');
  expect(within(dialog).getByText(/3 objects \(3.0 GiB\)/)).toBeInTheDocument();
  fireEvent.click(within(dialog).getByRole('button', { name: 'Apply cleanup' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('control/storage/retention/apply', { fingerprint: 'reviewed-fingerprint' }));
  await waitFor(() => expect(notify).toHaveBeenCalledWith('Deleted 3 objects (3.0 GiB).', false));
  // The plan is reviewed again after applying.
  await waitFor(() => expect(api.get.mock.calls.filter(([path]) => path === 'control/storage/retention/plan')).toHaveLength(2));
});

it('reports a refused apply when the plan changed since review', async () => {
  api.post.mockRejectedValue(new Error('the cleanup plan changed since it was reviewed; review it again'));
  renderWithToast(<BackupRetentionCard />);
  fireEvent.click(await screen.findByRole('button', { name: /Review cleanup/ }));
  fireEvent.click(await screen.findByRole('button', { name: /Apply cleanup/ }));
  fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Apply cleanup' }));
  await waitFor(() => expect(notify).toHaveBeenCalledWith('the cleanup plan changed since it was reviewed; review it again', true));
});

it('disables apply when nothing would be deleted and warns about unavailable servers', async () => {
  api.get.mockImplementation(async (path: string) => path === 'control/storage/retention/plan'
    ? { ...plan, delete_objects: 0, delete_bytes: 0, unavailable_servers: ['Edge'], databases: [plan.databases[2]] }
    : { settings, reviewed: true });
  renderWithToast(<BackupRetentionCard />);
  fireEvent.click(await screen.findByRole('button', { name: /Review cleanup/ }));
  expect(await screen.findByText(/Some servers did not answer: Edge/)).toBeInTheDocument();
  expect(screen.getByRole('button', { name: /Apply cleanup/ })).toBeDisabled();
  expect(screen.queryByText(/Retention has not been saved yet/)).not.toBeInTheDocument();
});

it('saves workspace retention settings', async () => {
  api.put.mockResolvedValue({ status: 'saved', unsynced_servers: [] });
  renderWithToast(<BackupRetentionCard />);
  fireEvent.change(await screen.findByLabelText(/Daily backups/), { target: { value: '14' } });
  fireEvent.change(screen.getByLabelText(/Monthly backups/), { target: { value: '6' } });
  fireEvent.change(screen.getByLabelText(/Removed database grace/), { target: { value: '45' } });
  fireEvent.click(screen.getByLabelText(/Clean up automatically/));
  fireEvent.click(screen.getByRole('button', { name: 'Save retention' }));
  await waitFor(() => expect(api.put).toHaveBeenCalledWith('control/storage/retention', { daily: 14, weekly: 4, monthly: 6, orphan_grace_days: 45, automatic: false }));
  await waitFor(() => expect(notify).toHaveBeenCalledWith('Backup retention saved.'));
});

it('sets and clears a per-database override', async () => {
  api.patch.mockResolvedValue({ status: 'saved' });
  const saved = vi.fn(async () => {});
  const view = renderWithToast(<DatabaseRetentionCard serverID="server-a" databaseID="portal" onSaved={saved} />);
  const useDefault = await screen.findByLabelText(/Use the workspace default \(7 daily · 4 weekly · 0 monthly\)/);
  expect(screen.queryByLabelText(/Daily backups/)).not.toBeInTheDocument();
  fireEvent.click(useDefault);
  fireEvent.change(screen.getByLabelText(/Daily backups/), { target: { value: '30' } });
  fireEvent.change(screen.getByLabelText(/Weekly backups/), { target: { value: '0' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save retention' }));
  await waitFor(() => expect(api.patch).toHaveBeenCalledWith('control/servers/server-a/databases/portal/backups/retention', { daily: 30, weekly: 0, monthly: 0 }));
  view.unmount();
  renderWithToast(<DatabaseRetentionCard serverID="server-a" databaseID="portal" override={{ daily: 30, weekly: 0, monthly: 0 }} onSaved={saved} />);
  expect(await screen.findByLabelText(/Daily backups/)).toHaveValue(30);
  fireEvent.click(screen.getByLabelText(/Use the workspace default/));
  fireEvent.click(screen.getByRole('button', { name: 'Save retention' }));
  await waitFor(() => expect(api.patch).toHaveBeenLastCalledWith('control/servers/server-a/databases/portal/backups/retention', null));
  expect(saved).toHaveBeenCalledTimes(2);
});
