import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';

const api = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn() }));
vi.mock('../lib/api', () => api);
vi.mock('./ToastProvider', () => ({ useToast: () => vi.fn() }));

import { ServerServiceControl } from './ServerServiceControl';

afterEach(() => { cleanup(); vi.clearAllMocks(); });

it('turns service control of adopted applications on from the panel and explains what it allows', async () => {
  api.get.mockResolvedValue({ supported: true, enabled: false, source: 'config', config_default: false, account: 'deploy' });
  api.put.mockResolvedValue({ supported: true, enabled: true, source: 'setting', config_default: false, account: 'deploy' });
  render(<ServerServiceControl serverID="edge" />);
  const toggle = await screen.findByRole('switch', { name: 'Service control for adopted applications' });
  expect(api.get).toHaveBeenCalledWith('control/servers/edge/host/service-control');
  expect(toggle).toHaveAttribute('aria-checked', 'false');
  expect(screen.getByText(/start, stop and restart its systemd units/)).toBeInTheDocument();
  expect(screen.getByText(/shared deployment account/)).toBeInTheDocument();
  expect(screen.getByText('deploy')).toBeInTheDocument();
  expect(screen.getByText('Agent configuration default')).toBeInTheDocument();
  fireEvent.click(toggle);
  await waitFor(() => expect(api.put).toHaveBeenCalledWith('control/servers/edge/host/service-control', { enabled: true }));
  await waitFor(() => expect(screen.getByRole('switch', { name: 'Service control for adopted applications' })).toHaveAttribute('aria-checked', 'true'));
  expect(screen.getByText('Set here')).toBeInTheDocument();
});

it('is hidden on servers without native management and reports a failed change', async () => {
  api.get.mockResolvedValue({ supported: false, enabled: false, source: 'config', config_default: false });
  const { container } = render(<ServerServiceControl serverID="edge" />);
  await waitFor(() => expect(api.get).toHaveBeenCalled());
  await waitFor(() => expect(container.querySelector('[data-server-service-control]')).toBeNull());
  expect(screen.queryByRole('switch')).toBeNull();
  cleanup();

  api.get.mockResolvedValue({ supported: true, enabled: true, source: 'setting', config_default: false });
  api.put.mockRejectedValue(new Error('could not save the service control setting'));
  render(<ServerServiceControl serverID="edge" />);
  fireEvent.click(await screen.findByRole('switch', { name: 'Service control for adopted applications' }));
  expect(await screen.findByText('could not save the service control setting')).toBeInTheDocument();
  expect(screen.getByRole('switch', { name: 'Service control for adopted applications' })).toHaveAttribute('aria-checked', 'true');
});
