import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import type { RepositoryDetection } from '../lib/types';
import { NewApplicationPage } from './NewApplicationPage';

vi.mock('../state/WorkspaceContext', () => ({ useWorkspace: () => ({ servers: [{ id: 'host', name: 'Host', url: 'https://agent.example.com' }], apps: [{ id: 'taken' }], status: {}, refresh: vi.fn(), loading: false }) }));
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const detection: RepositoryDetection = {
  repository: 'acme/shop', branch: 'main', default_branch: 'main', branches: ['main', 'release'], installation_id: 7, detected: true, mode: 'native', runtime: 'rails', framework: 'rails', framework_version: '8.0.1',
  processes: [{ name: 'web', command: 'bin/rails server', web: true }, { name: 'worker', command: 'bundle exec sidekiq' }], needs_postgres: true, needs_redis: true, pins: { ruby: '3.4.1' }, health_path: '/up',
  reasons: ['Gemfile and config/application.rb: Ruby on Rails 8.0', 'sidekiq gem: a worker process runs bundle exec sidekiq'],
};

function stubAPI() {
  const respond = (body: unknown, status = 200) => ({ ok: status < 400, status, json: async () => body });
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    if (url.startsWith('/api/control/github/repos')) return respond([{ full_name: 'acme/shop', default_branch: 'main', private: true, installation_id: 7 }]);
    if (url.startsWith('/api/control/repos/detect')) return respond(detection);
    if (url.startsWith('/api/control/application-defaults')) return respond({ runtimes: { rails: { values: { RAILS_LOG_TO_STDOUT: '1' }, revision: 'r1' } } });
    if (url === '/api/control/native-apps' && init?.method === 'POST') return respond({ id: 'shop' }, 201);
    if (url === '/api/control/apps/shop/deploy' && init?.method === 'POST') return respond({ id: 'd1' }, 202);
    return respond({ error: `unexpected ${url}` }, 404);
  });
  vi.stubGlobal('fetch', fetch);
  return fetch;
}

const next = () => fireEvent.click(screen.getByRole('button', { name: /Continue/ }));

it('validates the application name as it is typed', () => {
  stubAPI();
  render(<NewApplicationPage />);
  const name = screen.getByLabelText(/Application name/);
  fireEvent.change(name, { target: { value: '9lives' } });
  expect(screen.getByText('Start with a lowercase letter.')).toBeInTheDocument();
  fireEvent.change(name, { target: { value: 'taken' } });
  expect(screen.getByText('An application with this name already exists.')).toBeInTheDocument();
  fireEvent.change(name, { target: { value: 'shop' } });
  expect(screen.getByText('shop is available.')).toBeInTheDocument();
  expect(screen.getByRole('button', { name: /Name/ })).toHaveAttribute('aria-current', 'step');
});

it('creates a native Rails application from the detected setup', async () => {
  const fetch = stubAPI();
  render(<NewApplicationPage />);
  fireEvent.change(screen.getByLabelText(/Application name/), { target: { value: 'shop' } });
  next();

  fireEvent.click(await screen.findByRole('button', { name: /acme\/shop/ }));
  await screen.findByText(/Detected Ruby on Rails 8.0.1/);
  expect(screen.getByLabelText(/Branch/)).toHaveValue('main');
  next();

  expect(screen.getByText('Gemfile and config/application.rb: Ruby on Rails 8.0')).toBeInTheDocument();
  expect(screen.getByRole('radio', { name: /Native/ })).toBeChecked();
  expect(screen.getByLabelText(/Process 2 command/)).toHaveValue('bundle exec sidekiq');
  expect(screen.getByLabelText(/Readiness path/)).toHaveValue('/up');
  expect(screen.getByRole('switch', { name: 'Redis' })).toBeChecked();
  expect(screen.queryByLabelText('Runtime profile')).not.toBeInTheDocument();
  expect(screen.queryByRole('switch', { name: 'Serve approved customer hostnames' })).not.toBeInTheDocument();
  next();

  expect(screen.getByRole('radio', { name: /Host/ })).toBeChecked();
  expect(screen.getByText(/Docker is not required/)).toBeInTheDocument();
  next();

  next();
  expect(screen.getByText(/Enter a hostname/)).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText(/Application hostname/), { target: { value: 'shop.example.com' } });
  next();

  expect(screen.getByRole('radio', { name: /Create PostgreSQL on the server/ })).toBeChecked();
  next();

  expect(screen.queryByLabelText('Application master key')).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText('Rails master key'), { target: { value: 'abc123' } });
  await waitFor(() => expect(screen.getByLabelText('Variable 1 name')).toHaveValue('RAILS_LOG_TO_STDOUT'));
  next();

  expect(screen.getByText('acme/shop @ main')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: /Create application/ }));
  await screen.findByRole('button', { name: /Deploy now/ });

  const call = fetch.mock.calls.find(([url]) => url === '/api/control/native-apps');
  expect(call).toBeDefined();
  const body = JSON.parse(String(call?.[1]?.body));
  expect(body).toMatchObject({ id: 'shop', repository: 'acme/shop', branch: 'main', runtime: 'rails', domain: 'shop.example.com', health_path: '/up', server_id: 'host', github_installation: 7, data: { postgres: true, redis: true, backup_hours: 24, offsite: false } });
  expect(body.processes).toEqual([{ name: 'web', command: 'bin/rails server', web: true }, { name: 'worker', command: 'bundle exec sidekiq' }]);
  expect(body.env).toEqual({ RAILS_LOG_TO_STDOUT: '1', RAILS_MASTER_KEY: 'abc123' });
  expect(body.env_kinds.RAILS_MASTER_KEY).toBe('credential');
  expect(body.env.LIDZA_MASTER_KEY).toBeUndefined();

  fireEvent.click(screen.getByRole('button', { name: /Deploy now/ }));
  await waitFor(() => expect(fetch.mock.calls.some(([url]) => url === '/api/control/apps/shop/deploy')).toBe(true));
}, 15000);
