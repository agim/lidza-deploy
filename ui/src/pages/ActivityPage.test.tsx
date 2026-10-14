import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ActivityPage } from './WorkspacePages';

vi.mock('../state/WorkspaceContext', () => ({
  useWorkspace: () => ({ apps: [{ id: 'portal' }] }),
}));

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

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

    const { container } = render(<ActivityPage />);

    expect((await screen.findAllByText('Database request timed out'))[0]).toBeInTheDocument();
    expect(screen.getByText('2 occurrences')).toBeInTheDocument();
    expect(container.querySelector('time[datetime="2026-10-14T12:34:56Z"]')).toBeInTheDocument();
    expect(screen.getByText('/api/mail')).toBeInTheDocument();

    const detailsToggle = screen.getByText('View details');
    fireEvent.click(detailsToggle);
    expect(detailsToggle.closest('details')).toHaveAttribute('open');
    expect(screen.getByText(/saveMessage/)).toBeInTheDocument();
    expect(screen.getByText('lidza-portal-123')).toBeInTheDocument();
    expect(screen.getByText('release-123')).toBeInTheDocument();
    expect(screen.getByText('abc123')).toBeInTheDocument();
    expect(screen.getByText('req-latest')).toBeInTheDocument();
    expect(screen.getByText('req-first')).toBeInTheDocument();
    await waitFor(() => expect(fetch).toHaveBeenCalledWith('/api/control/apps/portal/errors', expect.any(Object)));
  });
});
