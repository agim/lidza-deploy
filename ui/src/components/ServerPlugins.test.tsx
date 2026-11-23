import { act, cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ServerPlugins } from './ServerPlugins';

const api = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), put: vi.fn(), del: vi.fn() }));
const notify = vi.hoisted(() => vi.fn());
vi.mock('../lib/api', () => api);
vi.mock('./ToastProvider', () => ({ useToast: () => notify }));
const flush = async () => { await act(async () => { await Promise.resolve(); await Promise.resolve(); await Promise.resolve(); }); };
const installed = { name: 'search', version: '1.0', provides: 'search', summary: 'A search server per application.', hooks: ['describe', 'check'], key_id: '0123456789abcdef' };

describe('ServerPlugins', () => {
  beforeEach(() => { api.get.mockReset(); api.post.mockReset(); api.put.mockReset(); api.del.mockReset(); notify.mockReset(); });
  afterEach(cleanup);

  it('shows the plugins, runs a plugin’s own check and installs one from its release', async () => {
    api.get.mockResolvedValueOnce({ plugins: [installed], keys: ['0123456789abcdef'], operator_keys: ['ab'.repeat(32)], pinned: 1, arch: 'arm64' })
      .mockResolvedValue({ plugins: [installed, { name: 'queue', version: '2.1', provides: 'queue', hooks: ['describe'] }], keys: ['0123456789abcdef'], operator_keys: ['ab'.repeat(32)], arch: 'arm64' });
    api.post.mockImplementation((path: string) => path.endsWith('/hooks/check')
      ? Promise.resolve({ answer: { ok: false, checks: [{ id: 'server', title: 'Search server', ok: false, repairable: true, detail: 'its service failed' }] } })
      : Promise.resolve({ name: 'queue', version: '2.1' }));
    render(<ServerPlugins serverID="home" />);
    await flush();
    expect(screen.getByText('search 1.0')).toBeInTheDocument();
    expect(screen.getByText(/this server is arm64/)).toBeInTheDocument();
    expect(screen.getByText(/1 key is built into the agent/)).toBeInTheDocument();
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Check server' })); }); await flush();
    expect(api.post).toHaveBeenCalledWith('control/servers/home/plugins/search/hooks/check', {});
    expect(within(screen.getByRole('list', { name: 'Checks of search' })).getByText('its service failed')).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText(/^Repository/), { target: { value: 'example/deploy-plugin-queue' } });
    fireEvent.change(screen.getByLabelText('Version'), { target: { value: '2.1' } });
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Install plugin' })); }); await flush();
    expect(api.post).toHaveBeenCalledWith('control/servers/home/plugins/install', { repository: 'example/deploy-plugin-queue', version: '2.1' });
    expect(screen.getByText('queue 2.1')).toBeInTheDocument();
    expect(notify).toHaveBeenCalledWith('Plugin queue 2.1 installed.');
  });

  it('saves the trusted keys as a list and shows why the server refused an install', async () => {
    api.get.mockResolvedValue({ plugins: [], keys: [], operator_keys: [], pinned: 0, arch: 'amd64' });
    api.put.mockResolvedValue({ plugins: [], keys: ['cdcdcdcdcdcdcdcd'], operator_keys: ['cd'.repeat(32)], arch: 'amd64' });
    api.post.mockRejectedValue(new Error('the plugin was signed by a key this server does not trust'));
    render(<ServerPlugins serverID="home" />);
    await flush();
    expect(screen.getByText('No plugins on this server')).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText('Public keys you trust'), { target: { value: `${'cd'.repeat(32)}\n\n` } });
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Save trusted keys' })); }); await flush();
    expect(api.put).toHaveBeenCalledWith('control/servers/home/plugins/keys', { keys: ['cd'.repeat(32)] });
    fireEvent.change(screen.getByLabelText(/^Repository/), { target: { value: 'example/x' } });
    fireEvent.change(screen.getByLabelText('Version'), { target: { value: '1.0' } });
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Install plugin' })); }); await flush();
    expect(screen.getByRole('alert')).toHaveTextContent('does not trust');
  });

  it('says so when the server’s agent has no plugins yet', async () => {
    api.get.mockRejectedValue(new Error('this server’s agent is older than the control panel and has no plugins yet'));
    render(<ServerPlugins serverID="home" />);
    await flush();
    expect(screen.getByRole('alert')).toHaveTextContent('has no plugins yet');
  });
});
