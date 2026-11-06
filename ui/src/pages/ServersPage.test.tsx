import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ServersPage } from './ServersPage';
import { ServerResources } from '../components/ServerResources';

vi.mock('../state/WorkspaceContext', () => ({ useWorkspace: () => ({
  servers: [{ id: 'local', name: 'This server', url: 'http://127.0.0.1:9090' }],
  apps: [{ id: 'portal', server_id: 'local', domain: 'portal.example.com' }],
}) }));

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); });

// Server cards also ask each agent for its host update summary; answer those
// separately so the health request sequence below stays exact.
const withHostUpdates = (mock: (...args: unknown[]) => unknown) => (url: unknown, init?: unknown) =>
  String(url).includes('/host/updates') ? Promise.resolve({ ok: true, status: 200, json: async () => ({ supported: false }) }) : mock(url, init);

const health = {
  server_id: 'local', checked: '2026-10-15T13:00:00Z', cpus: 4, cpu_percent: 16.3, load1: .11,
  memory_total: 8 * 1024 ** 3, memory_available: 6 * 1024 ** 3,
  swap_total: 4 * 1024 ** 3, swap_free: 3 * 1024 ** 3,
  disks: [{ path: '/var/lib/docker', total: 100 * 1024 ** 3, available: 30 * 1024 ** 3, used_percent: 70 }],
  database_bytes: { portal: 20 * 1024 ** 2 },
};

describe('ServersPage resources', () => {
  it('restores host metrics and retains the same server card with stale values during outages', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce({ ok: true, status: 200, json: async () => ({ servers: [health], unavailable_servers: [] }) })
      .mockRejectedValueOnce(new Error('Agent connection failed'))
      .mockResolvedValueOnce({ ok: true, status: 200, json: async () => ({ servers: [{ ...health, cpu_percent: 0 }], unavailable_servers: [] }) });
    vi.stubGlobal('fetch', withHostUpdates(fetchMock));
    const { container } = render(<ServersPage />);
    expect(await screen.findByText('16.3%')).toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledWith('/api/control/server-health', expect.any(Object));
    expect(screen.getByText('2.0 GiB used')).toBeInTheDocument();
    expect(screen.getByText('70.0% used')).toBeInTheDocument();
    expect(screen.getByText('1.0 GiB used / 4.0 GiB')).toBeInTheDocument();
    expect(screen.getByText('20.0 MiB')).toBeInTheDocument();
    const card = container.querySelector('[data-server-card="local"]');
    fireEvent.click(screen.getByRole('button', { name: 'Refresh resources' }));
    expect(await screen.findByText(/These measurements are stale/)).toBeInTheDocument();
    expect(screen.getByText('16.3%')).toBeInTheDocument();
    expect(container.querySelector('[data-server-card="local"]')).toBe(card);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Refresh resources' })).toBeEnabled());
    fireEvent.click(screen.getByRole('button', { name: 'Refresh resources' }));
    expect(await screen.findByText('0.0%')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText(/These measurements are stale/)).not.toBeInTheDocument());
  });

  it('labels unavailable and unsampled measurements without inventing zero usage', () => {
    render(<ServerResources health={{ server_id: 'local', cpu_percent: null }} loading={false} />);
    expect(screen.getByText('Sampling…')).toBeInTheDocument();
    expect(screen.queryByText('0.0%')).not.toBeInTheDocument();
    expect(screen.queryByText('0 B used')).not.toBeInTheDocument();
    expect(screen.getByText('Disk measurements unavailable.')).toBeInTheDocument();
  });

  it('refreshes host measurements automatically without overlapping requests', async () => {
    vi.useFakeTimers();
    let finish!: (value: unknown) => void;
    const fetchMock = vi.fn().mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }))
      .mockResolvedValue({ ok: true, status: 200, json: async () => ({ servers: [health] }) });
    vi.stubGlobal('fetch', withHostUpdates(fetchMock));
    render(<ServersPage />);
    const { act } = await import('@testing-library/react');
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    await act(async () => { finish({ ok: true, status: 200, json: async () => ({ servers: [health] }) }); });
    await act(async () => { await vi.advanceTimersByTimeAsync(15_000); });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(screen.getByText('16.3%')).toBeInTheDocument();
  });
});

describe('ServerResources warnings', () => {
  it('lists host toolchain warnings on the server card', () => {
    const warning = 'Ruby 3.4.4: Bundler 2.7.1, 2.6.9 installed beside RubyGems 4.0.23\'s default Bundler 4.0.23; older Bundler versions print constant redefinition warnings on this RubyGems (remove them with the server\'s Remove unused Bundler versions action)';
    render(<ServerResources health={{ ...health, warnings: [warning] }} loading={false} />);
    expect(screen.getByRole('status', { name: 'Server warnings' })).toHaveTextContent(warning);
    expect(screen.queryByRole('group', { name: 'Server notes' })).toBeNull();
  });

  it('shows no warning block when the host has none', () => {
    render(<ServerResources health={health} loading={false} />);
    expect(screen.queryByRole('status', { name: 'Server warnings' })).toBeNull();
  });

  it('keeps informational notes muted and collapsed, outside the warning block', () => {
    const note = "Rails releases on Ruby 3.4.4 use the host's Bundler 4.0.23; 6 applications pin older versions in their Gemfile.lock";
    const { container } = render(<ServerResources health={{ ...health, notes: [note] }} loading={false} />);
    expect(screen.queryByRole('status', { name: 'Server warnings' })).toBeNull();
    const notes = container.querySelector('details.host-notes') as HTMLDetailsElement;
    expect(notes).not.toBeNull();
    expect(notes.open).toBe(false);
    expect(notes).toHaveTextContent('1 note');
    expect(notes.querySelector('p.muted-copy')).toHaveTextContent(note);
  });
});

describe('ServerResources database sizes', () => {
  it('labels retained databases with their last backup instead of an error', () => {
    render(<ServerResources health={{
      ...health,
      database_bytes: { portal: 20 * 1024 ** 2, moved: 4096 },
      retained_databases: { gone: { backup_bytes: 1.2 * 1024 ** 3, backup_created: '2026-11-02T00:00:00Z' }, moved: {}, empty: {} },
    }} loading={false} />);
    expect(screen.getByText('20.0 MiB')).toBeInTheDocument();
    expect(screen.getByText('retained · last backup 1.2 GiB')).toBeInTheDocument();
    expect(screen.getByText('retained · 4.0 KiB')).toBeInTheDocument();
    expect(screen.getByText('retained · no backups')).toBeInTheDocument();
    expect(screen.getByText('4 measured')).toBeInTheDocument();
    expect(screen.queryByText(/database size unavailable/)).toBeNull();
  });
});
