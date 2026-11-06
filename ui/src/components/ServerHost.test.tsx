import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { HostUpdateBadges, ServerUpdates } from './ServerUpdates';
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

it('lists disruptive updates apart and only includes them after confirmation', async () => {
  const status = {
    supported: true, pending: 3, security: 2, disruptive: 1, reboot_required: false, unattended_installed: true, unattended_enabled: true, configured: true,
    packages: [{ name: 'libssl3t64', current: '3.0.13-0ubuntu3.4', candidate: '3.0.13-0ubuntu3.5', security: true }, { name: 'docker-ce', current: '27.3.1', candidate: '28.0.1', security: false, disruptive: true, effect: 'restarts the container runtime: all containers restart unless Docker live-restore is on' }],
    disruptive_packages: [{ name: 'docker-ce', current: '27.3.1', candidate: '28.0.1', security: false, disruptive: true, effect: 'restarts the container runtime: all containers restart unless Docker live-restore is on' }],
    docker: { installed: true, running: true, live_restore: false, live_restore_configured: false, managed: true, warning: 'Docker live-restore is off: a Docker upgrade or daemon restart stops and restarts every container.' },
    policy: { auto_security: true, auto_reboot: true, manage_package_sources: false, disruptive_in_window: true, reboot_window: { weekdays: [0], hour: 3, duration_minutes: 60 } },
    next_disruptive_window: '2026-11-08T03:00:00Z', jobs: [],
  };
  api.get.mockResolvedValue(status);
  api.post.mockResolvedValue(status);
  render(<ServerUpdates serverID="edge" />);
  expect(await screen.findByText('Disruptive updates')).toBeInTheDocument();
  expect(screen.getByText('restarts the container runtime: all containers restart unless Docker live-restore is on')).toBeInTheDocument();
  expect(screen.getByText('Docker live-restore off')).toBeInTheDocument();
  expect(screen.getByText(/a Docker upgrade or daemon restart stops and restarts every container/)).toBeInTheDocument();

  fireEvent.click(screen.getByRole('button', { name: 'Install security updates now' }));
  expect(await screen.findByText(/Docker, Caddy and PostgreSQL repositories have no security channel/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));

  fireEvent.click(screen.getByRole('button', { name: 'Install all updates' }));
  expect(await screen.findByText(/except these disruptive ones, which stay pending: docker-ce/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Install all updates' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('control/servers/edge/host/updates/run', { scope: 'all', include_disruptive: false }));
  await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument());

  fireEvent.click(screen.getByRole('button', { name: 'Include disruptive updates' }));
  expect(await screen.findByText(/docker-ce restarts the container runtime/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Install including disruptive' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('control/servers/edge/host/updates/run', { scope: 'all', include_disruptive: true }));
});
