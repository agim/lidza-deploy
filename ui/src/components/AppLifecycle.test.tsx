import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Application } from '../lib/types';

const api = vi.hoisted(() => ({ get: vi.fn(), del: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn() }));
vi.mock('../lib/api', () => api);
const state = vi.hoisted(() => ({ apps: [] as Application[], refresh: vi.fn() }));
vi.mock('../state/WorkspaceContext', () => ({
  useWorkspace: () => ({ apps: state.apps, servers: [{ id: 'host', name: 'Host' }], deployments: [], latestDeployment: () => undefined, refresh: state.refresh }),
}));

import { ApplicationDetailPage } from '../pages/ApplicationDetailPage';
import { AppDangerZone } from './AppLifecycle';

afterEach(() => { cleanup(); vi.clearAllMocks(); state.apps = []; });
beforeEach(() => { api.get.mockResolvedValue(undefined); });

const base = { repository: 'acme/app', branch: 'main', server_id: 'host' };
const runtimes: Record<string, Application> = {
  container: { ...base, id: 'api', domain: 'api.example.com', current: { commit: 'abc1234' } },
  stack: { ...base, id: 'stack', domain: 'stack.example.com', runtime_profile: 'full', current: { commit: 'abc1234', stack: {} } },
  created: { ...base, id: 'shop', domain: 'shop.example.com', runtime: 'go', native: { id: 'shop', name: 'shop', services: ['lidza-app-shop-web.service'] }, management_capabilities: { deploy: true, remove: true, lifecycle: true } },
  adopted: { ...base, id: 'rails-ledger', domain: 'ledger.example.com', runtime: 'rails', native: { id: 'rails-ledger', name: 'Ledger', services: ['ledger-server.service'] }, management_capabilities: { deploy: true, remove: true, lifecycle: true, adopted: true } },
  static: { ...base, id: 'site', domain: 'site.example.com', runtime: 'astro', native: { id: 'site', name: 'site', services: [], document_root: 'public' }, management_capabilities: { deploy: true, remove: true, lifecycle: true } },
};

// The application page's header holds the actions menu.
async function openMenu() {
  fireEvent.keyDown(screen.getByRole('button', { name: 'Actions' }), { key: 'Enter' });
  return await screen.findByRole('menu');
}
const page = (id: string) => render(<ApplicationDetailPage appID={id} tab="overview" />);

it('offers Restart, Stop and Delete in the Application group for every runtime, in the same order', async () => {
  state.apps = Object.values(runtimes);
  for (const app of Object.values(runtimes)) {
    const view = page(app.id);
    const menu = await openMenu();
    expect(within(menu).getByText('Application')).toBeInTheDocument();
    const labels = [...menu.querySelectorAll('[data-lifecycle]')].map(item => item.textContent);
    expect(labels).toEqual(['Restart', 'Stop', 'Delete']);
    view.unmount();
  }
});

it('stops after a confirmation that says what happens, and starts a stopped application from its header', async () => {
  api.post.mockResolvedValue({ status: 'stopped' });
  state.apps = [runtimes.adopted];
  page('rails-ledger');
  const menu = await openMenu();
  fireEvent.click(within(menu).getByText('Stop'));
  const dialog = await screen.findByRole('alertdialog');
  expect(within(dialog).getByText(/visitors see a maintenance page; data is kept/i)).toBeInTheDocument();
  fireEvent.click(within(dialog).getByRole('button', { name: 'Stop application' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('control/apps/rails-ledger/stop'));
  cleanup();

  state.apps = [{ ...runtimes.adopted, stopped: true }];
  const { container } = page('rails-ledger');
  const header = container.querySelector('.page-header') as HTMLElement;
  expect(within(header).getByText('stopped')).toBeInTheDocument();
  expect(within(header).queryByRole('button', { name: 'Deploy' })).not.toBeInTheDocument();
  fireEvent.click(within(header).getByRole('button', { name: 'Start' }));
  await waitFor(() => expect(api.post).toHaveBeenCalledWith('control/apps/rails-ledger/start'));
  const stoppedMenu = await openMenu();
  expect([...stoppedMenu.querySelectorAll('[data-lifecycle]')].map(item => item.textContent)).toEqual(['Delete']);
  expect(within(stoppedMenu).getByRole('menuitem', { name: /Start/ })).toBeInTheDocument();
  expect(within(stoppedMenu).getByRole('menuitem', { name: /Restart/ })).toHaveAttribute('aria-disabled', 'true');
});

it('disables the actions with the reason while a move holds the application or the agent cannot', async () => {
  state.apps = [
    { ...runtimes.container, move: { id: 'm1', state: 'running', source_server: 'host', target_server: 'other' } },
    { ...runtimes.adopted, management_capabilities: { deploy: true, remove: true, lifecycle: false, lifecycle_reason: 'Service control is not enabled on this server.' } },
  ];
  const first = page('api');
  let menu = await openMenu();
  for (const name of ['Restart', 'Stop', 'Delete']) {
    const item = within(menu).getByRole('menuitem', { name: new RegExp(`^${name}`) });
    expect(item).toHaveAttribute('aria-disabled', 'true');
    expect(item).toHaveAttribute('title', 'This application is moving; finish or abort the move first.');
  }
  first.unmount();
  page('rails-ledger');
  menu = await openMenu();
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

it('shows a short service-control reason on the disabled items and links to the server setting', async () => {
  const reason = 'Service control is off for this server.';
  state.apps = [{ ...runtimes.adopted, management_capabilities: { deploy: true, remove: true, lifecycle: false, lifecycle_reason: reason, lifecycle_setting: 'service_control', adopted: true } }];
  page('rails-ledger');
  const menu = await openMenu();
  for (const name of ['Restart', 'Stop']) {
    const item = within(menu).getByRole('menuitem', { name: new RegExp(`^${name}`) });
    expect(item).toHaveAttribute('aria-disabled', 'true');
    expect(item).toHaveAttribute('title', reason);
    // One short secondary line, no paragraph.
    const line = item.querySelector('.menu-reason');
    expect(line?.textContent).toBe(reason);
    expect(line?.textContent?.length).toBeLessThan(60);
  }
  const link = within(menu).getByRole('menuitem', { name: 'Service control settings' });
  expect(link).toHaveAttribute('href', '/console/servers/host/agent');
  expect(link).not.toHaveAttribute('aria-disabled');
  cleanup();

  // Another blocker (a move) names itself and offers no settings link.
  state.apps = [{ ...runtimes.adopted, move: { id: 'm1', state: 'running', source_server: 'host', target_server: 'other' }, management_capabilities: { deploy: true, remove: true, lifecycle: false, lifecycle_reason: reason, lifecycle_setting: 'service_control' } }];
  page('rails-ledger');
  expect(within(await openMenu()).queryByRole('menuitem', { name: 'Service control settings' })).not.toBeInTheDocument();
  cleanup();

  render(<AppDangerZone app={{ ...runtimes.adopted, management_capabilities: { deploy: true, remove: true, lifecycle: false, lifecycle_reason: reason, lifecycle_setting: 'service_control' } }} onChanged={() => {}} onDeleted={() => {}} />);
  const note = screen.getAllByRole('note')[0];
  expect(note).toHaveTextContent(reason);
  expect(within(note).getByRole('link', { name: /Agent settings/ })).toHaveAttribute('href', '/console/servers/host/agent');
});
