import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { BundlerCleanupCard, CleanupPreviewCard } from './ServerHygiene';
import { ServerResources } from './ServerResources';

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

const json = (body: unknown, status = 200) => Promise.resolve({ ok: status < 300, status, json: async () => body });

describe('cleanup dry run', () => {
  it('lists what the next cleanup frees', async () => {
    const fetchMock = vi.fn(() => json({ policy: {}, running: false, preview: { checked: '2026-11-05T10:00:00Z', bytes: 3 * 1024 ** 3, items: [
      { kind: 'image', name: 'lidza/shop:d1', bytes: 2 * 1024 ** 3, reason: "older than shop's current and previous release" },
      { kind: 'move-temp', name: 'move-clickhouse/exports/shop/.clickhouse-export-1', bytes: 1024 ** 3 },
    ] } }));
    vi.stubGlobal('fetch', fetchMock);
    render(<CleanupPreviewCard serverID="local" />);
    fireEvent.click(screen.getByRole('button', { name: /Check now/ }));
    const list = await screen.findByRole('list', { name: 'Cleanup preview' });
    expect(within(list).getByText('lidza/shop:d1')).toBeInTheDocument();
    expect(within(list).getByText('Move transfer file')).toBeInTheDocument();
    expect(screen.getByText(/can be freed/)).toHaveTextContent('3');
    expect(fetchMock).toHaveBeenCalledWith('/api/control/servers/local/maintenance?preview=1', expect.any(Object));
  });
});

describe('Bundler removal', () => {
  const plan = { groups: [{ user: 'deploy', ruby: '3.3.0', rubygems: '3.6.9', default_bundler: '2.6.9', apps: ['shop'], remove: ['2.3.5', '2.1.4'], keep: [{ version: '2.4.10', reason: 'release 20251201000000 of shop runs with it' }] }] };

  it('confirms the exact versions, then posts them', async () => {
    const fetchMock = vi.fn((url: unknown, init?: { method?: string }) => {
      if (init?.method === 'POST') return json({ groups: [], job: { status: 'running', started: '2026-11-05T10:00:00Z' } }, 202);
      return json(plan);
    });
    vi.stubGlobal('fetch', fetchMock);
    render(<BundlerCleanupCard serverID="local" />);
    expect(await screen.findByText('2.3.5, 2.1.4')).toBeInTheDocument();
    expect(screen.getByText(/Kept 2.4.10/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /Remove unused Bundler versions/ }));
    const dialog = await screen.findByRole('alertdialog');
    expect(dialog).toHaveTextContent('Ruby 3.3.0 (deploy): Bundler 2.3.5, 2.1.4');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Remove versions' }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/api/control/servers/local/host/ruby/bundlers/remove', expect.objectContaining({ method: 'POST' })));
    const post = fetchMock.mock.calls.find(([, init]) => (init as { method?: string } | undefined)?.method === 'POST')!;
    expect(JSON.parse(String((post[1] as { body: string }).body))).toEqual({ remove: [{ user: 'deploy', ruby: '3.3.0', version: '2.3.5' }, { user: 'deploy', ruby: '3.3.0', version: '2.1.4' }] });
  });

  it('stays hidden when no Ruby has stale versions', async () => {
    const fetchMock = vi.fn(() => json({ groups: [] }));
    vi.stubGlobal('fetch', fetchMock);
    const { container } = render(<BundlerCleanupCard serverID="local" />);
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });
});

describe('server card disk state', () => {
  it('marks a low disk and lists ClickHouse system log sizes', () => {
    render(<ServerResources loading={false} health={{ server_id: 'local', checked: '2026-11-05T10:00:00Z', disk_low: true, disks: [{ path: '/', total: 100, available: 10, used_percent: 90 }], clickhouse_system_logs: { shop: 5 * 1024 ** 3 } }} />);
    expect(screen.getByText('Disk low')).toBeInTheDocument();
    expect(screen.getByText('shop ClickHouse system logs')).toBeInTheDocument();
  });
});
