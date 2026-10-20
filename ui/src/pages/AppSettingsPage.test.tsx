import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { AppSettingsPage } from './AppSettingsPage';

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe('AppSettingsPage', () => {
  it('preserves routing and safety values when saving from the environment tab', async () => {
    const requests: RequestInit[] = [];
    vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init: RequestInit = {}) => {
      requests.push(init);
      return { ok: true, status: 200, json: async () => ({ id: 'portal', server_id: 'local', repository: 'acme/portal', branch: 'main', domain: 'portal.example.com', customer_hosts: true, backup_before_deploy: true, environment: [] }) } as Response;
    }));

    render(<AppSettingsPage appID="portal" />);
    await screen.findByRole('heading', { name: 'portal' });
    fireEvent.click(screen.getByRole('tab', { name: 'Environment' }));
    fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));

    await waitFor(() => expect(requests.some(request => request.method === 'PATCH')).toBe(true));
    const request = requests.find(item => item.method === 'PATCH');
    const body = JSON.parse(String(request?.body));
    expect(body).toMatchObject({ branch: 'main', domain: 'portal.example.com', customer_hosts: true, backup_before_deploy: true });
  });
});

it('saves profile selection and preserves it when switching to environment', async () => {
  const requests: RequestInit[] = [];
  vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init: RequestInit = {}) => {
    requests.push(init);
    return { ok: true, status: 200, json: async () => ({ id: 'portal', branch: 'main', domain: 'portal.example.com', stack_profiles_supported: true, runtime_profile: 'minimum', runtime_deployed: true, environment: [] }) } as Response;
  }));
  render(<AppSettingsPage appID="portal" />);
  const profile = await screen.findByRole('combobox', { name: 'Runtime profile' });
  expect(profile).toHaveValue('minimum');
  expect(screen.queryByRole('option', { name: /Single container/ })).not.toBeInTheDocument();
  fireEvent.change(profile, { target: { value: 'standard' } });
  fireEvent.click(screen.getByRole('tab', { name: 'Environment' }));
  fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(requests.some(request => request.method === 'PATCH')).toBe(true));
  expect(JSON.parse(String(requests.find(item => item.method === 'PATCH')?.body))).toMatchObject({ runtime_profile: 'standard', domain: 'portal.example.com', branch: 'main' });
});
