import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { DefaultsPage } from './WorkspacePages';
import { ApplyDefaultsButton } from '../components/ApplyDefaults';

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

const profile = {
  values: { DB_MIGRATE: 'true', LOG_LEVEL: 'info' }, revision: 'rev-go',
  runtimes: {
    go: { values: { DB_MIGRATE: 'true', LOG_LEVEL: 'info' }, revision: 'rev-go' },
    rails: { values: { RAILS_ENV: 'production', RACK_ENV: 'production' }, revision: 'rev-rails' },
    astro: { values: { NODE_ENV: 'production', ASTRO_TELEMETRY_DISABLED: '1' }, revision: 'rev-astro' },
    node: { values: { NODE_ENV: 'production', TZ: 'UTC' }, revision: 'rev-node' },
  },
};

it('edits and saves workspace defaults per runtime and shows API validation messages', { timeout: 15000 }, async () => {
  const requests: Array<{ url: string; init: RequestInit }> = [];
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
    requests.push({ url: String(input), init });
    if (init.method === 'PUT') {
      const body = JSON.parse(String(init.body));
      if (body.values.SECRET_KEY_BASE) return { ok: false, status: 400, json: async () => ({ error: 'SECRET_KEY_BASE looks like a credential or service connection; configure it on each application' }) } as Response;
      return { ok: true, status: 200, json: async () => ({ ...profile, runtimes: { ...profile.runtimes, [body.runtime]: { values: body.values, revision: 'saved' } } }) } as Response;
    }
    return { ok: true, status: 200, json: async () => profile } as Response;
  }));
  render(<DefaultsPage />);
  for (const label of ['Līdza (Go)', 'Ruby on Rails', 'Astro', 'Node.js']) expect(await screen.findByRole('tab', { name: label })).toBeInTheDocument();
  expect(await screen.findByLabelText('DB_MIGRATE value')).toHaveValue('true');
  fireEvent.mouseDown(screen.getByRole('tab', { name: 'Ruby on Rails' }), { button: 0, ctrlKey: false });
  expect(await screen.findByLabelText('RAILS_ENV value')).toHaveValue('production');
  fireEvent.click(screen.getByRole('button', { name: 'Add variable' }));
  const names = screen.getAllByRole('textbox', { name: 'Variable name' });
  fireEvent.change(names[names.length - 1], { target: { value: 'SECRET_KEY_BASE' } });
  fireEvent.change(screen.getByLabelText('SECRET_KEY_BASE value'), { target: { value: 'shared' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save Ruby on Rails defaults' }));
  expect(await screen.findByText(/SECRET_KEY_BASE looks like a credential/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Remove SECRET_KEY_BASE' }));
  // The button holds its progress indicator briefly after each request.
  await waitFor(() => expect(screen.getByRole('button', { name: 'Save Ruby on Rails defaults' })).toBeEnabled(), { timeout: 4000 });
  fireEvent.click(screen.getByRole('button', { name: 'Save Ruby on Rails defaults' }));
  await waitFor(() => expect(requests.filter(request => request.init.method === 'PUT')).toHaveLength(2));
  const saved = JSON.parse(String(requests.filter(request => request.init.method === 'PUT')[1].init.body));
  expect(saved).toEqual({ runtime: 'rails', values: { RACK_ENV: 'production', RAILS_ENV: 'production' } });
});

it('reviews the application runtime set before applying defaults', async () => {
  const requests: Array<{ url: string; init: RequestInit }> = [];
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
    requests.push({ url: String(input), init });
    return { ok: true, status: 200, json: async () => init.method === 'POST' ? { saved: true, runtime: 'node' } : profile } as Response;
  }));
  const applied = vi.fn();
  render(<ApplyDefaultsButton appID="api" runtime="node" presentKeys={['NODE_ENV']} onApplied={applied} />);
  fireEvent.click(screen.getByRole('button', { name: 'Apply workspace defaults' }));
  expect(await screen.findByRole('heading', { name: 'Apply Node.js defaults' })).toBeInTheDocument();
  expect(await screen.findByRole('checkbox', { name: 'TZ' })).toBeChecked();
  expect(screen.getByRole('checkbox', { name: 'NODE_ENV' })).not.toBeChecked();
  expect(screen.getByText(/Already set/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Apply selected defaults' }));
  await waitFor(() => expect(applied).toHaveBeenCalled());
  const post = requests.find(request => request.init.method === 'POST');
  expect(post?.url).toBe('/api/control/apps/api/apply-defaults');
  expect(JSON.parse(String(post?.init.body))).toEqual({ keys: ['TZ'], replace: false, revision: 'rev-node' });
});
