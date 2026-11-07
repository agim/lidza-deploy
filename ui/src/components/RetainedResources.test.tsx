import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { RetainedResources, type RetainedResource, lastBackup, retainedSince, retainedWhy } from './RetainedResources';

const api = vi.hoisted(() => ({ get: vi.fn(), del: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn() }));
vi.mock('../lib/api', () => api);
vi.mock('./ToastProvider', () => ({ useToast: () => () => {} }));

afterEach(() => { cleanup(); vi.clearAllMocks(); });

const GiB = 1024 ** 3;
const hoursAgo = (n: number) => new Date(Date.now() - n * 3600_000).toISOString();

// The pilot's old host: two move copies, two removed applications and three
// databases no record holds, under the server id "local".
const moved: RetainedResource = {
  id: 'r-analytics', app_id: 'analytics-app', server_id: 'local', server_name: 'Old host', kind: 'application', reason: 'moved', runs_on: 'new', runs_on_name: 'New host', moved_at: '2026-10-20T10:00:00Z',
  origin: { move_id: 'm1', source_server: 'local', switched: true }, created: '0001-01-01T00:00:00Z',
  components: [{ kind: 'database', name: 'lidza-db-analytics-data', size_bytes: 22 * GiB, measured: true }, { kind: 'files', name: 'lidza-storage-analytics', size_bytes: 1024, measured: true }],
  databases: [{ id: 'analytics', engine: 'postgresql', volume: 'lidza-db-analytics-data', size_bytes: 22 * GiB, covered: true, last_offsite_backup: { id: 'b2', created: hoursAgo(48), offsite: true }, last_local_backup: { id: 'b1', created: hoursAgo(50) } }],
  size_bytes: 22 * GiB + 1024, final_backup_required: false,
};
const eventsStore: RetainedResource = { ...moved, id: 'r-events', app_id: 'events-store', databases: [], components: [{ kind: 'clickhouse', name: 'lidza-ch-events-data', size_bytes: 2048, measured: true }], size_bytes: 2048 };
const removed: RetainedResource = { id: 'r-portal', app_id: 'portal-example', server_id: 'local', server_name: 'Old host', kind: 'application', reason: 'removed', created: '2026-09-01T10:00:00Z', components: [{ kind: 'files', name: 'lidza-storage-portal', size_bytes: 100, measured: true }], size_bytes: 100, final_backup_required: false };
const removedCom: RetainedResource = { ...removed, id: 'r-portal-com', app_id: 'portal-example-com' };
const legacy = (app: string): RetainedResource => ({ id: `database-${app}`, app_id: app, server_id: 'local', server_name: 'Old host', kind: 'database', reason: 'removed', data_since: '2025-03-04T00:00:00Z', components: [{ kind: 'database', name: `lidza-db-${app}-data`, size_bytes: 10, measured: true }], databases: [{ id: app, engine: 'postgresql', volume: `lidza-db-${app}-data`, covered: false }], size_bytes: 10, final_backup_required: true });
const other: RetainedResource = { ...removed, id: 'r-blog', app_id: 'blog', server_id: 'beta', server_name: 'Beta' };
const pilot = [moved, eventsStore, removed, removedCom, legacy('personal-site'), legacy('exampl-portal'), legacy('portal-example'), other];

it('lists every kind of retained resource grouped by server name', async () => {
  api.get.mockResolvedValue({ retained_resources: pilot });
  const { container } = render(<RetainedResources />);
  expect(await screen.findByText('Old host')).toBeInTheDocument();
  const groups = container.querySelectorAll('[data-retained-server]');
  expect([...groups].map(group => group.getAttribute('data-retained-server'))).toEqual(['beta', 'local']);
  expect(screen.queryByText(/^local$/)).toBeNull();
  const old = within(groups[1] as HTMLElement);
  for (const app of ['analytics-app', 'events-store', 'portal-example', 'portal-example-com', 'personal-site', 'exampl-portal']) expect(old.getAllByText(app).length).toBeGreaterThan(0);
  expect(old.getAllByRole('row')).toHaveLength(8); // a header and seven entries
  expect(old.getByText('PostgreSQL analytics')).toBeInTheDocument();
  expect(old.getAllByText(/22\.0 GiB/).length).toBeGreaterThan(0);
  expect(old.getByText(/ClickHouse lidza-ch-events-data/)).toBeInTheDocument();
  // The internal id is secondary.
  expect(old.getByText('database-personal-site').closest('small')).toHaveClass('retained-id');
});

