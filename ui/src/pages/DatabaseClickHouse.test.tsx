import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { DatabaseDetailPage, DatabasesPage } from './DataPages';
vi.mock('../state/WorkspaceContext', () => ({ useWorkspace: () => ({ apps: [{ id: 'events' }] }) }));

const api = vi.hoisted(() => ({ get: vi.fn(), patch: vi.fn(), post: vi.fn() }));
vi.mock('../lib/api', () => api);
vi.mock('../components/ToastProvider', () => ({ useToast: () => () => {} }));
afterEach(() => { cleanup(); vi.clearAllMocks(); });

it('labels ClickHouse databases and restores a backup in place after confirmation', async () => {
  api.get.mockResolvedValue([{ app_id: 'events-ch0a1b2c3d', server_id: 'server-a', engine: 'clickhouse', mode: 'external', size_bytes: 3 * 2 ** 20, ready: true,
    backup: { hours: 24, keep: 1 }, backups: [{ id: 'b1', created: '2026-11-05T10:00:00Z', size: 2048, sha256: 'a'.repeat(64) }] }]);
  api.post.mockResolvedValue({});
  render(<DatabaseDetailPage appID="events-ch0a1b2c3d" />);
  expect(await screen.findByText('ClickHouse · server-a')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Restore' }));
  expect(await screen.findByText('Restore this ClickHouse backup?')).toBeInTheDocument();
  expect(api.post).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole('button', { name: 'Replace and restore' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('control/servers/server-a/databases/events-ch0a1b2c3d/backups/b1/restore'));
});

it('lists an application ClickHouse database with its size and backups', async () => {
  api.get.mockResolvedValue({ databases: [
    { app_id: 'events-db0a1b2c3d', server_id: 'server-a', mode: 'external', ready: true, backup: { hours: 24, keep: 1 }, backups: [], attachments: [{ app_id: 'events', env_key: 'DATABASE_URL' }] },
    { app_id: 'events-ch0a1b2c3d', server_id: 'server-a', engine: 'clickhouse', mode: 'external', size_bytes: 3 * 2 ** 20, ready: true, backup: { hours: 24, keep: 1 },
      backups: [{ id: 'b1', created: '2026-11-05T10:00:00Z', size: 2048 }], attachments: [{ app_id: 'events', env_key: 'CLICKHOUSE_URL' }] },
  ] });
  render(<DatabasesPage />);
  expect(await screen.findByText('ClickHouse · server-a')).toBeInTheDocument();
  expect(screen.getByText('External PostgreSQL · server-a')).toBeInTheDocument();
  const card = screen.getByText('ClickHouse · server-a').closest('.database-card')!;
  expect(card).toHaveTextContent('Size 3.0 MiB');
  expect(card).toHaveTextContent('Backups 1');
});
