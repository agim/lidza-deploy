import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';

const api = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn() }));
vi.mock('../lib/api', () => api);
vi.mock('./ToastProvider', () => ({ useToast: () => vi.fn() }));

import { ServerLogAccess } from './ServerLogAccess';

afterEach(() => { cleanup(); vi.clearAllMocks(); });

const name = 'System log access for the deployment account';

it('gives the deployment account system log access from the panel and shows its membership', async () => {
  api.get.mockResolvedValue({ supported: true, enabled: false, source: 'config', account: 'deploy', group: 'systemd-journal', member: false });
  api.put.mockResolvedValue({ supported: true, enabled: true, source: 'setting', account: 'deploy', group: 'systemd-journal', member: true });
  render(<ServerLogAccess serverID="edge" />);
  const toggle = await screen.findByRole('switch', { name });
  expect(api.get).toHaveBeenCalledWith('control/servers/edge/host/log-access');
  expect(toggle).toHaveAttribute('aria-checked', 'false');
  expect(screen.getByText(/read every service’s logs on this server with/)).toBeInTheDocument();
  expect(screen.getByText('journalctl -u <unit>')).toBeInTheDocument();
  expect(screen.getByText(/may contain secrets/)).toBeInTheDocument();
  expect(screen.getByText('Not in systemd-journal')).toBeInTheDocument();
  expect(screen.getByText(/leaves the membership as it is/)).toBeInTheDocument();
  fireEvent.click(toggle);
  await waitFor(() => expect(api.put).toHaveBeenCalledWith('control/servers/edge/host/log-access', { enabled: true }));
  await waitFor(() => expect(screen.getByRole('switch', { name })).toHaveAttribute('aria-checked', 'true'));
  expect(screen.getByText('In systemd-journal')).toBeInTheDocument();
});

it('is hidden without a shared deployment account and reports a failed membership change', async () => {
  api.get.mockResolvedValue({ supported: false, reason: 'native management is not configured on this server', enabled: false, source: 'config', group: 'systemd-journal', member: false });
  const { container } = render(<ServerLogAccess serverID="edge" />);
  await waitFor(() => expect(api.get).toHaveBeenCalled());
  await waitFor(() => expect(container.querySelector('[data-server-log-access]')).toBeNull());
  expect(screen.queryByRole('switch')).toBeNull();
  cleanup();

  api.get.mockResolvedValue({ supported: true, enabled: false, source: 'setting', account: 'deploy', group: 'systemd-journal', member: false });
  api.put.mockResolvedValue({ supported: true, enabled: true, source: 'setting', account: 'deploy', group: 'systemd-journal', member: false, error: 'could not add deploy to the systemd-journal group: exit status 3' });
  render(<ServerLogAccess serverID="edge" />);
  fireEvent.click(await screen.findByRole('switch', { name }));
  expect(await screen.findByText(/could not add deploy to the systemd-journal group/)).toBeInTheDocument();
  expect(screen.getByText('Not in systemd-journal')).toBeInTheDocument();
});
