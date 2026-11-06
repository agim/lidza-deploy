import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ActivityDrawer } from './ActivityDrawer';
import { CLOSED_POLL_MS, OPEN_POLL_MS, progressLabel } from '../lib/activity';

const now = Date.now();
const iso = (minutesAgo: number) => new Date(now - minutesAgo * 60_000).toISOString();
const feed = {
  items: [
    { id: 'move:m1', kind: 'move', title: 'Moving analytics to media-01', server_id: 'media', server_name: 'media-01', app_id: 'analytics', state: 'running', message: 'Copy data', started: iso(12), updated: iso(0), progress: { done: 3, total: 9, unit: 'steps' }, link: '/console/applications?move=analytics', details: ['Step 4 of 9: Copy data', 'copied 1.2 GiB'] },
    { id: 'update:v0.3.0-private.48:local', kind: 'update', title: 'Updating GUI to v0.3.0-private.48', server_id: 'local', server_name: 'GUI', state: 'waiting', message: 'Waits for media-01 to finish updating', waiting_on: 'Waits for media-01 to finish updating', updated: iso(1), progress: { done: 0, total: 2, unit: 'servers' }, link: '/console/updates', details: [] },
    { id: 'deployment:media:d1', kind: 'deployment', title: 'Deploying storefront failed', server_id: 'media', server_name: 'media-01', app_id: 'storefront', state: 'failed', message: 'bundle install failed', started: iso(40), updated: iso(30), progress: null, link: '/console/deployments/d1', details: [] },
  ],
};

function stubFeed() {
  const fetch = vi.fn(async () => ({ ok: true, status: 200, json: async () => feed }) as Response);
  vi.stubGlobal('fetch', fetch);
  return fetch;
}

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); try { localStorage.clear(); } catch { /* none */ } });

describe('ActivityDrawer', () => {
  it('counts running and waiting work on the header button', async () => {
    const fetch = stubFeed();
    render(<ActivityDrawer />);
    expect(await screen.findByTestId('ops-count')).toHaveTextContent('2');
    expect(screen.getByRole('button', { name: 'Activity, 2 in progress' })).toBeInTheDocument();
    expect(fetch).toHaveBeenCalledWith('/api/control/activity', expect.anything());
  });

  it('groups items, explains waits and links to the page of the job', async () => {
    stubFeed();
    render(<ActivityDrawer />);
    fireEvent.click(await screen.findByRole('button', { name: 'Activity, 2 in progress' }));
    const running = await screen.findByRole('region', { name: 'Running' });
    const waiting = screen.getByRole('region', { name: 'Waiting' });
    const finished = screen.getByRole('region', { name: 'Finished recently' });
    expect(within(running).getByText('Moving analytics to media-01')).toBeInTheDocument();
    expect(within(running).getByText('3 of 9 steps')).toBeInTheDocument();
    expect(within(waiting).getByText('Waits for media-01 to finish updating')).toBeInTheDocument();
    expect(within(finished).getByText('Deploying storefront failed')).toBeInTheDocument();

    fireEvent.click(within(running).getByRole('button', { name: /Moving analytics/ }));
    expect(within(running).getByText('Step 4 of 9: Copy data')).toBeInTheDocument();
    expect(within(running).getByRole('link', { name: 'Open' })).toHaveAttribute('href', '/console/applications?move=analytics');
  });

  it('dismisses finished items and keeps them dismissed', async () => {
    stubFeed();
    render(<ActivityDrawer />);
    fireEvent.click(await screen.findByRole('button', { name: 'Activity, 2 in progress' }));
    const finished = await screen.findByRole('region', { name: 'Finished recently' });
    fireEvent.click(within(finished).getByRole('button', { name: 'Dismiss Deploying storefront failed' }));
    expect(screen.queryByRole('region', { name: 'Finished recently' })).not.toBeInTheDocument();
    cleanup();
    render(<ActivityDrawer />);
    fireEvent.click(await screen.findByRole('button', { name: 'Activity, 2 in progress' }));
    await screen.findByRole('region', { name: 'Running' });
    expect(screen.queryByText('Deploying storefront failed')).not.toBeInTheDocument();
  });

  it('polls faster while open and not while the tab is hidden', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const fetch = stubFeed();
    render(<ActivityDrawer />);
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
    await vi.advanceTimersByTimeAsync(CLOSED_POLL_MS);
    expect(fetch).toHaveBeenCalledTimes(2);
    fireEvent.click(screen.getByRole('button', { name: /^Activity/ }));
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledTimes(3));
    await vi.advanceTimersByTimeAsync(OPEN_POLL_MS);
    expect(fetch).toHaveBeenCalledTimes(4);
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
    await vi.advanceTimersByTimeAsync(OPEN_POLL_MS * 3);
    expect(fetch).toHaveBeenCalledTimes(4);
    hidden.mockReturnValue(false);
    document.dispatchEvent(new Event('visibilitychange'));
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledTimes(5));
  });

  it('labels byte progress', () => {
    expect(progressLabel({ done: 1288490188, total: 4294967296, unit: 'bytes' })).toBe('1.2 GiB of 4.0 GiB');
    expect(progressLabel({ done: 1, total: 1, unit: 'servers' })).toBe('1 of 1 server');
  });
});
