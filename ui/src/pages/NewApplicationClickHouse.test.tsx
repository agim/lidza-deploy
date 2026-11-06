import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import type { RepositoryDetection } from '../lib/types';
import { NewApplicationPage } from './NewApplicationPage';

vi.mock('../state/WorkspaceContext', () => ({ useWorkspace: () => ({ servers: [{ id: 'host', name: 'Host', url: 'https://agent.example.com' }], apps: [], status: {}, refresh: vi.fn(), loading: false }) }));
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const detection: RepositoryDetection = {
  repository: 'acme/events', branch: 'main', default_branch: 'main', branches: ['main'], installation_id: 7, detected: true, mode: 'native', runtime: 'go', framework: 'go',
  processes: [{ name: 'web', command: 'bin/events', web: true }], needs_postgres: false, needs_redis: false, needs_clickhouse: true, health_path: '/',
  reasons: ['go.mod requires github.com/ClickHouse/clickhouse-go/v2: ClickHouse'],
};

function stubAPI() {
  const respond = (body: unknown, status = 200) => ({ ok: status < 400, status, json: async () => body });
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    if (url.startsWith('/api/control/github/repos')) return respond([{ full_name: 'acme/events', default_branch: 'main', private: true, installation_id: 7 }]);
    if (url.startsWith('/api/control/repos/detect')) return respond(detection);
    if (url === '/api/control/native-apps' && init?.method === 'POST') return respond({ id: 'events' }, 201);
    return respond({ error: `unexpected ${url}` }, 404);
  });
  vi.stubGlobal('fetch', fetch);
  return fetch;
}

const next = () => fireEvent.click(screen.getByRole('button', { name: /Continue/ }));

it('prefills ClickHouse from detection and requests it with backups', async () => {
  const fetch = stubAPI();
  render(<NewApplicationPage />);
  fireEvent.change(screen.getByLabelText(/Application name/), { target: { value: 'events' } });
  next();
  fireEvent.click(await screen.findByRole('button', { name: /acme\/events/ }));
  await screen.findByText(/Detected/);
  next();

  expect(screen.getByText('go.mod requires github.com/ClickHouse/clickhouse-go/v2: ClickHouse')).toBeInTheDocument();
  expect(screen.getByRole('switch', { name: 'ClickHouse' })).toBeChecked();
  next();
  next();
  fireEvent.change(screen.getByLabelText(/Application hostname/), { target: { value: 'events.example.com' } });
  next();

  expect(screen.getByRole('radio', { name: /No database/ })).toBeChecked();
  expect(screen.getByRole('switch', { name: 'ClickHouse on this server' })).toBeChecked();
  // ClickHouse alone is backed up, so the schedule is offered.
  expect(screen.getByLabelText('Backup frequency')).toHaveValue('24');
  next();

  fireEvent.click(screen.getByRole('button', { name: /Add variable/ }));
  fireEvent.change(screen.getByLabelText('Variable 1 name'), { target: { value: 'CLICKHOUSE_URL' } });
  next();
  expect(screen.getByText('The ClickHouse setting supplies CLICKHOUSE_URL.')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Remove variable 1' }));
  next();

  expect(screen.getByText('ClickHouse on the server')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: /Create application/ }));
  await waitFor(() => expect(fetch.mock.calls.some(([url]) => url === '/api/control/native-apps')).toBe(true));
  const call = fetch.mock.calls.find(([url]) => url === '/api/control/native-apps');
  const body = JSON.parse(String(call?.[1]?.body));
  expect(body.data).toMatchObject({ postgres: false, redis: false, clickhouse: true, backup_hours: 24, offsite: false });
}, 15000);
