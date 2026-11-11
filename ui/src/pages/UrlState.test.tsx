import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { normalizeLocation, useRoute } from '../lib/router';
import { ApplicationsPage } from './ApplicationsPage';
import { ApplicationDetailPage } from './ApplicationDetailPage';
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
const cards = () => [...document.querySelectorAll('.resource-row .resource-name a')].map(link => link.textContent);
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

  it('opens the Move tab for an earlier move link', () => {
    open('/console/applications?move=shop');
    expect(here()).toBe('/console/applications/shop/move');
  });
});

describe('application tabs in the URL', () => {
  // The page shows the tab the URL names, as App does.
  function Routed() { const route = useRoute(); return <ApplicationDetailPage appID={route.id || ''} tab={route.tab || 'overview'} />; }
  it('opens the tab named by the path and adds a history entry on change', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => respond({ id: 'shop', server_id: 'b', repository: 'org/shop', branch: 'main', domain: 'shop.example.com', environment: [{ key: 'TITLE', kind: 'setting', value: 'Shop' }] })));
    open('/console/applications/shop/settings/environment');
    expect(here()).toBe('/console/applications/shop/environment');
    render(<Routed />);
    expect(screen.getByRole('tab', { name: 'Environment' })).toHaveAttribute('aria-selected', 'true');
    expect(await screen.findByDisplayValue('Shop')).toBeInTheDocument();
    const before = history.length;
    fireEvent.mouseDown(screen.getByRole('tab', { name: 'Domains' }), { button: 0, ctrlKey: false });
    expect(here()).toBe('/console/applications/shop/domains');
    expect(screen.getByRole('tab', { name: 'Domains' })).toHaveAttribute('aria-selected', 'true');
    expect(history.length).toBe(before + 1);
    expect(screen.getByRole('tab', { name: 'Overview' })).toHaveAttribute('href', '/console/applications/shop');
  });
});

describe('new application wizard steps', () => {
  const stubAPI = () => vi.stubGlobal('fetch', vi.fn(async (url: string) => url.startsWith('/api/control/github/repos') ? respond([]) : respond({ runtimes: {} })));
  const current = () => screen.getAllByRole('button').find(button => button.getAttribute('aria-current') === 'step')?.textContent || '';
  const next = () => fireEvent.click(screen.getByRole('button', { name: /Continue/ }));

  it('gives each step a history entry so browser Back returns to the previous step', async () => {
    stubAPI();
    open('/console/applications/new');
    render(<NewApplicationPage />);
    expect(current()).toContain('Name');
    fireEvent.change(screen.getByLabelText(/Application name/), { target: { value: 'suite' } });
    const before = history.length;
    next();
    expect(here()).toBe('/console/applications/new?step=repository');
    expect(current()).toContain('Repository');
    expect(history.length).toBe(before + 1);
    act(() => history.back());
    await waitFor(() => expect(current()).toContain('Name'));
    expect(here()).toBe('/console/applications/new');
    expect(screen.getByLabelText(/Application name/)).toHaveValue('suite');
    act(() => history.forward());
    await waitFor(() => expect(current()).toContain('Repository'));
  });

  it('matches the browser Back from its own Back button', async () => {
    stubAPI();
    open('/console/applications/new?server=b');
    render(<NewApplicationPage serverID="b" />);
    fireEvent.change(screen.getByLabelText(/Application name/), { target: { value: 'suite' } });
    next();
    expect(here()).toBe('/console/applications/new?server=b&step=repository');
    const back = vi.spyOn(history, 'back');
    const length = history.length;
    fireEvent.click(screen.getByRole('button', { name: 'Back' }));
    expect(back).toHaveBeenCalledOnce();
    await waitFor(() => expect(current()).toContain('Name'));
    expect(here()).toBe('/console/applications/new?server=b');
    expect(history.length).toBe(length);
  });

  it('starts a fresh draft at the first step after a reload', () => {
    stubAPI();
    open('/console/applications/new?step=review');
    render(<NewApplicationPage />);
    expect(current()).toContain('Name');
    expect(here()).toBe('/console/applications/new');
  });
});
