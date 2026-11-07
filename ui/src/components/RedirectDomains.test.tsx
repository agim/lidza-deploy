import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { RedirectDomains } from './RedirectDomains';

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

const view = (redirects: unknown[], extra: Record<string, unknown> = {}) => ({
  redirects, hostnames: ['new.example.com', 'www.new.example.com'], statuses: [{ domain: 'old.example.com', state: 'ready' }], ingress: 'agent', detail: '', server_id: 'beta',
  addresses: ['203.0.113.9', '2001:db8::9'], records: [], warnings: [], ...extra,
});

it('edits redirect domains with targets, status, keep path and DNS records', async () => {
  const requests: Array<{ url: string; init: RequestInit }> = [];
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
    requests.push({ url: String(input), init });
    if (init.method === 'PUT') return { ok: true, status: 200, json: async () => ({ ...view(JSON.parse(String(init.body)).redirects), note: 'Redirects: published to Caddy.' }) } as Response;
    return { ok: true, status: 200, json: async () => view([{ host: 'old.example.com', status: 301, keep_path: true }]) } as Response;
  }));
  render(<RedirectDomains appID="web" />);
  const first = await screen.findByRole('group', { name: 'Redirect domain 1' });
  expect(within(first).getByRole('textbox', { name: 'Redirect hostname 1' })).toHaveValue('old.example.com');
  expect(within(first).getByRole('combobox', { name: 'Redirect target 1' })).toHaveValue('');
  expect(within(first).getByText('new.example.com (primary)')).toBeInTheDocument();
  expect(within(first).getByRole('checkbox', { name: 'Keep path 1' })).toBeChecked();
  const records = within(first).getByRole('table', { name: 'DNS records for old.example.com' });
  expect(within(records).getByText('A')).toBeInTheDocument();
  expect(within(records).getByText('AAAA')).toBeInTheDocument();
  expect(within(records).getByText('2001:db8::9')).toBeInTheDocument();
  expect(within(first).getByText('HTTPS ready')).toBeInTheDocument();

  fireEvent.click(screen.getByRole('button', { name: 'Add redirect domain' }));
  const second = screen.getByRole('group', { name: 'Redirect domain 2' });
  fireEvent.change(within(second).getByRole('textbox', { name: 'Redirect hostname 2' }), { target: { value: 'Legacy.example.com' } });
  fireEvent.change(within(second).getByRole('combobox', { name: 'Redirect target 2' }), { target: { value: '__custom__' } });
  fireEvent.change(within(second).getByRole('textbox', { name: 'Custom redirect target 2' }), { target: { value: 'www.example.org' } });
  expect(within(second).getByText(/External hostname/)).toBeInTheDocument();
  fireEvent.change(within(second).getByRole('combobox', { name: 'Redirect status 2' }), { target: { value: '308' } });
  fireEvent.click(within(second).getByRole('checkbox', { name: 'Keep path 2' }));
  expect(within(second).getByText('Certificate issued on demand once DNS points to this server.')).toBeInTheDocument();

  fireEvent.click(screen.getByRole('button', { name: 'Save redirects' }));
  await waitFor(() => expect(requests.some(request => request.init.method === 'PUT')).toBe(true));
  const saved = requests.find(request => request.init.method === 'PUT')!;
  expect(saved.url).toBe('/api/control/apps/web/redirects');
  expect(JSON.parse(String(saved.init.body))).toEqual({ redirects: [
    { host: 'old.example.com', status: 301, keep_path: true },
    { host: 'legacy.example.com', target: 'www.example.org', status: 308, keep_path: false },
  ] });
  // The saved list comes back with the custom target kept as custom.
  expect(await screen.findByRole('textbox', { name: 'Custom redirect target 2' })).toHaveValue('www.example.org');
});

it('says when the server cannot publish redirects or its agent is too old', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, status: 200, json: async () => view([{ host: 'old.example.com', status: 301, keep_path: true }], { ingress: 'caddyfile', detail: 'Enable Caddy’s admin API (127.0.0.1:2019) so redirect domains are published.', statuses: [] }) }) as Response));
  render(<RedirectDomains appID="web" />);
  expect(await screen.findByText(/Enable Caddy’s admin API/)).toBeInTheDocument();
  expect(screen.getByText('Not served on this server (see above).')).toBeInTheDocument();
  cleanup();
  vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 404, json: async () => ({ error: 'update the hosting agent to manage redirect domains' }) }) as Response));
  render(<RedirectDomains appID="web" />);
  expect(await screen.findByText('Update the hosting agent to manage redirect domains.')).toBeInTheDocument();
});
