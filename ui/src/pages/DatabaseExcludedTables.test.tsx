import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { DatabaseDetailPage } from './DataPages';

const api = vi.hoisted(() => ({ get: vi.fn(), patch: vi.fn(), post: vi.fn() }));
vi.mock('../lib/api', () => api);
vi.mock('../components/ToastProvider', () => ({ useToast: () => () => {} }));
vi.mock('../state/WorkspaceContext', () => ({ useWorkspace: () => ({ apps: [] }) }));

afterEach(() => { cleanup(); vi.clearAllMocks(); });

const database = { app_id: 'shop', server_id: 'server-a', ready: true, primary: true, backup: { hours: 24, keep: 1 }, before_deploy: { mode: 'always' }, excluded_tables: ['public.events'], excluded_tables_supported: true,
  backups: [{ id: 'b1', created: '2026-11-05T10:00:00Z', size: 4096, excluded_table_data: ['public.events'] }, { id: 'b0', created: '2026-11-04T10:00:00Z', size: 8192 }] };

it('shows the skipped-tables tab and notes skipped data in the backup history', async () => {
  api.get.mockImplementation(async (path: string) => {
    if (path === 'control/databases') return [database];
    if (path === 'control/servers/server-a/databases/shop/tables') return { total: 2, excluded: ['public.events'], tables: [{ name: 'public.events', rows: 52_000, bytes: 80 * 2 ** 20, excludable: true, excluded: true }, { name: 'public.orders', rows: 10, bytes: 8192, excludable: true, excluded: false }] };
    return undefined;
  });
  const excluded = render(<DatabaseDetailPage appID="shop" tab="excluded" />);
  expect(await screen.findByText('Backups skip data of these tables')).toBeInTheDocument();
  expect(await screen.findByText('about 52k rows, 80.0 MiB')).toBeInTheDocument();
  excluded.unmount();
  render(<DatabaseDetailPage appID="shop" tab="backups" />);
  expect(await screen.findByText('data of 1 table skipped')).toBeInTheDocument();
  expect(screen.getByRole('tab', { name: 'Excluded tables' })).toBeInTheDocument();
});

it('offers no skipped-tables setting where backups always contain every table', async () => {
  api.get.mockImplementation(async (path: string) => path === 'control/databases' ? [{ ...database, excluded_tables: [], excluded_tables_supported: false, backups: [] }] : undefined);
  render(<DatabaseDetailPage appID="shop" tab="settings" />);
  expect((await screen.findAllByText('Before deploys')).length).toBeGreaterThan(0);
  expect(screen.queryByRole('tab', { name: 'Excluded tables' })).not.toBeInTheDocument();
  expect(screen.queryByText('Backups skip data of these tables')).not.toBeInTheDocument();
});
