import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ConnectServerPage, ServerDetailPage } from './ServerDetailPages';
import { useRoute } from '../lib/router';

vi.mock('../state/WorkspaceContext', () => ({ useWorkspace: () => ({
  servers: [{ id: 'local', name: 'Production', url: 'http://127.0.0.1:9090' }],
  apps: [{ id: 'portal', server_id: 'local', domain: 'portal.example.com' }],
  latestDeployment: () => undefined,
  refresh: async () => {},
}) }));
vi.mock('../components/ToastProvider', () => ({ useToast: () => () => {} }));
vi.mock('../lib/api', () => ({ get: vi.fn(() => new Promise(() => {})), post: vi.fn(), put: vi.fn(), del: vi.fn(), patch: vi.fn() }));
// Each section stands in for itself, so the test sees which ones are mounted (and load data).
vi.mock('../components/ServerIngress', () => ({ ServerIngress: () => <p>ingress section</p> }));
vi.mock('../components/ServerUpdates', () => ({ ServerUpdates: () => <p>updates section</p>, HostUpdateBadges: () => null }));
vi.mock('../components/ServerTuning', () => ({ ServerTuning: () => <p>tuning section</p> }));
vi.mock('../components/ServerFirewall', () => ({ ServerFirewall: () => <p>firewall section</p> }));
vi.mock('../components/RetainedResources', () => ({ RetainedResources: () => <p>retained section</p> }));
vi.mock('./ServerCleanupPage', () => ({ ServerCleanup: () => <p>cleanup section</p> }));
vi.mock('./ServerConsolePage', () => ({ ServerConsole: () => <p>console section</p> }));

const sections = ['ingress', 'updates', 'tuning', 'firewall', 'cleanup', 'retained', 'console'];
const mounted = () => sections.filter(name => screen.queryByText(`${name} section`));
const open = (path: string) => history.replaceState(null, '', path);
// Radix tabs activate on mouse down with the primary button.
const choose = (name: string) => act(() => { fireEvent.mouseDown(screen.getByRole('tab', { name }), { button: 0 }); });
// The page shows the tab the URL names, as App does.
function Routed() { const route = useRoute(); return <ServerDetailPage serverID="local" tab={route.tab || 'overview'} />; }

beforeEach(() => open('/console/servers/local'));
afterEach(() => { cleanup(); });

describe('server detail tabs', () => {
  it('opens the overview at the bare server path with every server tab', () => {
    const { container } = render(<Routed />);
    expect(container.querySelector('[data-template=detail][data-resource=servers]')).not.toBeNull();
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Production');
    expect(screen.getByRole('tab', { name: 'Overview' })).toHaveAttribute('aria-selected', 'true');
    for (const name of ['Applications', 'Processes', 'Web ingress', 'Disk cleanup', 'Console', 'SSH access', 'Agent']) expect(screen.getByRole('tab', { name })).toHaveAttribute('href');
    expect(mounted()).toEqual([]);
  });

  it('opens the tab named in the path and mounts only that section', () => {
    open('/console/servers/local/tuning');
    render(<Routed />);
    expect(screen.getByRole('tab', { name: 'Tuning' })).toHaveAttribute('aria-selected', 'true');
    expect(mounted()).toEqual(['tuning']);
  });

  it('puts the chosen tab in the URL as a new history entry', () => {
    render(<Routed />);
    const length = history.length;
    choose('Firewall');
    expect(location.pathname).toBe('/console/servers/local/firewall');
    expect(history.length).toBe(length + 1);
    expect(mounted()).toEqual(['firewall']);
    choose('Overview');
    expect(location.pathname).toBe('/console/servers/local');
    expect(mounted()).toEqual([]);
    choose('Console');
    expect(mounted()).toEqual(['console']);
  });

  it('keeps the connection and the Danger zone on the Agent tab', () => {
    open('/console/servers/local/agent');
    render(<Routed />);
    expect(screen.getByRole('button', { name: 'Save connection' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Danger zone' })).toBeInTheDocument();
    // A server with applications cannot be disconnected.
    expect(screen.getByRole('button', { name: 'Disconnect server' })).toBeDisabled();
  });

  it('connects a new server on its create form, without tabs', () => {
    open('/console/servers/new');
    render(<ConnectServerPage />);
    expect(screen.queryByRole('tablist')).not.toBeInTheDocument();
    expect(screen.getByRole('heading', { name: /Connect server/ })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Save connection' })).toBeInTheDocument();
  });
});
