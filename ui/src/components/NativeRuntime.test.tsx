import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { ApplicationProcesses } from './ApplicationProcesses';
import { NativeDataServices } from './NativeDataServices';
import { ServerResources } from './ServerResources';
import type { ServerHealth } from '../lib/types';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const reply = (data: unknown, status = 200) => ({ ok: status < 400, status, json: async () => data });

it('shows native service usage and restarts through the service route with confirmation', async () => {
  const fetch = vi.fn((url: string, init?: RequestInit) => {
    if (init?.method === 'POST') return Promise.resolve(reply({ status: 'requested' }));
    return Promise.resolve(reply({ processes: [
      { id: 'web', name: 'lidza-app-shop-web.service', kind: 'Service', state: 'active', startup: 'enabled', runtime: 'native', memory_bytes: 104857600, memory_limit_bytes: 536870912, cpu_percent: 12.5, tasks: 7, restarts: 1, restart_path: 'control/native-apps/shop/services/lidza-app-shop-web.service/restart', confirm: 'shop' },
      { id: 'jobs', name: 'lidza-app-shop-jobs.service', kind: 'Service · preserved', state: 'inactive', startup: 'disabled', runtime: 'native', preserved: true, restart_path: 'control/native-apps/shop/services/lidza-app-shop-jobs.service/restart', confirm: 'shop' },
      { id: 'mail', name: 'lidza-app-shop-mail.service', kind: 'Service', state: 'active', runtime: 'native' },
    ] }));
  });
  vi.stubGlobal('fetch', fetch);
  render(<ApplicationProcesses appID="shop" />);
  expect(await screen.findByText(/CPU 12.5% · Memory 100.0 MiB \/ 512.0 MiB/)).toBeInTheDocument();
  expect(screen.getByText(/7 tasks · 1 restart/)).toBeInTheDocument();
  // Deploys leave a preserved service alone; the operator can restart it.
  expect(screen.getByText(/Deploys leave this service as it is/)).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Restart lidza-app-shop-jobs.service' })).toBeInTheDocument();
  // Without service control there is no restart route and no button.
  expect(screen.queryByRole('button', { name: 'Restart lidza-app-shop-mail.service' })).not.toBeInTheDocument();
  expect(document.body.textContent).not.toMatch(/container/i);
  fireEvent.click(screen.getByRole('button', { name: 'Restart lidza-app-shop-web.service' }));
  fireEvent.click(await screen.findByRole('button', { name: 'Restart service' }));
  await vi.waitFor(() => expect(fetch.mock.calls.some(([url, init]) => String(url).includes('control/native-apps/shop/services/lidza-app-shop-web.service/restart') && init?.method === 'POST' && String(init.body).includes('"confirm":"shop"'))).toBe(true));
});

it('shows the data services provisioned on the server for a native application', async () => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(reply({ app_id: 'shop',
    postgres: { engine: 'postgresql', version: 17, database: 'shop', ready: true, size_bytes: 2097152, backups: 3, resource: 'shop-db' },
    redis: { engine: 'redis', unit: 'lidza-redis-shop.service', active: 'active', used_memory_bytes: 1048576, max_memory_bytes: 67108864, persistence: 'rdb' } })));
  render(<NativeDataServices appID="shop" />);
  expect(await screen.findByText('PostgreSQL')).toBeInTheDocument();
  expect(screen.getByText(/Database shop · PostgreSQL 17 on the server/)).toBeInTheDocument();
  expect(screen.getByText(/3 backups/)).toBeInTheDocument();
  expect(screen.getByText('Redis')).toBeInTheDocument();
  expect(screen.getByText(/Service lidza-redis-shop.service · active/)).toBeInTheDocument();
  expect(document.body.textContent).not.toMatch(/container/i);
});

it('explains a native application without provisioned data services', async () => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(reply({ error: 'no data services are provisioned for this application' }, 409)));
  render(<NativeDataServices appID="legacy" />);
  expect(await screen.findByText(/No database or cache was provisioned on the server/)).toBeInTheDocument();
});

it('shows the native section without Docker on a native-only server', () => {
  const health: ServerHealth = { server_id: 'host', checked: '2026-11-05T10:00:00Z', native: { applications: 2, units: 3, active: 2, inactive: 1, failed_units: ['lidza-app-shop-jobs.service'], failed_host_units: ['nginx.service'], journal_bytes: 2097152, releases: [{ app_id: 'shop', path: '/srv/lidza/apps/shop/releases', bytes: 52428800, releases: 3, measured: '2026-11-05T10:00:00Z' }] } };
  render(<ServerResources health={health} loading={false} />);
  const native = screen.getByLabelText('Native services');
  expect(within(native).getByText(/3 services · 2 active · 2 failed/)).toBeInTheDocument();
  expect(within(native).getByText('nginx.service')).toBeInTheDocument();
  expect(within(native).getByText(/shop releases/)).toBeInTheDocument();
  expect(screen.queryByLabelText('Containers')).not.toBeInTheDocument();
  cleanup();
  render(<ServerResources health={{ ...health, docker: { version: '27.3.1', containers: 4, running: 3 } }} loading={false} />);
  expect(within(screen.getByLabelText('Containers')).getByText('3 running / 4 total')).toBeInTheDocument();
});

it('describes removing retained native data services without container wording', async () => {
  const { removalDescription } = await import('./RetainedResources');
  const text = removalDescription({ id: 'r1', app_id: 'shop', server_id: 'host', components: [{ kind: 'database', name: 'app_shop', host: true }, { kind: 'redis', name: 'lidza-redis@shop.service', host: true }] });
  expect(text).toContain('Deletes these data services on host: PostgreSQL app_shop, Redis lidza-redis@shop.service');
  expect(text).not.toMatch(/container|volume/i);
  expect(removalDescription({ id: 'r2', app_id: 'api', server_id: 'host', components: [{ kind: 'files', name: 'api-files' }] })).toContain('deletes these volumes from host: api-files');
});
