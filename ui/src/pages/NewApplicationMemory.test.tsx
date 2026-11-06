import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import type { RepositoryDetection } from '../lib/types';
import { NewApplicationPage } from './NewApplicationPage';

vi.mock('../state/WorkspaceContext', () => ({ useWorkspace: () => ({ servers: [{ id: 'small', name: 'Small', url: 'https://small.example.com' }, { id: 'big', name: 'Big', url: 'https://big.example.com' }], apps: [], status: {}, refresh: vi.fn(), loading: false }) }));
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const GiB = 2 ** 30;
const detection: RepositoryDetection = {
  repository: 'acme/shop', branch: 'main', default_branch: 'main', branches: ['main'], installation_id: 7, detected: true, mode: 'native', runtime: 'go', framework: 'lidza',
  processes: [{ name: 'web', command: 'bin/shop', web: true }], needs_postgres: false, needs_redis: false, health_path: '/healthz', reasons: ['go.mod requires github.com/agim/lidza: Līdza application'],
  build_memory: { mib: 2560, basis: ['Līdza build with a front end ~2.5 GiB'] },
};

function stubAPI(health: Array<{ server_id: string; memory_total: number; memory_available: number }>) {
  const respond = (body: unknown, status = 200) => ({ ok: status < 400, status, json: async () => body });
  vi.stubGlobal('fetch', vi.fn(async (url: string) => {
    if (url.startsWith('/api/control/github/repos')) return respond([{ full_name: 'acme/shop', default_branch: 'main', private: true, installation_id: 7 }]);
    if (url.startsWith('/api/control/repos/detect')) return respond(detection);
    if (url.startsWith('/api/control/server-health')) return respond({ servers: health });
    return respond({ error: `unexpected ${url}` }, 404);
  }));
}

const next = () => fireEvent.click(screen.getByRole('button', { name: /Continue/ }));

async function openServerStep() {
  render(<NewApplicationPage />);
  fireEvent.change(screen.getByLabelText(/Application name/), { target: { value: 'shop' } });
  next();
  fireEvent.click(await screen.findByRole('button', { name: /acme\/shop/ }));
  await screen.findByText(/Detected/);
  next();
  next();
}

it('blocks a server whose memory certainly cannot build the application', async () => {
  stubAPI([{ server_id: 'small', memory_total: 2 * GiB, memory_available: 1.5 * GiB }, { server_id: 'big', memory_total: 16 * GiB, memory_available: 12 * GiB }]);
  await openServerStep();
  const blocker = await screen.findByText(/Small has 2.0 GiB in total/);
  expect(blocker.closest('.build-memory-check')).toHaveClass('build-memory-blocker');
  expect(screen.getByText(/Estimate: Līdza build with a front end/)).toBeInTheDocument();
  next();
  // Still on the Server step.
  expect(screen.getByRole('button', { name: /Server/ })).toHaveAttribute('aria-current', 'step');
  fireEvent.click(screen.getByRole('radio', { name: /Big/ }));
  expect(await screen.findByText(/Building needs about 2.5 GiB of memory; Big has 12.0 GiB free/)).toBeInTheDocument();
  next();
  expect(screen.getByRole('button', { name: /Domain/ })).toHaveAttribute('aria-current', 'step');
});

it('warns when the server is likely too small but lets the operator continue', async () => {
  stubAPI([{ server_id: 'small', memory_total: 4 * GiB, memory_available: 2 * GiB }]);
  await openServerStep();
  const warning = await screen.findByText(/Small has 2.0 GiB free now \(4.0 GiB total, 512 MiB kept for running applications\)/);
  expect(warning.closest('.build-memory-check')).toHaveClass('build-memory-warning');
  next();
  expect(screen.getByRole('button', { name: /Domain/ })).toHaveAttribute('aria-current', 'step');
});
