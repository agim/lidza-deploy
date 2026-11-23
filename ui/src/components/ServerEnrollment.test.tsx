import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ServerEnrollment } from './ServerEnrollment';

const api = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), del: vi.fn() }));
const refresh = vi.hoisted(() => vi.fn(() => Promise.resolve()));
const navigate = vi.hoisted(() => vi.fn());
vi.mock('../lib/api', () => api);
vi.mock('../state/WorkspaceContext', () => ({ useWorkspace: () => ({ refresh }) }));
vi.mock('./ToastProvider', () => ({ useToast: () => vi.fn() }));
vi.mock('../lib/router', async original => ({ ...(await original<typeof import('../lib/router')>()), navigate }));

// A field's label element also holds its hint, so match the label text as a prefix.
const fill = (label: string, value: string) => fireEvent.change(screen.getByLabelText(new RegExp(`^${label}`)), { target: { value } });
const flush = async () => { await act(async () => { await Promise.resolve(); await Promise.resolve(); }); };

describe('ServerEnrollment', () => {
  beforeEach(() => { vi.useFakeTimers(); api.get.mockReset(); api.post.mockReset(); api.del.mockReset(); refresh.mockClear(); navigate.mockClear(); });
  afterEach(() => { cleanup(); vi.useRealTimers(); });

  it('installs over SSH and opens the server once it is connected', async () => {
    api.post.mockResolvedValue({ id: 'one', name: 'Server One', hostname: 'server1.example.com', mode: 'ssh', state: 'installing', message: 'Connecting over SSH' });
    api.get.mockResolvedValueOnce({ id: 'one', name: 'Server One', hostname: 'server1.example.com', mode: 'ssh', state: 'installing', message: 'Logged in; installing the agent', host_key: 'SHA256:abc', log: 'Downloading the agent release…\n' })
      .mockResolvedValue({ id: 'one', name: 'Server One', hostname: 'server1.example.com', mode: 'ssh', state: 'connected', message: 'Server connected' });
    render(<ServerEnrollment mode="ssh" />);
    fill('Server ID', 'one'); fill('Display name', 'Server One'); fill('Hostname', 'server1.example.com'); fill('SSH password', 'pass-word');
    fireEvent.click(screen.getByRole('button', { name: 'Install and connect' }));
    await flush();
    // The password travels in the request body only; it is one-time and not kept in the form afterwards.
    expect(api.post).toHaveBeenCalledWith('control/server-enrollments', { id: 'one', name: 'Server One', hostname: 'server1.example.com', email: '', ssh: { user: 'root', port: 22, address: '', host_key: '', password: 'pass-word' } });
    expect(screen.getByRole('status')).toHaveTextContent('Installing. Connecting over SSH');
    expect(screen.queryByLabelText('SSH password')).not.toBeInTheDocument();
    await act(async () => { vi.advanceTimersByTime(2000); }); await flush();
    expect(screen.getByText('SHA256:abc')).toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'Installer output' })).toHaveTextContent('Downloading the agent release');
    await act(async () => { vi.advanceTimersByTime(2000); }); await flush();
    expect(refresh).toHaveBeenCalled();
    expect(navigate).toHaveBeenCalledWith(expect.stringContaining('/servers/one'));
  });

  it('shows the one command to run, and the reason when the installation fails', async () => {
    api.post.mockResolvedValue({ id: 'two', name: 'Two', hostname: 'server2.example.com', mode: 'command', state: 'waiting', message: 'Waiting for the command to run on the server', command: 'curl -fsSL https://deploy.example/enroll/abc/install.sh | sudo sh' });
    api.get.mockResolvedValue({ id: 'two', name: 'Two', hostname: 'server2.example.com', mode: 'command', state: 'failed', message: 'The enrollment expired before the server connected; start again' });
    render(<ServerEnrollment mode="command" />);
    expect(screen.queryByLabelText('SSH password')).not.toBeInTheDocument();
    fill('Server ID', 'two'); fill('Display name', 'Two'); fill('Hostname', 'server2.example.com');
    fireEvent.click(screen.getByRole('button', { name: 'Create install command' }));
    await flush();
    expect(api.post).toHaveBeenCalledWith('control/server-enrollments', { id: 'two', name: 'Two', hostname: 'server2.example.com', email: '' });
    expect(screen.getByLabelText('Install command')).toHaveValue('curl -fsSL https://deploy.example/enroll/abc/install.sh | sudo sh');
    await act(async () => { vi.advanceTimersByTime(2000); }); await flush();
    expect(screen.getByRole('alert')).toHaveTextContent('The enrollment expired');
    expect(screen.queryByLabelText('Install command')).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Start again' }));
    expect(screen.getByRole('button', { name: 'Create install command' })).toBeInTheDocument();
  });

  it('shows why the control panel refused to start', async () => {
    api.post.mockRejectedValue(new Error('server ID already exists'));
    render(<ServerEnrollment mode="command" />);
    fill('Server ID', 'one'); fill('Display name', 'One'); fill('Hostname', 'server1.example.com');
    fireEvent.click(screen.getByRole('button', { name: 'Create install command' }));
    await flush();
    expect(screen.getByRole('alert')).toHaveTextContent('server ID already exists');
  });
});
