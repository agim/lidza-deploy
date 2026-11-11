import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { DeletedAppDetailPage, DeletedAppsPage } from './DeletedAppsPage';
import type { ArchiveRow, ArchivesResponse } from '../lib/archive';

const api = vi.hoisted(() => ({ get: vi.fn(), del: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn() }));
vi.mock('../lib/api', () => api);
vi.mock('../components/ToastProvider', () => ({ useToast: () => () => {} }));

afterEach(() => { cleanup(); vi.clearAllMocks(); });

const row: ArchiveRow = {
  id: 'archive-1', app_id: 'shop', kind: 'container', repository: 'acme/shop', branch: 'main', deleted_at: '2026-11-01T10:00:00Z', deleted_by: 'admin@example.com', reason: 'replaced',
  expires_at: '2027-01-30T10:00:00Z', phase: 'archived', hostnames: ['shop.example.com', 'www.shop.example.com'], commit: 'abc1234567890', kept_bytes: 3 * 1024 * 1024,
  kept: [{ kind: 'settings', name: 'shop' }, { kind: 'volume', name: 'lidza-db-shop-data', detail: 'PostgreSQL database shop, container stopped', size_bytes: 3 * 1024 * 1024, measured: true }],
  deleted: [{ kind: 'backups', name: 'shop', count: 3, detail: '1 local, 2 off-site' }, { kind: 'cache', name: 'lidza-cache-shop', detail: 'Valkey container' }],
  id_available: true, server_id: 'alpha', server_name: 'Alpha', days_left: 85,
};
const data: ArchivesResponse = { archives: [row, { ...row, id: 'archive-2', app_id: 'old', days_left: 3, id_available: false }], purged: [{ archive_id: 'archive-0', app_id: 'gone', at: '2026-10-01T00:00:00Z', trigger: 'expiry', deleted: [], server_id: 'alpha', server_name: 'Alpha' }], retention_days: 90, unavailable_servers: [] };

it('lists deleted applications with days left and kept sizes, each linking to its page', async () => {
  api.get.mockResolvedValue(data);
  const { container } = render(<DeletedAppsPage />);
  expect(await screen.findByText('85 days left')).toBeInTheDocument();
  expect(api.get).toHaveBeenCalledWith('control/archives');
  expect(screen.getByText('3 days left')).toBeInTheDocument();
  expect(screen.getAllByText('3.0 MiB').length).toBe(2);
  expect(screen.getByRole('link', { name: 'shop' })).toHaveAttribute('href', '/console/deleted-apps/archive-1');
  expect(screen.getByText(/gone/)).toBeInTheDocument();
  // An index holds no actions on its rows.
  expect(screen.queryByRole('button', { name: 'Restore' })).toBeNull();
  expect(container.querySelector('[data-template=index]')).not.toBeNull();
});

it('shows what was kept and deleted on the application page', async () => {
  api.get.mockResolvedValue(data);
  render(<DeletedAppDetailPage id="archive-1" tab="overview" />);
  expect(await screen.findByText(/Backups \(3\)/)).toBeInTheDocument();
  expect(screen.getByText(/PostgreSQL database shop, container stopped/)).toBeInTheDocument();
  expect(screen.getByText('shop.example.com, www.shop.example.com')).toBeInTheDocument();
});

it('restores and reports skipped hostnames', async () => {
  api.get.mockResolvedValue(data);
  api.post.mockResolvedValue({ result: { app_id: 'shop', hostnames: ['shop.example.com'], skipped: { 'www.shop.example.com': 'taker' } }, webhook: 'the repository webhook was registered again', deployment: { id: 'd1' } });
  render(<DeletedAppDetailPage id="archive-1" tab="overview" />);
  fireEvent.click(await screen.findByRole('button', { name: 'Restore' }));
  const dialog = await screen.findByRole('alertdialog');
  fireEvent.click(within(dialog).getByRole('button', { name: 'Restore application' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('control/archives/archive-1/restore?server=alpha', {}));
  expect(await within(dialog).findByText(/www.shop.example.com \(taker\)/)).toBeInTheDocument();
});

it('asks for a new id when the old one is taken', async () => {
  api.get.mockResolvedValue(data);
  api.post.mockResolvedValue({ result: { app_id: 'old-2', hostnames: ['shop.example.com'] } });
  render(<DeletedAppDetailPage id="archive-2" tab="settings" />);
  fireEvent.click(await screen.findByRole('button', { name: 'Restore application' }));
  const dialog = await screen.findByRole('alertdialog');
  fireEvent.change(within(dialog).getByLabelText(/New application id/), { target: { value: 'old-2' } });
  fireEvent.click(within(dialog).getByRole('button', { name: 'Restore application' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('control/archives/archive-2/restore?server=alpha', { new_id: 'old-2' }));
});

it('deletes permanently from the Danger zone only after the application id is typed', async () => {
  api.get.mockResolvedValue(data);
  api.del.mockResolvedValue({});
  const { container } = render(<DeletedAppDetailPage id="archive-1" tab="settings" />);
  const button = await screen.findByRole('button', { name: 'Delete now' });
  expect(container.querySelector('[data-danger-zone="archive-1"]')).toContainElement(button);
  fireEvent.click(button);
  const dialog = await screen.findByRole('alertdialog');
  const submit = within(dialog).getByRole('button', { name: 'Delete permanently' });
  expect(submit).toBeDisabled();
  fireEvent.change(within(dialog).getByLabelText(/permanently$/), { target: { value: 'shop' } });
  fireEvent.click(submit);
  await waitFor(() => expect(api.del).toHaveBeenCalledWith('control/archives/archive-1?server=alpha', { confirm: 'shop' }));
});
