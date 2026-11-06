import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { BackupExcludedTablesCard, skippedTablesLabel, tableSummary } from './BackupExcludedTables';

const api = vi.hoisted(() => ({ get: vi.fn(), patch: vi.fn(), post: vi.fn() }));
const toast = vi.hoisted(() => vi.fn());
vi.mock('../lib/api', () => api);
vi.mock('./ToastProvider', () => ({ useToast: () => toast }));

afterEach(() => { cleanup(); vi.clearAllMocks(); });

const listing = {
  total: 4, excluded: ['public.events'],
  tables: [
    { name: 'public.events', rows: 52_000, bytes: 80 * 2 ** 20, excludable: true, excluded: true },
    { name: 'public.orders', rows: 1200, bytes: 400 * 2 ** 10, excludable: true, excluded: false },
    { name: 'audit.log_entries', rows: -1, bytes: 8192, excludable: true, excluded: false },
    { name: 'public.parted', rows: 0, bytes: 0, excludable: false, excluded: false },
  ],
};

it('lists the skipped tables with rows and size, warns about restores and saves a new choice', async () => {
  api.get.mockResolvedValue(listing);
  api.patch.mockResolvedValue({ status: 'saved' });
  const saved = vi.fn();
  const { container } = render(<BackupExcludedTablesCard serverID="server-a" databaseID="shop" excluded={['public.events']} onSaved={saved} />);
  expect(await screen.findByText('about 52k rows, 80.0 MiB')).toBeInTheDocument();
  expect(api.get).toHaveBeenCalledWith('control/servers/server-a/databases/shop/tables');
  expect(screen.getByRole('note')).toHaveTextContent('Restores from these backups will not contain this data');

  fireEvent.click(screen.getByRole('button', { name: 'Choose tables' }));
  const events = screen.getByRole('checkbox', { name: 'Skip the data of public.events' });
  expect(events).toBeChecked();
  // Partitioned parents and other tables that cannot be skipped are not offered.
  expect(screen.queryByRole('checkbox', { name: 'Skip the data of public.parted' })).not.toBeInTheDocument();
  // An unknown row estimate shows the size only.
  expect(screen.getByText('8.0 KiB')).toBeInTheDocument();
  expect(container).not.toHaveTextContent('rows unknown');

  fireEvent.change(screen.getByRole('searchbox', { name: 'Filter tables' }), { target: { value: 'audit' } });
  expect(screen.queryByRole('checkbox', { name: 'Skip the data of public.events' })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole('checkbox', { name: 'Skip the data of audit.log_entries' }));
  expect(screen.getByText(/Restores will not contain the data of 2 tables/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Save skipped tables' }));
  await waitFor(() => expect(api.patch).toHaveBeenCalledWith('control/servers/server-a/databases/shop/backups/excluded-tables', { tables: ['audit.log_entries', 'public.events'] }));
  await waitFor(() => expect(saved).toHaveBeenCalled());
  expect(toast).toHaveBeenCalledWith('Backups of shop skip the data of 2 tables.');
  expect(container.querySelectorAll('[style]')).toHaveLength(0);
});

it('refuses to skip every table and can clear the list', async () => {
  api.get.mockResolvedValue({ ...listing, total: 2, tables: listing.tables.slice(0, 2) });
  api.patch.mockResolvedValue({ status: 'saved' });
  render(<BackupExcludedTablesCard serverID="server-a" databaseID="shop" excluded={['public.events']} />);
  fireEvent.click(await screen.findByRole('button', { name: 'Choose tables' }));
  fireEvent.click(screen.getByRole('checkbox', { name: 'Skip the data of public.orders' }));
  expect(screen.getByText('Backups must keep the data of at least one table.')).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Save skipped tables' })).toBeDisabled();
  fireEvent.click(screen.getByRole('checkbox', { name: 'Skip the data of public.orders' }));
  fireEvent.click(screen.getByRole('checkbox', { name: 'Skip the data of public.events' }));
  fireEvent.click(screen.getByRole('button', { name: 'Save skipped tables' }));
  await waitFor(() => expect(api.patch).toHaveBeenCalledWith('control/servers/server-a/databases/shop/backups/excluded-tables', { tables: [] }));
  expect(toast).toHaveBeenCalledWith('Backups of shop contain every table again.');
});

it('shows an agent refusal and keeps the editor open', async () => {
  api.get.mockResolvedValue(listing);
  api.patch.mockRejectedValue(new Error('no such table in shop: public.orders'));
  render(<BackupExcludedTablesCard serverID="server-a" databaseID="shop" excluded={[]} />);
  expect(await screen.findByText('Backups contain the data of every table.')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Choose tables' }));
  fireEvent.click(screen.getByRole('checkbox', { name: 'Skip the data of public.orders' }));
  fireEvent.click(screen.getByRole('button', { name: 'Save skipped tables' }));
  await waitFor(() => expect(toast).toHaveBeenCalledWith('no such table in shop: public.orders', true));
  expect(within(document.body).getByRole('button', { name: 'Save skipped tables' })).toBeEnabled();
});

it('names skipped table data for backup listings', () => {
  expect(skippedTablesLabel(['public.events'])).toBe('data of 1 table skipped');
  expect(skippedTablesLabel(['public.events', 'public.audit_log'])).toBe('data of 2 tables skipped');
  expect(skippedTablesLabel(undefined)).toBe('');
  expect(tableSummary({ rows: -1, bytes: -1 })).toBe('not measured');
  expect(tableSummary({ rows: 3_400_000, bytes: 2 ** 30 })).toBe('about 3.4M rows, 1.0 GiB');
});
