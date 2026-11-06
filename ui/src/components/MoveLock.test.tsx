import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { AppSettingsPage } from '../pages/AppSettingsPage';
import { MigrationPlan } from './MigrationPlan';
import type { MoveJob } from '../lib/move';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const settings = { id: 'shop', server_id: 'alpha', repository: 'acme/shop', branch: 'main', domain: 'shop.example.com', backup_before_deploy: true, environment: [{ key: 'TITLE', kind: 'setting', value: 'Shop' }] };
const move = (state: string, extra: Partial<MoveJob> = {}): MoveJob => ({
  id: 'm1', app_id: 'shop', source_server: 'alpha', target_server: 'beta', target_server_name: 'Beta', target_runtime: 'native', runtime: 'rails', state,
  hostnames: ['shop.example.com'], addresses: [], dns: [], tls_ready: false, records: [], actions: ['abort'], switched: false, created: new Date(Date.now() - 125_000).toISOString(),
  steps: [
    { key: 'plan', title: 'Plan', status: 'done', log: [] },
    { key: 'data', title: 'Copy data', status: state === 'failed' ? 'failed' : 'running', started: new Date(Date.now() - 30_000).toISOString(), log: [] },
    { key: 'deploy', title: 'Deploy on destination', status: 'pending', log: [] },
  ], ...extra,
});

function serve(job: MoveJob | undefined) {
  const requests: Array<{ url: string; method: string }> = [];
  vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit = {}) => {
    requests.push({ url, method: init.method || 'GET' });
    if (url === '/api/control/apps/shop/move') return job ? { ok: true, status: 200, json: async () => job } : { ok: true, status: 204, json: async () => ({}) };
    if (url === '/api/control/apps/shop/settings') return { ok: true, status: 200, json: async () => settings };
    return { ok: true, status: 200, json: async () => ({ processes: [] }) };
  }));
  return requests;
}

it('shows the final sync on every settings tab and makes the forms read-only', async () => {
  const requests = serve(move('syncing', { locked: true, phase: 'locked' }));
  render(<AppSettingsPage appID="shop" />);
  const card = await screen.findByRole('region', { name: 'Move in progress' });
  expect(card).toHaveTextContent('Moving to Beta');
  expect(card).toHaveTextContent('Final sync');
  expect(card).toHaveTextContent('Final sync — app locked');
  expect(card).toHaveTextContent('Copy data');
  expect(card).toHaveTextContent(/2m 0\ds/);
  expect(card).toHaveTextContent('Settings are locked during the final sync. Finish or abort the move to change them.');
  expect(within(card).getByRole('link', { name: /Open move/ })).toHaveAttribute('href', '/console/applications?move=shop');

  const reason = 'shop is in the final sync of its move to Beta; finish or abort the move first.';
  const save = screen.getByRole('button', { name: 'Save changes' });
  expect(save).toBeDisabled();
  expect(save).toHaveAttribute('title', reason);
  expect(screen.getByRole('combobox', { name: /Branch/ })).toBeDisabled();
  expect(screen.getByRole('textbox', { name: /Application hostname/ })).toBeDisabled();
  expect(screen.getByRole('checkbox', { name: /Back up attached databases/ })).toBeDisabled();
  expect(screen.getByRole('textbox', { name: /Application hostname/ }).closest('fieldset.move-lock-fieldset')).toHaveAttribute('title', reason);

  fireEvent.mouseDown(screen.getByRole('tab', { name: 'Environment' }), { button: 0, ctrlKey: false });
  fireEvent.click(screen.getByRole('tab', { name: 'Environment' }));
  expect(await screen.findByDisplayValue('Shop')).toBeDisabled();
  expect(screen.getByRole('button', { name: /Add variable/ })).toBeDisabled();
  expect(screen.getByRole('region', { name: 'Move in progress' })).toBeInTheDocument();

  fireEvent.mouseDown(screen.getByRole('tab', { name: 'Processes' }), { button: 0, ctrlKey: false });
  fireEvent.click(screen.getByRole('tab', { name: 'Processes' }));
  expect(screen.getByRole('region', { name: 'Move in progress' })).toBeInTheDocument();
  // Reading processes stays available.
  expect(await screen.findByRole('button', { name: 'Refresh' })).toBeEnabled();
  expect(requests.some(request => request.method !== 'GET')).toBe(false);
});

