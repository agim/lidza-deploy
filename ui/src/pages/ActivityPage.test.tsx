import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ActivityDetailPage, ActivityPage } from './WorkspacePages';

const workspace = vi.hoisted(() => ({ apps: [{ id: 'portal' }] as Array<Record<string, unknown>> }));
vi.mock('../state/WorkspaceContext', () => ({
  useWorkspace: () => workspace,
}));

const nativeApp = { id: 'shop', runtime: 'rails', native: { id: 'shop', name: 'shop', runtime: 'rails', services: ['lidza-app-shop-web.service'] } };

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); workspace.apps = [{ id: 'portal' }]; });

describe('ActivityPage', () => {
  it('shows API timestamps and operational details on the error list', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({
        status: 'ready',
        last_received: '2026-10-14T12:35:00Z',
        errors: [
          {
            id: 'second',
            fingerprint: 'database-timeout',
            source: 'server',
            message: 'Database request timed out',
            stack: 'Error: timeout\n    at saveMessage (mail.go:42)',
            method: 'POST',
            route: '/api/mail',
            requestId: 'req-latest',
            createdAt: '2026-10-14T12:34:56Z',
            container: 'lidza-portal-123',
            release: 'release-123',
            commit: 'abc123',
          },
          {
            id: 'first',
            fingerprint: 'database-timeout',
            source: 'server',
            message: 'Database request timed out',
            requestId: 'req-first',
            createdAt: '2026-10-14T12:30:00Z',
          },
        ],
      }),
    }) as Response));

    const index = render(<ActivityPage />);
    expect(await screen.findByRole('link', { name: 'Database request timed out' })).toHaveAttribute('href', '/console/errors/database-timeout?app=portal');
    expect(screen.getByText('2')).toBeInTheDocument();
    expect(index.container.querySelector('time[datetime="2026-10-14T12:34:56Z"]')).toBeInTheDocument();
    expect(screen.getByText(/\/api\/mail/)).toBeInTheDocument();
    await waitFor(() => expect(fetch).toHaveBeenCalledWith('/api/control/apps/portal/errors', expect.any(Object)));
    index.unmount();

    const overview = render(<ActivityDetailPage appID="portal" id="database-timeout" tab="overview" />);
    expect(await screen.findByText('2 occurrences')).toBeInTheDocument();
    expect(screen.getByText(/saveMessage/)).toBeInTheDocument();
    expect(screen.getByText('lidza-portal-123')).toBeInTheDocument();
    expect(screen.getByText('release-123')).toBeInTheDocument();
    expect(screen.getByText('abc123')).toBeInTheDocument();
    overview.unmount();
    render(<ActivityDetailPage appID="portal" id="database-timeout" tab="occurrences" />);
    expect(await screen.findByText('req-latest')).toBeInTheDocument();
    expect(screen.getByText('req-first')).toBeInTheDocument();
  });

  it('renders error groups for a native application without a change-plan banner', async () => {
    workspace.apps = [nativeApp];
    vi.stubGlobal('fetch', vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({
        status: 'ready',
        errors: [
          { id: 'native-second', fingerprint: 'rails-nil-total', source: 'server', message: "NoMethodError (undefined method 'total' for nil)", stack: "app/models/order.rb:12:in 'total'", method: 'GET', route: '/orders', requestId: 'a1b2c3d4-e5f6', createdAt: '2026-11-05T10:00:05Z', container: 'lidza-app-shop-web.service', release: '20261105100000', commit: 'abc123' },
          { id: 'native-first', fingerprint: 'rails-nil-total', source: 'server', message: "NoMethodError (undefined method 'total' for nil)", createdAt: '2026-11-05T10:00:00Z', container: 'lidza-app-shop-web.service' },
        ],
      }),
    }) as Response));

    render(<ActivityDetailPage appID="shop" id="rails-nil-total" tab="overview" />);

    expect((await screen.findAllByText("NoMethodError (undefined method 'total' for nil)"))[0]).toBeInTheDocument();
    expect(screen.getByText('2 occurrences')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.queryByText(/change plan/i)).not.toBeInTheDocument();
    expect(screen.getByText('Service unit')).toBeInTheDocument();
    expect(screen.getByText('lidza-app-shop-web.service')).toBeInTheDocument();
    expect(screen.getByText('20261105100000')).toBeInTheDocument();
    expect(screen.getByText(/order\.rb:12/)).toBeInTheDocument();
    await waitFor(() => expect(fetch).toHaveBeenCalledWith('/api/control/apps/shop/errors', expect.any(Object)));
  });

  it('renders security events for a native application', async () => {
    workspace.apps = [nativeApp];
    vi.stubGlobal('fetch', vi.fn(async () => ({
      ok: true,
      status: 200,
      json: async () => ({
        status: 'ready',
        errors: [
          { id: 'probe-1', fingerprint: 'probe-env', source: 'server', message: 'Suspected secret-file probe: HTTP 404', method: 'GET', route: '/.env', requestId: 'b2c3d4e5-f6a7', createdAt: '2026-11-05T10:00:01Z', container: 'lidza-app-shop-web.service', security: { category: 'secret-file', status: 404, outcome: 'rejected' } },
        ],
      }),
    }) as Response));

    render(<ActivityPage security appID="shop" />);

    expect(await screen.findByText('secret file')).toBeInTheDocument();
    expect(screen.getAllByText(/\/\.env/)[0]).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    await waitFor(() => expect(fetch).toHaveBeenCalledWith('/api/control/apps/shop/security', expect.any(Object)));
  });
});
