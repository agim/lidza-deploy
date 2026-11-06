import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { normalizeLocation } from '../lib/router';
import { ApplicationsPage } from './ApplicationsPage';
import { AppSettingsPage } from './AppSettingsPage';
import { NewApplicationPage } from './NewApplicationPage';

const workspace = vi.hoisted(() => ({
  apps: [
    { id: 'site', repository: 'org/site', branch: 'main', domain: 'site.example.com', server_id: 'a', runtime: 'astro' },
    { id: 'shop', repository: 'org/shop', branch: 'main', domain: 'shop.example.com', server_id: 'b', runtime: 'rails' },
  ],
  servers: [{ id: 'a', name: 'Server A' }, { id: 'b', name: 'Server B' }],
}));
vi.mock('../state/WorkspaceContext', () => ({ useWorkspace: () => ({ ...workspace, deployments: [], latestDeployment: () => undefined, refresh: vi.fn(), status: {}, loading: false }) }));
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

const open = (url: string) => { history.replaceState(null, '', url); normalizeLocation(); };
const here = () => location.pathname + location.search;
const cards = () => [...document.querySelectorAll('[data-app-card]')].map(card => card.getAttribute('data-app-card'));
const respond = (body: unknown, status = 200) => ({ ok: status < 400, status, json: async () => body });

describe('application filters in the URL', () => {
  it('opens a shared filtered link and reflects changes without new history entries', () => {
    open('/console/applications?q=shop');
    render(<ApplicationsPage />);
    expect(screen.getByRole('searchbox')).toHaveValue('shop');
    expect(cards()).toEqual(['shop']);
    const before = history.length;
    fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'site' } });
    expect(here()).toBe('/console/applications?q=site');
    expect(cards()).toEqual(['site']);
    fireEvent.change(screen.getByRole('searchbox'), { target: { value: '' } });
    expect(here()).toBe('/console/applications');
    expect(cards()).toEqual(['site', 'shop']);
    expect(history.length).toBe(before);
  });

  it('reads status, runtime and server choices and drops invalid ones', () => {
    open('/console/applications?status=failed&runtime=rails&server=b');
    render(<ApplicationsPage />);
    expect(screen.getByRole('combobox', { name: 'Filter by application status' })).toHaveTextContent('Failed');
    expect(screen.getByRole('combobox', { name: 'Filter by runtime' })).toHaveTextContent(/Rails/);
    expect(screen.getByRole('combobox', { name: 'Filter by server' })).toHaveTextContent('Server B');
    cleanup();
    open('/console/applications?status=bogus&runtime=cobol');
    render(<ApplicationsPage />);
    expect(screen.getByRole('combobox', { name: 'Filter by application status' })).toHaveTextContent('All statuses');
    expect(here()).toBe('/console/applications');
  });

  it('keeps the open move panel in the URL', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => respond({ error: 'no move' }, 404)));
    open('/console/applications?move=shop');
    const { container } = render(<ApplicationsPage />);
    const panel = (id: string) => container.querySelector(`[data-app-card="${id}"] details.move-flow`) as HTMLDetailsElement;
    expect(panel('shop').open).toBe(true);
    expect(panel('site').open).toBe(false);
    await waitFor(() => expect(fetch).toHaveBeenCalledWith('/api/control/apps/shop/move', expect.anything()));
    act(() => { panel('shop').open = false; fireEvent(panel('shop'), new Event('toggle')); });
    expect(here()).toBe('/console/applications');
    act(() => { panel('site').open = true; fireEvent(panel('site'), new Event('toggle')); });
    expect(here()).toBe('/console/applications?move=site');
  });
});

describe('settings tab in the URL', () => {
  it('opens the tab named by the path and replaces the entry on change', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => respond({ id: 'portal', server_id: 'a', repository: 'acme/portal', branch: 'main', domain: 'portal.example.com', environment: [{ key: 'TITLE', kind: 'setting', value: 'Portal' }] })));
    open('/console/applications/portal/settings/environment');
    render(<AppSettingsPage appID="portal" />);
    expect(await screen.findByRole('tab', { name: 'Environment' })).toHaveAttribute('aria-selected', 'true');
    const before = history.length;
    fireEvent.mouseDown(screen.getByRole('tab', { name: 'General' }), { button: 0, ctrlKey: false });
    expect(here()).toBe('/console/applications/portal/settings/general');
    expect(screen.getByRole('tab', { name: 'General' })).toHaveAttribute('aria-selected', 'true');
    expect(history.length).toBe(before);
  });
});

describe('new application wizard steps', () => {
  const stubAPI = () => vi.stubGlobal('fetch', vi.fn(async (url: string) => url.startsWith('/api/control/github/repos') ? respond([]) : respond({ runtimes: {} })));
  const current = () => screen.getAllByRole('button').find(button => button.getAttribute('aria-current') === 'step')?.textContent || '';
  const next = () => fireEvent.click(screen.getByRole('button', { name: /Continue/ }));

  it('gives each step a history entry so browser Back returns to the previous step', async () => {
    stubAPI();
    open('/console/new-application');
    render(<NewApplicationPage />);
    expect(current()).toContain('Name');
    fireEvent.change(screen.getByLabelText(/Application name/), { target: { value: 'suite' } });
    const before = history.length;
    next();
    expect(here()).toBe('/console/new-application?step=repository');
    expect(current()).toContain('Repository');
    expect(history.length).toBe(before + 1);
    act(() => history.back());
    await waitFor(() => expect(current()).toContain('Name'));
    expect(here()).toBe('/console/new-application');
    expect(screen.getByLabelText(/Application name/)).toHaveValue('suite');
    act(() => history.forward());
    await waitFor(() => expect(current()).toContain('Repository'));
  });

  it('matches the browser Back from its own Back button', async () => {
    stubAPI();
    open('/console/new-application?server=b');
    render(<NewApplicationPage serverID="b" />);
    fireEvent.change(screen.getByLabelText(/Application name/), { target: { value: 'suite' } });
    next();
    expect(here()).toBe('/console/new-application?server=b&step=repository');
    const back = vi.spyOn(history, 'back');
    const length = history.length;
    fireEvent.click(screen.getByRole('button', { name: 'Back' }));
    expect(back).toHaveBeenCalledOnce();
    await waitFor(() => expect(current()).toContain('Name'));
    expect(here()).toBe('/console/new-application?server=b');
    expect(history.length).toBe(length);
  });

  it('starts a fresh draft at the first step after a reload', () => {
    stubAPI();
    open('/console/new-application?step=review');
    render(<NewApplicationPage />);
    expect(current()).toContain('Name');
    expect(here()).toBe('/console/new-application');
  });
});
