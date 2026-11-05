import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import type { RepositoryDetection } from '../lib/types';
import { NewApplicationPage } from './NewApplicationPage';

vi.mock('../state/WorkspaceContext', () => ({ useWorkspace: () => ({ servers: [{ id: 'server', name: 'Test server', url: 'https://agent.example.com' }], apps: [], status: {}, refresh: vi.fn(), loading: false }) }));
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const detection: RepositoryDetection = {
  repository: 'org/suite', branch: 'main', default_branch: 'main', branches: ['main'], detected: true, mode: 'container', needs_postgres: false, needs_redis: false,
  compose_file: 'compose.yaml', services: [{ name: 'synapse', image: 'ghcr.io/element-hq/synapse', role: 'image' }, { name: 'postfix', image: 'boky/postfix', role: 'image' }],
  stack_manifest_missing: true,
  reasons: ['compose.yaml defines 2 services (synapse, postfix) using third-party images: container', 'Deploying these services needs a deploy/lidza.stack.json stack manifest in the repository'],
};

it('explains that a compose repository needs a stack manifest before it can deploy as containers', async () => {
  const respond = (body: unknown, status = 200) => ({ ok: status < 400, status, json: async () => body });
  vi.stubGlobal('fetch', vi.fn(async (url: string) => {
    if (url.startsWith('/api/control/github/repos')) return respond([]);
    if (url.startsWith('/api/control/repos/detect')) return respond(detection);
    if (url.startsWith('/api/control/application-defaults')) return respond({ runtimes: {} });
    return respond({ error: `unexpected ${url}` }, 404);
  }));
  const next = () => fireEvent.click(screen.getByRole('button', { name: /Continue/ }));
  render(<NewApplicationPage />);
  fireEvent.change(screen.getByLabelText(/Application name/), { target: { value: 'suite' } });
  next();
  fireEvent.change(screen.getByLabelText(/GitHub repository/), { target: { value: 'org/suite' } });
  fireEvent.change(screen.getByLabelText(/Branch/), { target: { value: 'main' } });
  fireEvent.click(screen.getByRole('button', { name: /Detect/ }));
  await screen.findByText(/^Detected/);
  next();
  expect(screen.getByText(/stack manifest in the repository/)).toBeInTheDocument();
  expect(screen.getByRole('radio', { name: /Containers/ })).toBeChecked();
  next();
  expect((await screen.findByRole('alert')).textContent).toContain('deploy/lidza.stack.json');
  fireEvent.click(screen.getByRole('radio', { name: /Native/ }));
  expect(screen.queryByText(/needs a deploy\/lidza.stack.json manifest/)).not.toBeInTheDocument();
});
