import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { AppSettingsPage } from './AppSettingsPage';

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

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
