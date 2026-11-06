import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { MigrationPlan } from './MigrationPlan';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const plan = {
  app_id: 'events', source_server: 'alpha', target_server: 'beta', source_runtime: 'container', runtime: 'go', target_runtime: 'native', runtime_choices: ['native', 'container'],
  repository: 'acme/events', branch: 'main', hostnames: ['events.example.com'], environment_keys: ['CLICKHOUSE_URL', 'DATABASE_URL'], copied_keys: [], destination_keys: ['CLICKHOUSE_URL', 'DATABASE_URL'],
  databases: [
    { id: 'events', engine: 'postgresql', mode: 'local', tables: 12, size_bytes: 4 * 2 ** 20, action: 'copy' },
    { id: 'telemetry', engine: 'clickhouse', mode: 'container', tables: 5, size_bytes: 300 * 2 ** 20, action: 'copy', note: 'ClickHouse 26.8.15.10, 120000 rows' },
  ],
  redis: false, volumes: [], addresses: ['203.0.113.9'], warnings: [], notes: [], blockers: [],
};

function respond(status: number, body: unknown) {
  return Promise.resolve({ ok: status < 300, status, json: async () => body });
}

it('lists the ClickHouse database with its size and tables as copied', async () => {
  vi.stubGlobal('fetch', vi.fn((url: string, init: RequestInit = {}) => {
    if (url === '/api/control/apps/events/move' && !init.method) return respond(404, { error: 'none' });
    if (url.startsWith('/api/control/apps/events/move-plan?')) return respond(200, plan);
    return respond(500, { error: 'unexpected' });
  }));
  const { container } = render(<MigrationPlan appID="events" serverID="alpha" servers={[{ id: 'alpha', name: 'Alpha' }, { id: 'beta', name: 'Beta' }]} />);
  const details = container.querySelector('details')!;
  details.open = true;
  fireEvent(details, new Event('toggle'));
  fireEvent.click(await screen.findByRole('button', { name: 'Review move' }));
  const item = await screen.findByText((_, element) => element?.tagName === 'LI' && (element.textContent || '').startsWith('ClickHouse telemetry'));
  expect(item).toHaveTextContent('ClickHouse telemetry: 300.0 MiB, 5 tables · copied · ClickHouse 26.8.15.10, 120000 rows');
  expect(screen.getByRole('button', { name: 'Start move' })).toBeEnabled();
});
