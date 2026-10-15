import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ServersPage } from './ServersPage';
import { ServerResources } from '../components/ServerResources';

vi.mock('../state/WorkspaceContext', () => ({ useWorkspace: () => ({
  servers: [{ id: 'local', name: 'This server', url: 'http://127.0.0.1:9090' }],
  apps: [{ id: 'portal', server_id: 'local', domain: 'portal.example.com' }],
}) }));

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); });

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
    vi.stubGlobal('fetch', fetchMock);
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
    vi.stubGlobal('fetch', fetchMock);
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
