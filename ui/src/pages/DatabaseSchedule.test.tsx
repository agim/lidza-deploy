import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { expect, it, vi } from 'vitest';
import { DatabaseDetailPage } from './DataPages';

const api = vi.hoisted(() => ({ get: vi.fn(), patch: vi.fn(), post: vi.fn() }));
vi.mock('../lib/api', () => api);
vi.mock('../components/ToastProvider', () => ({ useToast: () => () => {} }));
vi.mock('../state/WorkspaceContext', () => ({ useWorkspace: () => ({ apps: [] }) }));

it('saves a calendar backup on the exact selected database resource', async () => {
  api.get.mockResolvedValue([{ app_id: 'database-a', server_id: 'server-a', ready: true,
    backup: { hours: 24, keep: 1, offsite: true }, backups: [] }]);
  api.patch.mockResolvedValue({});
  render(<DatabaseDetailPage appID="database-a" />);
  fireEvent.change(await screen.findByLabelText('Frequency'), { target: { value: 'calendar' } });
  fireEvent.change(screen.getByPlaceholderText('0 3 * * 1'), { target: { value: '@monthly' } });
  fireEvent.change(screen.getByLabelText(/Timezone/), { target: { value: 'Europe/Tirane' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save backup schedule' }));
  await waitFor(() => expect(api.patch).toHaveBeenCalledWith('control/servers/server-a/databases/database-a/backups', {
    hours: 0, cron: '@monthly', timezone: 'Europe/Tirane', keep: 1, offsite: true,
  }));
});
