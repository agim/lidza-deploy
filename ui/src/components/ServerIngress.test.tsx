import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { ServerIngress } from './ServerIngress';
import { environmentSources } from './MigrationPlan';
import type { HostIngressStatus } from '../lib/types';

const api = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), post: vi.fn(), patch: vi.fn() }));
vi.mock('../lib/api', () => api);
vi.mock('./ToastProvider', () => ({ useToast: () => () => {} }));

afterEach(() => { cleanup(); vi.clearAllMocks(); });

const detected: HostIngressStatus = { mode: 'agent', source: 'detected', detected: 'agent', admin_api: false, web_ports: [80, 443], detail: 'the host\'s web server has no Caddy admin API on 127.0.0.1:2019; the agent proxy is the ingress behind it', checked: '2026-11-05T10:00:00Z', configured: false };
const caddy: HostIngressStatus = { ...detected, mode: 'caddy', source: 'setting', caddy_admin: 'http://127.0.0.1:2019', caddy_server: 'srv0', override: { admin: 'http://127.0.0.1:2019', server: 'srv0' } };

it('shows the detected agent proxy and stores a Caddy override', async () => {
  api.get.mockResolvedValue(detected);
  api.put.mockResolvedValue(caddy);
  render(<ServerIngress serverID="edge" />);
  expect(await screen.findByText('Līdza agent proxy')).toBeInTheDocument();
  expect(screen.getByText('Detected')).toBeInTheDocument();
  expect(api.get).toHaveBeenCalledWith('control/servers/edge/host/ingress');
  fireEvent.change(screen.getByPlaceholderText('http://127.0.0.1:2019'), { target: { value: 'http://127.0.0.1:2019' } });
  fireEvent.change(screen.getByPlaceholderText('srv0'), { target: { value: 'srv0' } });
  fireEvent.click(screen.getByRole('button', { name: 'Use this Caddy' }));
  await waitFor(() => expect(api.put).toHaveBeenCalledWith('control/servers/edge/host/ingress', { caddy: { admin: 'http://127.0.0.1:2019', server: 'srv0' } }));
  expect(await screen.findByText('Caddy (admin API on 127.0.0.1:2019)')).toBeInTheDocument();
  expect(screen.getByText('Set here')).toBeInTheDocument();
  api.put.mockResolvedValue(detected);
  fireEvent.click(screen.getByRole('button', { name: 'Detect automatically' }));
  await waitFor(() => expect(api.put).toHaveBeenLastCalledWith('control/servers/edge/host/ingress', { caddy: null }));
  expect(await screen.findByText('Līdza agent proxy')).toBeInTheDocument();
});

it('refreshes detection and leaves configuration-file settings read-only', async () => {
  api.get.mockResolvedValue({ ...caddy, source: 'config', configured: true, override: undefined });
  render(<ServerIngress serverID="edge" />);
  expect(await screen.findByText('Caddy (admin API on 127.0.0.1:2019)')).toBeInTheDocument();
  expect(screen.getByText('Agent configuration file')).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: 'Use this Caddy' })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Detect again' }));
  await waitFor(() => expect(api.get).toHaveBeenLastCalledWith('control/servers/edge/host/ingress?refresh=1'));
});

it('summarises where copied variables come from', () => {
  expect(environmentSources({ panel: 14, 'deploy defaults': 6, image: 2 })).toBe(' (14 from settings, 6 from deploy defaults, 2 from image)');
  expect(environmentSources({ panel: 3 })).toBe('');
  expect(environmentSources(undefined)).toBe('');
});

it('reports unmanaged error pages and adopts the managed ones', async () => {
  const pages = { dir: '/etc/lidza/pages', ready: true, caddy_user: 'caddy', managed: 1, unmanaged: [{ server: 'srv0', root: '/home/deploy/pages', exists: false, hosts: ['a.example.com', 'b.example.com'] }], unmanaged_hosts: 2, detail: 'error pages for 2 hostnames are served from an unmanaged directory' };
  api.get.mockResolvedValue({ ...caddy, error_pages: pages });
  api.post.mockResolvedValue({ ...caddy, error_pages: { ...pages, managed: 3, unmanaged: [], unmanaged_hosts: 0, detail: undefined } });
  render(<ServerIngress serverID="edge" />);
  expect(await screen.findByText('Error pages for 2 hostnames are served from an unmanaged directory.')).toBeInTheDocument();
  expect(screen.getByText('/etc/lidza/pages')).toBeInTheDocument();
  expect(screen.getByText(/readable by caddy/)).toBeInTheDocument();
  expect(screen.getByText(/a.example.com, b.example.com/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Use Līdza error pages' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('control/servers/edge/host/ingress/error-pages'));
  await waitFor(() => expect(screen.queryByRole('button', { name: 'Use Līdza error pages' })).not.toBeInTheDocument());
  expect(screen.getByText(/3 use the managed pages/)).toBeInTheDocument();
});
