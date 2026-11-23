import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ServerReadiness } from './ServerReadiness';

const api = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }));
const notify = vi.hoisted(() => vi.fn());
vi.mock('../lib/api', () => api);
vi.mock('./ToastProvider', () => ({ useToast: () => notify }));
const flush = async () => { await act(async () => { await Promise.resolve(); await Promise.resolve(); }); };
const down = { id: 'postgres', title: 'PostgreSQL', ok: false, repairable: true, detail: 'PostgreSQL 17 cluster "main" (port 5432) cannot start: the postgres account cannot enter /mnt/data.' };

describe('ServerReadiness', () => {
  beforeEach(() => { api.get.mockReset(); api.post.mockReset(); notify.mockReset(); });
  afterEach(cleanup);

  it('names what is wrong and repairs it on request', async () => {
    api.get.mockResolvedValue({ ok: false, checks: [down] });
    api.post.mockResolvedValue({ ok: true, checks: [{ ...down, ok: true, repairable: false, detail: 'PostgreSQL 17 cluster "main" (port 5432) is online.' }], repaired: ['started PostgreSQL 17 cluster "main" (port 5432)'] });
    render(<ServerReadiness serverID="home" />);
    await flush();
    expect(screen.getByText('Needs attention')).toBeInTheDocument();
    expect(screen.getByText(/cannot enter \/mnt\/data/)).toBeInTheDocument();
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Repair now' })); }); await flush();
    expect(api.post).toHaveBeenCalledWith('control/servers/home/host/readiness/repair', {});
    expect(screen.getByText('Ready')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Repair now' })).not.toBeInTheDocument();
    expect(notify).toHaveBeenCalledWith('Repaired: started PostgreSQL 17 cluster "main" (port 5432).');
  });

  it('says what only a person can decide, and why a repair did not work', async () => {
    api.get.mockResolvedValue({ ok: false, checks: [{ id: 'postgres', title: 'PostgreSQL', ok: false, detail: 'PostgreSQL 17 cluster "main" (port 5432) is a standby (in recovery).' }, { id: 'docker', title: 'Docker', ok: false, repairable: true, detail: 'Docker is installed but its daemon is not running.' }] });
    api.post.mockResolvedValue({ ok: false, checks: [{ id: 'docker', title: 'Docker', ok: false, repairable: true, detail: 'Docker is installed but its daemon is not running.' }], failed: ['Docker could not be started: unit masked'] });
    render(<ServerReadiness serverID="home" />);
    await flush();
    expect(screen.getByText('Needs you')).toBeInTheDocument();
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Repair now' })); }); await flush();
    expect(screen.getByRole('alert')).toHaveTextContent('Docker could not be started: unit masked');
  });

  it('shows nothing for an agent without the report', async () => {
    api.get.mockRejectedValue(new Error('older agent'));
    const { container } = render(<ServerReadiness serverID="home" />);
    await flush();
    expect(container).toBeEmptyDOMElement();
  });
});
