import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';

const api = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn() }));
vi.mock('../lib/api', () => api);
vi.mock('./ToastProvider', () => ({ useToast: () => vi.fn() }));

import { AppAccessLog } from './AppAccessLog';

afterEach(() => { cleanup(); vi.clearAllMocks(); });

const name = 'Write an access log for this application';
const on = { supported: true, enabled: true, active: true, env_set: true, path: '/var/log/lidza/apps/shop/access.log', directory: '/var/log/lidza/apps/shop', env: 'LIDZA_ACCESS_LOG', account: 'app-shop', group: 'app-shop', hosts: ['shop.example.com', 'www.shop.example.com'], files: 2, bytes: 2048, caddy_version: 'v2.10.2', checked: new Date().toISOString(), restart_note: 'Running processes read LIDZA_ACCESS_LOG when they start: after the next restart or deploy.' };

it('shows the access log file, who reads it and the environment variable, and switches it off at once', async () => {
  api.get.mockResolvedValue(on);
  api.put.mockResolvedValue({ ...on, enabled: false, active: false, env_set: false, files: 0, bytes: 0 });
  render(<AppAccessLog appID="shop" />);
  const toggle = await screen.findByRole('switch', { name });
  expect(api.get).toHaveBeenCalledWith('control/apps/shop/access-log');
  expect(toggle).toHaveAttribute('aria-checked', 'true');
  expect(screen.getByText('Writing')).toBeInTheDocument();
  expect(screen.getByText('/var/log/lidza/apps/shop/access.log')).toBeInTheDocument();
  expect(screen.getByText('LIDZA_ACCESS_LOG')).toBeInTheDocument();
  expect(screen.getByText(/after the next restart or deploy/)).toBeInTheDocument();
  expect(screen.getByText('shop.example.com, www.shop.example.com')).toBeInTheDocument();
  expect(screen.getByText(/up to 14 days · 2 files/)).toBeInTheDocument();
  expect(screen.getByText(/The system journal is unchanged/)).toBeInTheDocument();
  fireEvent.click(toggle);
  await waitFor(() => expect(api.put).toHaveBeenCalledWith('control/apps/shop/access-log', { enabled: false }));
  await waitFor(() => expect(screen.getByRole('switch', { name })).toHaveAttribute('aria-checked', 'false'));
  expect(screen.getByText('Off')).toBeInTheDocument();
  expect(screen.queryByText('/var/log/lidza/apps/shop/access.log')).toBeNull();
});

it('reports why a log is not written and says when an application has none', async () => {
  api.get.mockResolvedValue({ ...on, active: false, env_set: false, error: 'Caddy v2.8.4 is too old for per-application access logs: they need Caddy 2.9 or later.' });
  render(<AppAccessLog appID="shop" />);
  expect(await screen.findByText(/they need Caddy 2.9 or later/)).toBeInTheDocument();
  expect(screen.getByText('Not writing')).toBeInTheDocument();
  expect(screen.getByText(/is set once Caddy writes the file/)).toBeInTheDocument();
  cleanup();

  api.get.mockResolvedValue({ supported: false, enabled: false, active: false, env_set: false, path: '', directory: '', env: 'LIDZA_ACCESS_LOG', files: 0, bytes: 0, reason: 'Container applications are not covered yet.' });
  render(<AppAccessLog appID="web" />);
  expect(await screen.findByText('Container applications are not covered yet.')).toBeInTheDocument();
  expect(screen.queryByRole('switch')).toBeNull();
});

it('keeps the switch locked while the application moves', async () => {
  api.get.mockResolvedValue(on);
  render(<AppAccessLog appID="shop" disabled />);
  expect(await screen.findByRole('switch', { name })).toBeDisabled();
});
