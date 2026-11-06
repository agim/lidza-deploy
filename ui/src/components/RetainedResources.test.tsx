import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { RetainedResources, type RetainedResource, retainedWhy } from './RetainedResources';

const api = vi.hoisted(() => ({ get: vi.fn(), del: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn() }));
vi.mock('../lib/api', () => api);
vi.mock('./ToastProvider', () => ({ useToast: () => () => {} }));

afterEach(() => { cleanup(); vi.clearAllMocks(); });

const moved: RetainedResource = {
  id: 'r1', app_id: 'shop', server_id: 'alpha', kind: 'application', created: '2026-11-01T10:00:00Z', reason: 'moved', runs_on: 'beta', runs_on_name: 'Beta',
  components: [{ kind: 'database', name: 'lidza-db-shop-data', size_bytes: 2048, measured: true }, { kind: 'files', name: 'lidza-storage-shop', size_bytes: 1024, measured: true }],
  databases: [{ id: 'shop', covered: true, last_offsite_backup: { id: 'b2', created: '2026-11-01T09:59:00Z', offsite: true } }],
  size_bytes: 3072, final_backup_required: false,
};
const removed: RetainedResource = { id: 'database-old', app_id: 'old', server_id: 'alpha', kind: 'database', reason: 'removed', components: [{ kind: 'database', name: 'lidza-db-old-data' }], size_pending: true, databases: [{ id: 'old', covered: false }], final_backup_required: true };
const attached: RetainedResource = { id: 'r3', app_id: 'blog', server_id: 'alpha', kind: 'application', reason: 'removed', components: [], attached_to: ['reporting'], final_backup_required: false };

it('lists retained resources with size, reason and last backup', async () => {
  api.get.mockResolvedValue([moved, removed, attached]);
  render(<RetainedResources serverID="alpha" />);
  expect(await screen.findByText('Moved — now runs on Beta')).toBeInTheDocument();
  expect(api.get).toHaveBeenCalledWith('control/servers/alpha/retained-resources');
  expect(screen.getByText('3.0 KiB')).toBeInTheDocument();
  expect(screen.getByText('Measuring…')).toBeInTheDocument();
  expect(screen.getByText(/shop: off-site/)).toBeInTheDocument();
  expect(screen.getByText('old: no backup (final backup needed)')).toBeInTheDocument();
  expect(screen.getByText('In use by reporting')).toBeInTheDocument();
  expect(screen.getAllByRole('button', { name: 'Remove' })).toHaveLength(2);
  expect(retainedWhy({ ...removed, kind: 'application' })).toBe('Application deleted, data kept');
});

it('removes only after the id is typed, with an optional final backup', async () => {
  api.get.mockResolvedValue([moved]);
  api.del.mockResolvedValue({ status: 'removing' });
  render(<RetainedResources serverID="alpha" />);
  fireEvent.click(await screen.findByRole('button', { name: 'Remove' }));
  const dialog = await screen.findByRole('alertdialog');
  const submit = within(dialog).getByRole('button', { name: 'Remove' });
  expect(submit).toBeDisabled();
  fireEvent.change(within(dialog).getByLabelText(/to confirm/), { target: { value: 'r' } });
  expect(submit).toBeDisabled();
  fireEvent.click(within(dialog).getByRole('checkbox'));
  fireEvent.change(within(dialog).getByLabelText(/to confirm/), { target: { value: 'r1' } });
  fireEvent.click(submit);
  await waitFor(() => expect(api.del).toHaveBeenCalledWith('control/servers/alpha/retained-resources/r1', { confirm: 'r1', final_backup: true }));
});

it('requires a final backup when no backup covers a database', async () => {
  api.get.mockResolvedValue({ retained_resources: [removed] });
  api.del.mockResolvedValue({ status: 'removing' });
  render(<RetainedResources />);
  fireEvent.click(await screen.findByRole('button', { name: 'Remove' }));
  const dialog = await screen.findByRole('alertdialog');
  const box = within(dialog).getByRole('checkbox');
  expect(box).toBeChecked();
  expect(box).toBeDisabled();
  expect(within(dialog).getByText(/Required: no verified backup/)).toBeInTheDocument();
  fireEvent.change(within(dialog).getByLabelText(/to confirm/), { target: { value: 'database-old' } });
  fireEvent.click(within(dialog).getByRole('button', { name: 'Remove' }));
  await waitFor(() => expect(api.del).toHaveBeenCalledWith('control/servers/alpha/retained-resources/database-old', { confirm: 'database-old', final_backup: true }));
});

it('shows nothing for an application without retained data', async () => {
  api.get.mockResolvedValue({ retained_resources: [moved] });
  const { container } = render(<RetainedResources appID="blog" />);
  await waitFor(() => expect(api.get).toHaveBeenCalledWith('control/databases'));
  await waitFor(() => expect(container.querySelector('[data-retained-resources]')).toBeNull());
});
