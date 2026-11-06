import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { ContainersPage, LogPage } from './ServerDetailPages';
import { ApplicationsPage } from './ApplicationsPage';

const shop = { id: 'shop', runtime: 'go', repository: 'acme/shop', branch: 'main', domain: 'shop.example.com', server_id: 'host', native: { id: 'shop', name: 'shop', services: ['lidza-app-shop-web.service', 'lidza-app-shop-jobs.service'] }, management_capabilities: { deploy: true, rollback: true, remove: true } };
vi.mock('../state/WorkspaceContext', () => ({ useWorkspace: () => ({ apps: [shop], servers: [{ id: 'host', name: 'Host' }], deployments: [], latestDeployment: () => undefined, refresh: vi.fn() }) }));
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const reply = (data: unknown) => ({ ok: true, status: 200, json: async () => data });

it('lists native services with their usage on the server process page and restarts them through service control', async () => {
  const fetch = vi.fn((url: string, init?: RequestInit) => Promise.resolve(reply(init?.method === 'POST' ? { status: 'requested' } : { checked_at: '2026-11-05T10:00:00Z', containers: [
    { name: 'lidza-app-shop-web.service', unit: 'lidza-app-shop-web.service', app_id: 'shop', role: 'Service', runtime: 'native', state: 'running', cpu_percent: 3.5, memory_bytes: 52428800, pids: '4', restart_count: 0, restartable: true, since: '2026-11-05T09:00:00Z' },
    { name: 'lidza-app-shop-jobs.service', unit: 'lidza-app-shop-jobs.service', app_id: 'shop', role: 'Service · preserved', runtime: 'native', state: 'inactive', restart_count: 0, restartable: true },
  ] })));
  vi.stubGlobal('fetch', fetch);
  const { container } = render(<ContainersPage serverID="host" />);
  await screen.findByText('lidza-app-shop-web.service');
  const group = container.querySelector('[data-app-containers="shop"]') as HTMLElement;
  expect(within(group).getByText('2 services · shop.example.com')).toBeInTheDocument();
  expect(within(group).getByText('3.5%')).toBeInTheDocument();
  expect(within(group).getByText('50.0 MiB')).toBeInTheDocument();
  expect(within(group).getByText(/Service · preserved/)).toBeInTheDocument();
  expect(within(group).getAllByRole('button', { name: /Restart/ })).toHaveLength(2);
  expect(group.textContent).not.toMatch(/container/i);
  fireEvent.click(within(group).getAllByRole('button', { name: /Restart/ })[0]);
  expect(await screen.findByText(/systemd stops and starts this service/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Restart service' }));
  await vi.waitFor(() => expect(fetch.mock.calls.some(([url, init]) => String(url).includes('control/native-apps/shop/services/lidza-app-shop-web.service/restart') && String(init?.body).includes('"confirm":"shop"'))).toBe(true));
  expect(fetch.mock.calls.some(([url]) => String(url).includes('containers/restart'))).toBe(false);
});

it('shows a native application runtime log and filters it by process', async () => {
  const fetch = vi.fn((url: string) => Promise.resolve(reply({ logs: url.includes('unit=') ? 'jobs output' : 'all output', units: shop.native.services })));
  vi.stubGlobal('fetch', fetch);
  render(<LogPage kind="runtime-logs" id="shop" />);
  expect(await screen.findByText('all output')).toBeInTheDocument();
  fireEvent.change(screen.getByRole('combobox', { name: 'Process' }), { target: { value: 'lidza-app-shop-jobs.service' } });
  expect(await screen.findByText('jobs output')).toBeInTheDocument();
  expect(fetch.mock.calls.some(([url]) => String(url).includes('logs?unit=lidza-app-shop-jobs.service'))).toBe(true);
});

it('offers native processes, data and removal without container operations', async () => {
  render(<ApplicationsPage />);
  fireEvent.keyDown(screen.getByRole('button', { name: 'More' }), { key: 'Enter' });
  expect(await screen.findByRole('menuitem', { name: 'Processes' })).toBeInTheDocument();
  expect(screen.getByRole('menuitem', { name: 'Data & storage' })).toBeInTheDocument();
  expect(screen.getByText('Remove application')).toBeInTheDocument();
  for (const name of ['Reload', 'Stop application', 'Cache', 'Database & backups']) expect(screen.queryByRole('menuitem', { name })).not.toBeInTheDocument();
  fireEvent.click(screen.getByText('Remove application'));
  expect(await screen.findByText(/Its services, account and web route are removed/)).toBeInTheDocument();
  expect(screen.getByText(/kept as a retained resource/)).toBeInTheDocument();
  expect(document.body.textContent).not.toMatch(/containers will be removed/);
});
