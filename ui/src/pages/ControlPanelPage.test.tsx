import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ControlPanelPage, dnsRecords, type PanelMove, type PanelMovePlan, type PanelMoveView } from './ControlPanelPage';

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

const view: PanelMoveView = { hostname: 'deploy.example.com', public_url: 'https://deploy.example.com', version: 'v1.0.0-private.1', read_only: false, servers: [{ id: 'old', name: 'Old host' }, { id: 'combined', name: 'Combined' }] };

const plan: PanelMovePlan = {
  input: { target: 'combined' }, hostname: 'deploy.example.com', target_name: 'Combined', source_server: 'old', source_name: 'Old host',
  servers: [
    { id: 'combined', name: 'Combined', role: 'destination', url: 'http://127.0.0.1:19092', new_url: 'http://127.0.0.1:9092', reachable: true, apps: 4, detail: "Becomes this panel's own server." },
    { id: 'old', name: 'Old host', role: 'source', url: 'http://127.0.0.1:9090', reachable: false, retire: true, apps: 0, detail: 'Holds no applications.' },
  ],
  addresses: ['203.0.113.9', '2001:db8::9'], data_files: 5, data_bytes: 20480, database: 'managed', database_bytes: 8 * 1024 * 1024,
  ingress: 'caddy', ingress_detail: 'a route through the Caddy admin API', certificate: 'The current certificate is copied.', certificate_ok: true, version: 'v1.0.0-private.1',
  checks: [{ name: 'version', ok: true, detail: 'The destination runs the same release v1.0.0-private.1.' }], blockers: [], warnings: [], ready: true,
};

const moving: PanelMove = {
  id: 'm1', target: 'combined', target_name: 'Combined', hostname: 'deploy.example.com', state: 'dns', phase: 'cutover', read_only: true, plan,
  addresses: ['203.0.113.9', '2001:db8::9'], certificate: 'acme', log: [{ at: '2026-11-06T12:00:00Z', message: 'The destination is active.' }],
  steps: [{ name: 'activate', title: 'Activate the destination', status: 'done' }, { name: 'dns', title: 'DNS and HTTPS', status: 'waiting', detail: 'Set the DNS records shown below.' }],
  dns: [{ hostname: 'deploy.example.com', addresses: ['198.51.100.7'], ready: false, source: 'ns1.example.com' }],
};

function respond(body: unknown, status = 200) { return Promise.resolve({ ok: status < 400, status, json: async () => body }); }

describe('ControlPanelPage', () => {
  it('reviews the plan and starts the move', async () => {
    const calls: Array<{ url: string; method: string; body?: string }> = [];
    let started = false;
    vi.stubGlobal('fetch', vi.fn((url: string, init: RequestInit = {}) => {
      calls.push({ url, method: init.method || 'GET', body: init.body as string | undefined });
      if (url === '/api/control/panel-move/plan') return respond(plan);
      if (url === '/api/control/panel-move' && init.method === 'POST') { started = true; return respond({ ...moving, state: 'running', phase: 'prepare', read_only: false }, 202); }
      return respond(started ? { ...view, move: { ...moving, state: 'running', phase: 'prepare', read_only: false, steps: [{ name: 'check', title: 'Check the destination', status: 'running' }] } } : view);
    }));
    render(<ControlPanelPage />);
    fireEvent.change(await screen.findByLabelText('Destination server'), { target: { value: 'combined' } });
    expect(screen.queryByRole('option', { name: 'Old host' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Review the move' }));
    const review = await screen.findByLabelText('Move plan');
    expect(within(review).getByText(/8.0 MiB, copied with pg_dump and pg_restore/)).toBeInTheDocument();
    expect(within(review).getByText('Leaves the server list (retire)')).toBeInTheDocument();
    expect(within(review).getByText('http://127.0.0.1:9092')).toBeInTheDocument();
    expect(JSON.parse(calls.find(c => c.url.endsWith('/plan'))!.body!)).toEqual({ target: 'combined' });
    fireEvent.click(screen.getByRole('button', { name: /Start the move/ }));
    expect(await screen.findByText('Moving to Combined')).toBeInTheDocument();
    expect(calls.some(c => c.url === '/api/control/panel-move' && c.method === 'POST')).toBe(true);
    expect(screen.getByRole('button', { name: 'Abort' })).toBeInTheDocument();
  });

  it('blocks a plan with blockers and asks for unreachable agent URLs', async () => {
    const blocked: PanelMovePlan = { ...plan, ready: false, blockers: ['The destination cannot reach Edge at http://127.0.0.1:19093 (no answer); enter an agent URL it can reach.'],
      servers: [...plan.servers, { id: 'edge', name: 'Edge', role: 'remote', url: 'http://127.0.0.1:19093', new_url: 'http://127.0.0.1:19093', reachable: false, apps: 1, detail: 'no answer' }] };
    vi.stubGlobal('fetch', vi.fn((url: string) => respond(url.endsWith('/plan') ? blocked : view)));
    render(<ControlPanelPage />);
    fireEvent.change(await screen.findByLabelText('Destination server'), { target: { value: 'combined' } });
    fireEvent.click(screen.getByRole('button', { name: 'Review the move' }));
    expect(await screen.findByText(/cannot reach Edge/)).toBeInTheDocument();
    expect(screen.getByLabelText('Agent URL of Edge')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Start the move/ })).toBeDisabled();
  });

  it('shows the DNS records to set with copy buttons while waiting for DNS', async () => {
    vi.stubGlobal('fetch', vi.fn(() => respond({ ...view, read_only: true, move: moving })));
    render(<ControlPanelPage />);
    expect(await screen.findByText('DNS records for deploy.example.com')).toBeInTheDocument();
    expect(screen.getByText('Read-only: moving')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Copy A' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Copy AAAA' })).toBeInTheDocument();
    expect(screen.getByText(/198.51.100.7 \(ns1.example.com\)/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Abort' })).not.toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: /Check DNS now/ })).toBeEnabled());
  });

  it('builds A and AAAA records from the destination addresses', () => {
    expect(dnsRecords('deploy.example.com', ['203.0.113.9', '2001:db8::9'])).toEqual([
      { name: 'deploy.example.com', type: 'A', value: '203.0.113.9' },
      { name: 'deploy.example.com', type: 'AAAA', value: '2001:db8::9' },
    ]);
  });
});