it('keeps the application usable while the move copies in the background', async () => {
  const requests = serve(move('running', { locked: false, phase: 'live' }));
  render(<AppSettingsPage appID="shop" />);
  const card = await screen.findByRole('region', { name: 'Move in progress' });
  expect(card).toHaveTextContent('Preparing move (app stays live)');
  expect(card).toHaveTextContent('picked up by the final sync');
  expect(card).not.toHaveTextContent('Settings are locked');
  await waitFor(() => expect(screen.getByRole('combobox', { name: /Branch/ })).toBeEnabled());
  expect(screen.getByRole('textbox', { name: /Application hostname/ }).closest('fieldset.move-lock-fieldset')).not.toBeDisabled();
  expect(requests.some(request => request.method !== 'GET')).toBe(false);
});

it('explains a failed step with its cause, attempts and what to do', async () => {
  serve(move('failed', { locked: false, phase: 'live', error: 'Copy data failed on Beta: not enough free disk space', failure: { step: 'data', engine: 'PostgreSQL shop', server: 'beta', class: 'operator', code: 'disk_space', message: 'Copy data failed (PostgreSQL shop) on Beta: not enough free disk space', remediation: 'Free disk space on Beta (or add a disk), then retry the move.', attempt: 1, max_attempts: 6, time: new Date().toISOString() } }));
  render(<AppSettingsPage appID="shop" />);
  const card = await screen.findByRole('region', { name: 'Move in progress' });
  expect(card).toHaveTextContent('Failed');
  expect(card).toHaveTextContent('Current step: Copy data');
  expect(card).toHaveTextContent('Needs your action (disk_space) · attempt 1/6');
  expect(card).toHaveTextContent('What to do: Free disk space on Beta (or add a disk), then retry the move.');
  expect(card.querySelector('.spin')).toBeNull();
  expect(screen.getByRole('button', { name: 'Save changes' })).toBeEnabled();
});

it('keeps a failed final sync locking the settings', async () => {
  serve(move('cutover', { locked: true, phase: 'locked', error: 'final sync failed: disk full' }));
  render(<AppSettingsPage appID="shop" />);
  const card = await screen.findByRole('region', { name: 'Move in progress' });
  expect(card).toHaveTextContent('disk full');
  expect(card).toHaveTextContent('Final sync — app locked');
  expect(screen.getByRole('button', { name: 'Save changes' })).toBeDisabled();
});

it('unlocks the settings once the move finished or was aborted', async () => {
  for (const state of ['finished', 'aborted']) {
    serve(move(state));
    render(<AppSettingsPage appID="shop" />);
    const branch = await screen.findByRole('combobox', { name: /Branch/ });
    await waitFor(() => expect(fetch).toHaveBeenCalledWith('/api/control/apps/shop/move', expect.anything()));
    expect(branch).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Save changes' })).toBeEnabled();
    expect(screen.queryByRole('region', { name: 'Move in progress' })).toBeNull();
    cleanup();
  }
});

it('replaces the collapsed move summary with the live status while a move holds the application', async () => {
  serve(move('running'));
  const servers = [{ id: 'alpha', name: 'Alpha' }, { id: 'beta', name: 'Beta' }];
  const active = render(<MigrationPlan appID="shop" serverID="alpha" servers={servers} held />);
  const summary = active.container.querySelector('summary')!;
  await waitFor(() => expect(summary).toHaveTextContent('Moving to Beta'));
  expect(summary).toHaveTextContent('Copy data');
  expect(summary).toHaveTextContent(/2m 0\ds/);
  expect(summary.querySelector('.spin')).not.toBeNull();

  cleanup();
  serve(move('failed'));
  const { container, rerender } = render(<MigrationPlan appID="shop" serverID="alpha" servers={servers} held />);
  const failed = container.querySelector('summary')!;
  await waitFor(() => expect(failed).toHaveTextContent('Moving to Beta'));
  expect(failed).toHaveTextContent('Failed');
  expect(failed.querySelector('.spin')).toBeNull();
  expect(failed.querySelector('.move-status-failed')).not.toBeNull();

  // Without a move, or once the list no longer reports one, the summary is its title again.
  rerender(<MigrationPlan appID="shop" serverID="alpha" servers={servers} />);
  expect(container.querySelector('summary')).toHaveTextContent(/^Move to another server$/);
  cleanup();
  const requests = serve(undefined);
  const idle = render(<MigrationPlan appID="shop" serverID="alpha" servers={servers} />);
  expect(idle.container.querySelector('summary')).toHaveTextContent(/^Move to another server$/);
  expect(requests).toHaveLength(0);
});
