import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { HostUpdateBadges } from './ServerUpdates';
import { ServerFirewall } from './ServerFirewall';
import type { HostFirewallView } from '../lib/types';

const api = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), post: vi.fn(), patch: vi.fn() }));
vi.mock('../lib/api', () => api);
vi.mock('./ToastProvider', () => ({ useToast: () => () => {} }));

afterEach(() => { vi.clearAllMocks(); });

const firewall: HostFirewallView = {
  supported: true, backend: 'ufw', installed: true, active: true, default_incoming: 'deny',
  model: { enabled: true, default_inbound: 'deny', rules: [{ port: '5432', proto: 'tcp', from: ['203.0.113.10'], comment: 'replica', source: 'user', enforced: 'ufw' }] },
  rules: [
    { port: '22', proto: 'tcp', comment: 'SSH', source: 'system', enforced: 'ufw' },
    { port: '5432', proto: 'tcp', from: ['203.0.113.10'], comment: 'replica', source: 'user', enforced: 'ufw' },
    { port: '7000', proto: 'tcp', comment: 'Container public port (app)', source: 'app:relay', enforced: 'docker' },
  ],
};

it('shows reboot and security badges on the server card', async () => {
  api.get.mockResolvedValue({ supported: true, pending: 4, security: 2, reboot_required: true });
  render(<HostUpdateBadges serverID="edge" />);
  expect(await screen.findByText('Reboot required')).toBeInTheDocument();
  expect(screen.getByText('2 security updates')).toBeInTheDocument();
  expect(api.get).toHaveBeenCalledWith('control/servers/edge/host/updates');
});

it('reviews, applies and confirms a firewall change', async () => {
  api.get.mockImplementation(async (path: string) => path.endsWith('/plan')
    ? { changes: [{ action: 'add', command: "ufw allow 443/udp comment 'lidza: HTTP/3'" }], fingerprint: 'abc123', confirm_seconds: 120 }
    : firewall);
  api.post.mockImplementation(async (path: string) => path.endsWith('/apply') ? { ...firewall, pending_seconds: 120 } : firewall);
  render(<ServerFirewall serverID="edge" />);
  expect(await screen.findByText('203.0.113.10')).toBeInTheDocument();
  expect(screen.getByText('App · relay · Docker')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Review changes' }));
  expect(await screen.findByText("ufw allow 443/udp comment 'lidza: HTTP/3'")).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Apply firewall' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('control/servers/edge/host/firewall/apply', { fingerprint: 'abc123' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('control/servers/edge/host/firewall/confirm', {}));
  expect(api.put).not.toHaveBeenCalled();
});
