import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { DatabasesPage } from './DataPages';

const api = vi.hoisted(() => ({ get: vi.fn(), patch: vi.fn(), post: vi.fn(), del: vi.fn() }));
vi.mock('../lib/api', () => api);
vi.mock('../components/ToastProvider', () => ({ useToast: () => () => {} }));
vi.mock('../state/WorkspaceContext', () => ({ useWorkspace: () => ({ apps: [{ id: 'shop' }] }) }));
vi.mock('../state/PreferencesContext', () => ({ usePreference: (_key: string, initial: string) => [initial, () => {}] }));

afterEach(() => { cleanup(); vi.clearAllMocks(); });

const fleet = {
  databases: [{ app_id: 'shop', server_id: 'server-a', ready: true, backup: { keep: 1 }, backups: [], attachments: [{ app_id: 'shop' }] }],
  retained_resources: [
    { id: 'r-old', app_id: 'legacy', server_id: 'server-a', server_name: 'Alpha', kind: 'database', created: '2026-10-01T10:00:00Z', size_bytes: 2 ** 20, databases: [{ id: 'legacy', engine: 'postgresql', size_bytes: 2 ** 20, covered: true }] },
    { id: 'r-shared', app_id: 'shared', server_id: 'server-a', server_name: 'Alpha', kind: 'database', size_bytes: 2 ** 20, attached_to: ['shop'] },
  ],
};

it('lists the databases no application uses and removes one from the Databases page', async () => {
  api.get.mockImplementation(async (path: string) => path === 'control/databases' ? fleet : undefined);
  api.del.mockResolvedValue({ status: 'removing' });
  render(<DatabasesPage />);
  expect(await screen.findByRole('link', { name: 'shop' })).toBeInTheDocument();
  expect(screen.queryByText('legacy')).not.toBeInTheDocument();
  fireEvent.change(screen.getByRole('combobox', { name: 'Show' }), { target: { value: 'unused' } });
  expect(await screen.findByText('legacy')).toBeInTheDocument();
  // Data an application still uses is not offered as unused.
  expect(screen.queryByText('shared')).not.toBeInTheDocument();
  expect(screen.queryByRole('link', { name: 'shop' })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Remove' }));
  const dialog = await screen.findByRole('alertdialog');
  expect(dialog).toHaveTextContent('Remove the data of legacy from Alpha?');
  fireEvent.click(within(dialog).getByRole('button', { name: 'Remove data' }));
  await waitFor(() => expect(api.del).toHaveBeenCalledWith('control/servers/server-a/retained-resources/r-old', { confirm: 'r-old', final_backup: false }));
});
