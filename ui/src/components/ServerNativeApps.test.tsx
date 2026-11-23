import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ServerNativeApps } from './ServerNativeApps';

const api = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }));
const notify = vi.hoisted(() => vi.fn());
vi.mock('../lib/api', () => api);
vi.mock('./ToastProvider', () => ({ useToast: () => notify }));

const flush = async () => { await act(async () => { await Promise.resolve(); await Promise.resolve(); }); };

describe('ServerNativeApps', () => {
  beforeEach(() => { vi.useFakeTimers(); api.get.mockReset(); api.post.mockReset(); notify.mockReset(); });
  afterEach(() => { cleanup(); vi.useRealTimers(); });

  it('enables native applications after confirmation and follows the agent restart', async () => {
    api.get.mockResolvedValueOnce({ configured: false, supported: true });
    api.post.mockResolvedValue({ configured: false, supported: true, restarting: true, user: 'deploy', root: '/home/deploy' });
    render(<ServerNativeApps serverID="home" />);
    await flush();
    expect(screen.getByText('Not enabled')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Enable native applications' }));
    // Nothing is sent before the operator confirms the agent restart.
    expect(api.post).not.toHaveBeenCalled();
    await act(async () => { fireEvent.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Enable' })); }); await flush();
    expect(api.post).toHaveBeenCalledWith('control/servers/home/host/native', {});
    expect(screen.getByRole('status')).toHaveTextContent('restarting');
    // The agent is away while it restarts, then reports the configuration.
    api.get.mockRejectedValueOnce(new Error('agent unavailable')).mockResolvedValue({ configured: true, supported: true, user: 'deploy', root: '/home/deploy' });
    await act(async () => { vi.advanceTimersByTime(3000); }); await flush();
    expect(screen.getByRole('status')).toHaveTextContent('restarting');
    await act(async () => { vi.advanceTimersByTime(3000); }); await flush();
    expect(screen.getByText('Enabled')).toBeInTheDocument();
    expect(screen.getByText('/home/deploy')).toBeInTheDocument();
    expect(notify).toHaveBeenCalledWith('Native applications are enabled on this server.');
  });

  it('says why a server cannot be enabled from here', async () => {
    api.get.mockResolvedValue({ configured: false, supported: false, reason: 'The agent does not run as root on this server, so it cannot create the deployment account.' });
    render(<ServerNativeApps serverID="home" />);
    await flush();
    expect(screen.getByText(/does not run as root/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Enable native applications' })).not.toBeInTheDocument();
  });
});
