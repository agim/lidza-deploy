import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import type { RepositoryDetection } from '../lib/types';
import { NewApplicationPage } from './NewApplicationPage';

vi.mock('../state/WorkspaceContext', () => ({ useWorkspace: () => ({ servers: [{ id: 'server', name: 'Test server', url: 'https://agent.example.com' }], apps: [], status: {}, refresh: vi.fn(), loading: false }) }));
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const detection: RepositoryDetection = {
  repository: 'org/marketing', branch: 'main', default_branch: 'main', branches: ['main'], detected: true, mode: 'native', runtime: 'static', framework: 'astro', framework_version: '5.1.0',
  source_directory: 'apps/marketing', static_output: 'dist', needs_postgres: false, needs_redis: false, pins: { node: '22' },
  reasons: ['The repository root has no application; apps/marketing/ does', 'astro.config.mjs: Astro static site published from dist/'],
};

function stubAPI() {
  const respond = (body: unknown, status = 200) => ({ ok: status < 400, status, json: async () => body });
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    if (url.startsWith('/api/control/github/repos')) return respond([]);
    if (url.startsWith('/api/control/repos/detect')) return respond(detection);
    if (url.startsWith('/api/control/application-defaults')) return respond({ runtimes: {} });
    if ((url === '/api/control/native-apps' || url === '/api/control/apps') && init?.method === 'POST') return respond({ id: 'marketing' }, 201);
    return respond({ error: `unexpected ${url}` }, 404);
  });
  vi.stubGlobal('fetch', fetch);
  return fetch;
}

const next = () => fireEvent.click(screen.getByRole('button', { name: /Continue/ }));

async function throughSetup() {
  render(<NewApplicationPage />);
  fireEvent.change(screen.getByLabelText(/Application name/), { target: { value: 'marketing' } });
  next();
  fireEvent.change(screen.getByLabelText(/GitHub repository/), { target: { value: 'org/marketing' } });
  fireEvent.change(screen.getByLabelText(/Branch/), { target: { value: 'main' } });
  fireEvent.click(screen.getByRole('button', { name: /Detect/ }));
  await screen.findByText(/Detected Astro 5.1.0 in apps\/marketing\//);
  next();
  expect(screen.getByLabelText(/Repository directory/)).toHaveValue('apps/marketing');
  expect(screen.getByText('astro.config.mjs: Astro static site published from dist/')).toBeInTheDocument();
}

async function toReview() {
  next();
  next();
  fireEvent.change(screen.getByLabelText(/Application hostname/), { target: { value: 'www.example.com' } });
  next();
  expect(screen.getByRole('radio', { name: /No database/ })).toBeChecked();
  next();
  expect(screen.queryByLabelText('Application master key')).not.toBeInTheDocument();
  expect(screen.queryByLabelText('Rails master key')).not.toBeInTheDocument();
  next();
  fireEvent.click(screen.getByRole('button', { name: /Create application/ }));
}

it('creates a native static Astro site from a monorepo directory', async () => {
  const fetch = stubAPI();
  await throughSetup();
  expect(screen.getByLabelText(/Build output directory/)).toHaveValue('dist');
  expect(screen.queryByLabelText(/Process 1 command/)).not.toBeInTheDocument();
  await toReview();
  await screen.findByRole('button', { name: /Deploy now/ });
  const call = fetch.mock.calls.find(([url]) => url === '/api/control/native-apps');
  const body = JSON.parse(String(call?.[1]?.body));
  expect(body).toMatchObject({ id: 'marketing', runtime: 'static', source_directory: 'apps/marketing', static_output: 'dist', domain: 'www.example.com', data: { postgres: false, redis: false } });
  expect(body.processes).toBeUndefined();
  expect(body.env.LIDZA_MASTER_KEY).toBeUndefined();
}, 15000);

it('keeps the existing container create path when containers are chosen', async () => {
  const fetch = stubAPI();
  await throughSetup();
  fireEvent.click(screen.getByRole('radio', { name: /Containers/ }));
  expect(screen.getByLabelText(/Container runtime/)).toHaveValue('astro');
  await toReview();
  await waitFor(() => expect(fetch.mock.calls.some(([url]) => url === '/api/control/apps')).toBe(true));
  const call = fetch.mock.calls.find(([url]) => url === '/api/control/apps');
  const body = JSON.parse(String(call?.[1]?.body));
  expect(body.runtime).toBe('astro');
  expect(body.build.source_directory).toBe('apps/marketing');
  expect(body.astro.mode).toBe('static');
  expect(body.env.LIDZA_MASTER_KEY).toBeUndefined();
  expect(body.customer_hosts).toBe(false);
  expect(body.runtime_profile).toBe('');
  expect(body.database.mode).toBe('none');
  expect(fetch.mock.calls.some(([url]) => url === '/api/control/native-apps')).toBe(false);
}, 15000);
