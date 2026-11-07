import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import type { Application } from '../lib/types';

const api = vi.hoisted(() => ({ get: vi.fn(), del: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn() }));
vi.mock('../lib/api', () => api);
const state = vi.hoisted(() => ({ apps: [] as Application[], refresh: vi.fn() }));
vi.mock('../state/WorkspaceContext', () => ({
  useWorkspace: () => ({ apps: state.apps, servers: [{ id: 'host', name: 'Host' }], deployments: [], latestDeployment: () => undefined, refresh: state.refresh }),
}));

import { ApplicationsPage } from '../pages/ApplicationsPage';
import { AppDangerZone } from './AppLifecycle';

afterEach(() => { cleanup(); vi.clearAllMocks(); state.apps = []; });

const base = { repository: 'acme/app', branch: 'main', server_id: 'host' };
const runtimes: Record<string, Application> = {
  container: { ...base, id: 'api', domain: 'api.example.com', current: { commit: 'abc1234' } },
  stack: { ...base, id: 'stack', domain: 'stack.example.com', runtime_profile: 'full', current: { commit: 'abc1234', stack: {} } },
  created: { ...base, id: 'shop', domain: 'shop.example.com', runtime: 'go', native: { id: 'shop', name: 'shop', services: ['lidza-app-shop-web.service'] }, management_capabilities: { deploy: true, remove: true, lifecycle: true } },
  adopted: { ...base, id: 'rails-ledger', domain: 'ledger.example.com', runtime: 'rails', native: { id: 'rails-ledger', name: 'Ledger', services: ['ledger-server.service'] }, management_capabilities: { deploy: true, remove: true, lifecycle: true, adopted: true } },
  static: { ...base, id: 'site', domain: 'site.example.com', runtime: 'astro', native: { id: 'site', name: 'site', services: [], document_root: 'public' }, management_capabilities: { deploy: true, remove: true, lifecycle: true } },
};

async function openMenu(id: string) {
  const card = document.querySelector(`[data-app-card="${id}"]`) as HTMLElement;
  fireEvent.keyDown(within(card).getByRole('button', { name: 'More' }), { key: 'Enter' });
  return await screen.findByRole('menu');
}

it('offers Restart, Stop and Delete in the Application group for every runtime, in the same order', async () => {
  state.apps = Object.values(runtimes);
  render(<ApplicationsPage />);
  for (const app of Object.values(runtimes)) {
    const menu = await openMenu(app.id);
    expect(within(menu).getByText('Application')).toBeInTheDocument();
    const labels = [...menu.querySelectorAll('[data-lifecycle]')].map(item => item.textContent);
    expect(labels).toEqual(['Restart', 'Stop', 'Delete']);
    fireEvent.keyDown(menu, { key: 'Escape' });
    await waitFor(() => expect(screen.queryByRole('menu')).not.toBeInTheDocument());
  }
});

it('stops after a confirmation that says what happens, and starts a stopped application from its card', async () => {
  api.post.mockResolvedValue({ status: 'stopped' });
  state.apps = [runtimes.adopted];
  render(<ApplicationsPage />);
  const menu = await openMenu('rails-ledger');
  fireEvent.click(within(menu).getByText('Stop'));
  const dialog = await screen.findByRole('alertdialog');
  expect(within(dialog).getByText(/visitors see a maintenance page; data is kept/i)).toBeInTheDocument();
  fireEvent.click(within(dialog).getByRole('button', { name: 'Stop application' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('control/apps/rails-ledger/stop'));
  cleanup();

  state.apps = [{ ...runtimes.adopted, stopped: true }];
  const { container } = render(<ApplicationsPage />);
  const card = container.querySelector('[data-app-card="rails-ledger"]') as HTMLElement;
  expect(within(card).getByText('stopped')).toBeInTheDocument();
  expect(within(card).queryByRole('button', { name: 'Deploy' })).not.toBeInTheDocument();
  fireEvent.click(within(card).getByRole('button', { name: 'Start' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('control/apps/rails-ledger/start'));
  const stoppedMenu = await openMenu('rails-ledger');
  expect([...stoppedMenu.querySelectorAll('[data-lifecycle]')].map(item => item.textContent)).toEqual(['Delete']);
  expect(within(stoppedMenu).getByRole('menuitem', { name: /Start/ })).toBeInTheDocument();
  expect(within(stoppedMenu).getByRole('menuitem', { name: /Restart/ })).toHaveAttribute('aria-disabled', 'true');
});

it('disables the actions with the reason while a move holds the application or the agent cannot', async () => {
  state.apps = [
    { ...runtimes.container, move: { id: 'm1', state: 'running', source_server: 'host', target_server: 'other' } },
    { ...runtimes.adopted, management_capabilities: { deploy: true, remove: true, lifecycle: false, lifecycle_reason: 'Service control is not enabled on this server.' } },
  ];
  render(<ApplicationsPage />);
  let menu = await openMenu('api');
  for (const name of ['Restart', 'Stop', 'Delete']) {
    const item = within(menu).getByRole('menuitem', { name: new RegExp(`^${name}`) });
    expect(item).toHaveAttribute('aria-disabled', 'true');
    expect(item).toHaveAttribute('title', 'This application is moving; finish or abort the move first.');
  }
  fireEvent.keyDown(menu, { key: 'Escape' });
  await waitFor(() => expect(screen.queryByRole('menu')).not.toBeInTheDocument());
  menu = await openMenu('rails-ledger');
  expect(within(menu).getByRole('menuitem', { name: /^Stop/ })).toHaveAttribute('title', 'Service control is not enabled on this server.');
  expect(menu.querySelector('[data-lifecycle="delete"]')).toBeInTheDocument();
});

it('puts Stop and Delete in the Danger zone, with the adopted application inventory', async () => {
  api.post.mockResolvedValue({ status: 'stopped' });
  const changed = vi.fn();
  render(<AppDangerZone app={runtimes.adopted} onChanged={changed} onDeleted={() => {}} />);
  const zone = document.querySelector('[data-danger-zone="rails-ledger"]') as HTMLElement;
  expect(within(zone).getByText('Danger zone')).toBeInTheDocument();
  fireEvent.click(within(zone).getByRole('button', { name: 'Stop application' }));
  const dialog = await screen.findByRole('alertdialog');
  fireEvent.click(within(dialog).getByRole('button', { name: 'Stop application' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('control/apps/rails-ledger/stop'));
  await waitFor(() => expect(changed).toHaveBeenCalled());
  await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument(), { timeout: 4000 });
  fireEvent.click(within(zone).getByRole('button', { name: 'Delete application' }));
  const deletion = await screen.findByRole('alertdialog');
  expect(within(deletion).getByText(/shared deployment account, its home and other applications stay as they are/)).toBeInTheDocument();
  expect(within(deletion).getByText(/a shared one is kept/)).toBeInTheDocument();
  cleanup();
  render(<AppDangerZone app={{ ...runtimes.container, stopped: true }} onChanged={changed} onDeleted={() => {}} />);
  fireEvent.click(screen.getByRole('button', { name: 'Start application' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('control/apps/api/start'));
});
