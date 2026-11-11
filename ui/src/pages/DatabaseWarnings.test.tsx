import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { DatabaseDetailPage } from './DataPages';

const api = vi.hoisted(() => ({ get: vi.fn(), patch: vi.fn(), post: vi.fn() }));
vi.mock('../lib/api', () => api);
vi.mock('../components/ToastProvider', () => ({ useToast: () => () => {} }));
vi.mock('../state/WorkspaceContext', () => ({ useWorkspace: () => ({ apps: [] }) }));

afterEach(() => { cleanup(); vi.clearAllMocks(); });

const warning = 'backup tool /usr/bin/sqlite3 changed and was not re-approved: its MD5 differs from package sqlite3\'s md5sums record; review the binary before backups resume';

it('shows a backup tool refusal on the affected database', async () => {
  api.get.mockImplementation(async (path: string) => path === 'control/databases' ? [{ app_id: 'shop-queue', server_id: 'server-a', engine: 'sqlite', ready: true, backup: { keep: 1 }, backups: [], warnings: [warning] }] : undefined);
  render(<DatabaseDetailPage appID="shop-queue" />);
  expect(await screen.findByText(warning)).toBeInTheDocument();
  expect(screen.getAllByText('needs attention').length).toBeGreaterThan(0);
});
