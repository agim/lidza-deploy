import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { AppDatabasesBeforeDeploy, beforeDeployLabel } from './BeforeDeployBackup';

const api = vi.hoisted(() => ({ get: vi.fn(), patch: vi.fn(), post: vi.fn() }));
vi.mock('../lib/api', () => api);
vi.mock('./ToastProvider', () => ({ useToast: () => () => {} }));

afterEach(() => { cleanup(); vi.clearAllMocks(); });

const databases = [
  { app_id: 'shop', server_id: 'server-a', ready: true, primary: true, before_deploy: { mode: 'always' }, attachments: [{ app_id: 'shop', env_key: 'DATABASE_URL' }] },
  { app_id: 'shop-events', server_id: 'server-a', ready: true, primary: false, before_deploy: { mode: 'reuse', max_age_hours: 24 }, attachments: [{ app_id: 'shop', env_key: 'EVENTS_DATABASE_URL' }] },
  { app_id: 'other', server_id: 'server-a', ready: true, primary: true, attachments: [{ app_id: 'other', env_key: 'DATABASE_URL' }] },
];

it('lists the application’s databases, disables scheduled-only for the primary and saves a secondary’s mode', async () => {
  api.get.mockResolvedValue({ databases });
  api.patch.mockResolvedValue({});
  render(<AppDatabasesBeforeDeploy appID="shop" />);
  const primary = within(await screen.findByRole('group', { name: 'Before deploys for shop' }));
  expect(screen.queryByRole('group', { name: 'Before deploys for other' })).not.toBeInTheDocument();
  expect(primary.getByRole('option', { name: /Scheduled backups only/ })).toBeDisabled();
  expect(primary.getByRole('option', { name: 'Reuse a backup newer than…' })).toBeEnabled();
  expect(primary.getByRole('combobox', { name: /Before deploys/ })).toHaveValue('always');

  const secondary = within(screen.getByRole('group', { name: 'Before deploys for shop-events' }));
  expect(secondary.getByRole('option', { name: 'Scheduled backups only' })).toBeEnabled();
  expect(secondary.getByRole('combobox', { name: /Before deploys/ })).toHaveValue('reuse');
  const age = secondary.getByRole('spinbutton', { name: /Maximum age/ });
  expect(age).toHaveValue(24);
  fireEvent.change(age, { target: { value: '48' } });
  fireEvent.click(secondary.getByRole('button', { name: 'Save before-deploy policy' }));
  await waitFor(() => expect(api.patch).toHaveBeenCalledWith('control/servers/server-a/databases/shop-events/backups/before-deploy', { mode: 'reuse', max_age_hours: 48 }));
  // The control reloads the list after saving and is usable again.
  await waitFor(() => expect(secondary.getByRole('button', { name: 'Save before-deploy policy' })).toBeEnabled());

  fireEvent.change(secondary.getByRole('combobox', { name: /Before deploys/ }), { target: { value: 'never' } });
  expect(secondary.queryByRole('spinbutton', { name: /Maximum age/ })).not.toBeInTheDocument();
  fireEvent.click(secondary.getByRole('button', { name: 'Save before-deploy policy' }));
  await waitFor(() => expect(api.patch).toHaveBeenLastCalledWith('control/servers/server-a/databases/shop-events/backups/before-deploy', { mode: 'never' }));
  expect(api.patch).toHaveBeenCalledTimes(2);
  // Saving reloads the list, so the page shows what the agent now applies.
  await waitFor(() => expect(api.get).toHaveBeenCalledTimes(3));
});

it('refuses an out-of-range maximum age locally and names the policies', async () => {
  api.get.mockResolvedValue({ databases });
  render(<AppDatabasesBeforeDeploy appID="shop" />);
  const secondary = within(await screen.findByRole('group', { name: 'Before deploys for shop-events' }));
  fireEvent.change(secondary.getByRole('spinbutton', { name: /Maximum age/ }), { target: { value: '0' } });
  fireEvent.click(secondary.getByRole('button', { name: 'Save before-deploy policy' }));
  expect(api.patch).not.toHaveBeenCalled();
  expect(beforeDeployLabel({ mode: 'reuse', max_age_hours: 12 })).toBe('Reuse a backup newer than 12 h');
  expect(beforeDeployLabel({ mode: 'never' })).toBe('Scheduled backups only');
  expect(beforeDeployLabel(undefined)).toBe('Always a fresh backup');
});