it('dates and explains each entry, never "before this agent recorded it"', async () => {
  expect(retainedSince(moved)).toBe(new Date('2026-10-20T10:00:00Z').toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' }));
  expect(retainedSince(legacy('personal-site'))).toMatch(/2025/);
  expect(retainedSince({ ...removed, created: undefined })).toBe('Date unknown');
  expect(retainedWhy(moved)).toMatch(/^Copy left by the move to New host on .+ \(the app runs there now\)$/);
  expect(retainedWhy(removed)).toMatch(/^Left when portal-example was removed on /);
  api.get.mockResolvedValue({ retained_resources: pilot });
  render(<RetainedResources />);
  await screen.findByText('Old host');
  expect(document.body.textContent).not.toMatch(/Before this agent recorded it/);
});

it('shows the last backup on one line with the full date on hover', () => {
  const backup = lastBackup(moved);
  expect(backup.text).toBe('2d ago · off-site + local');
  expect(backup.title).toBe(new Date(moved.databases![0].last_offsite_backup!.created).toLocaleString());
  expect(lastBackup(removed).text).toBe('No database');
  expect(lastBackup(legacy('personal-site'))).toMatchObject({ text: 'No backup', missing: true });
});

it('keeps the remove button on one line in a fixed column', async () => {
  api.get.mockResolvedValue({ retained_resources: [removed] });
  render(<RetainedResources />);
  const button = await screen.findByRole('button', { name: 'Remove' });
  expect(button).toHaveClass('retained-remove');
  expect(button.closest('td')).toHaveClass('retained-actions-col');
});

it('states what is deleted and needs the app name typed above 1 GB', async () => {
  api.get.mockResolvedValue([moved]);
  api.del.mockResolvedValue({ status: 'removing' });
  render(<RetainedResources serverID="local" />);
  fireEvent.click(await screen.findByRole('button', { name: 'Remove' }));
  const dialog = await screen.findByRole('alertdialog');
  expect(within(dialog).getByText(/Remove the data of analytics-app from Old host/)).toBeInTheDocument();
  expect(within(dialog).getByText(/PostgreSQL analytics \(22\.0 GiB\)/)).toBeInTheDocument();
  expect(within(dialog).getByText(/backups are not deleted: they belong to analytics-app on New host/)).toBeInTheDocument();
  const submit = within(dialog).getByRole('button', { name: 'Remove data' });
  expect(submit).toBeDisabled();
  fireEvent.change(within(dialog).getByLabelText(/to confirm/), { target: { value: 'analytics-app' } });
  fireEvent.click(submit);
  await waitFor(() => expect(api.del).toHaveBeenCalledWith('control/servers/local/retained-resources/r-analytics', { confirm: 'r-analytics', final_backup: false }));
});

it('removes small data without typing and requires a final backup only when none covers it', async () => {
  api.get.mockResolvedValue([legacy('personal-site')]);
  api.del.mockResolvedValue({ status: 'removing' });
  render(<RetainedResources serverID="local" />);
  fireEvent.click(await screen.findByRole('button', { name: 'Remove' }));
  const dialog = await screen.findByRole('alertdialog');
  expect(within(dialog).queryByLabelText(/to confirm/)).toBeNull();
  const box = within(dialog).getByRole('checkbox');
  expect(box).toBeChecked();
  expect(box).toBeDisabled();
  expect(within(dialog).getByText(/Required: no verified backup of this data/)).toBeInTheDocument();
  fireEvent.click(within(dialog).getByRole('button', { name: 'Remove data' }));
  await waitFor(() => expect(api.del).toHaveBeenCalledWith('control/servers/local/retained-resources/database-personal-site', { confirm: 'database-personal-site', final_backup: true }));
});

it('shows nothing for an application without retained data', async () => {
  api.get.mockResolvedValue({ retained_resources: [moved] });
  const { container } = render(<RetainedResources appID="blog" />);
  await waitFor(() => expect(api.get).toHaveBeenCalledWith('control/databases'));
  await waitFor(() => expect(container.querySelector('[data-retained-resources]')).toBeNull());
});

it('names the servers whose retained resources could not be read', async () => {
  api.get.mockResolvedValue({ retained_resources: [other], retained_resources_unavailable_servers: ['Old host'] });
  render(<RetainedResources />);
  expect(await screen.findByText(/Retained resources of Old host could not be read/)).toBeInTheDocument();
});
