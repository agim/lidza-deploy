import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { AppDataTab, AppDomainsTab, AppEnvironmentTab, AppSettingsTab } from './AppSettingsPage';

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

const astro = { id: 'site', runtime: 'astro', native: { id: 'site', name: 'site', services: [] }, repository: 'org/site', branch: 'main', domain: 'site.example.com', astro: { mode: 'static', output_directory: 'dist' }, readonly_env_keys: ['HOST_VALUE'], environment: [{ key: 'HOST_VALUE', kind: 'setting', value: 'fixed-host-value' }, { key: 'TITLE', kind: 'setting', value: 'editable-title' }], capabilities: { configure: true, environment: true, routing: false, resources: false, runtime_mode: false } };
const reply = (body: unknown) => vi.fn(async () => ({ ok: true, status: 200, json: async () => body }));

it('allows scoped settings edits while protecting routing, runtime and managed values', async () => {
  vi.stubGlobal('fetch', reply(astro));
  const settings = render(<AppSettingsTab appID="site" />);
  expect(await screen.findByRole('combobox', { name: /Branch/ })).toBeEnabled();
  expect(screen.getByRole('combobox', { name: 'Astro output' })).toBeDisabled();
  expect(screen.getByRole('textbox', { name: 'Build output directory' })).toBeEnabled();
  expect(screen.getByRole('button', { name: 'Save changes' })).toBeEnabled();
  settings.unmount();
  const domains = render(<AppDomainsTab appID="site" />);
  expect(await screen.findByRole('textbox', { name: /Application hostname/ })).toBeDisabled();
  domains.unmount();
  const environment = render(<AppEnvironmentTab appID="site" />);
  expect(await screen.findByDisplayValue('fixed-host-value')).toBeDisabled();
  expect(screen.getByDisplayValue('editable-title')).toBeEnabled();
  environment.unmount();
  // A native application shows the data services on its server instead of
  // the container database and cache controls.
  render(<AppDataTab appID="site" />);
  expect(await screen.findByText(/No database or cache was provisioned on the server/)).toBeInTheDocument();
  expect(screen.queryByText(/Valkey|container/i)).not.toBeInTheDocument();
});

describe('saving one tab', () => {
  it('preserves routing and safety values when saving the environment', async () => {
    const requests: RequestInit[] = [];
    vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init: RequestInit = {}) => {
      requests.push(init);
      return { ok: true, status: 200, json: async () => ({ id: 'portal', server_id: 'local', repository: 'acme/portal', branch: 'main', domain: 'portal.example.com', customer_hosts: true, backup_before_deploy: true, environment: [] }) } as Response;
    }));
    render(<AppEnvironmentTab appID="portal" />);
    fireEvent.click(await screen.findByRole('button', { name: 'Save variables' }));
    await waitFor(() => expect(requests.some(request => request.method === 'PATCH')).toBe(true));
    const body = JSON.parse(String(requests.find(item => item.method === 'PATCH')?.body));
    expect(body).toMatchObject({ branch: 'main', domain: 'portal.example.com', customer_hosts: true, backup_before_deploy: true, env_changes: {} });
  });

  it('saves the runtime profile from the Settings tab with the other values unchanged', async () => {
    const requests: RequestInit[] = [];
    vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init: RequestInit = {}) => {
      requests.push(init);
      return { ok: true, status: 200, json: async () => ({ id: 'portal', branch: 'main', domain: 'portal.example.com', stack_profiles_supported: true, runtime_profile: 'minimum', runtime_deployed: true, environment: [] }) } as Response;
    }));
    render(<AppSettingsTab appID="portal" />);
    const profile = await screen.findByRole('combobox', { name: 'Runtime profile' });
    expect(profile).toHaveValue('minimum');
    expect(screen.queryByRole('option', { name: /Single container/ })).not.toBeInTheDocument();
    fireEvent.change(profile, { target: { value: 'standard' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(requests.some(request => request.method === 'PATCH')).toBe(true));
    expect(JSON.parse(String(requests.find(item => item.method === 'PATCH')?.body))).toMatchObject({ runtime_profile: 'standard', domain: 'portal.example.com', branch: 'main' });
  });
});

it('ends the Settings tab with a Danger zone that starts and deletes the application', async () => {
  vi.stubGlobal('fetch', reply({ id: 'ledger', server_id: 'host', runtime: 'rails', repository: 'acme/ledger', branch: 'main', domain: 'ledger.example.com', native: { id: 'ledger', name: 'Ledger', services: ['ledger-server.service'] }, stopped: true, environment: [], capabilities: { configure: true, environment: true, deploy: true, reload: false, rollback: true, processes: true, logs: true, remove: true, lifecycle: true, adopted: true } }));
  const { container } = render(<AppSettingsTab appID="ledger" />);
  const zone = await screen.findByText('Danger zone');
  expect(container.querySelector('[data-danger-zone="ledger"]')).toContainElement(zone);
  expect(screen.getByRole('button', { name: 'Start application' })).toBeEnabled();
  expect(screen.getByRole('button', { name: 'Delete application' })).toBeEnabled();
  // The zone sits after the settings form, so its dialogs never submit it.
  expect(container.querySelector('form#settings-form [data-danger-zone]')).toBeNull();
  expect(container.lastElementChild?.lastElementChild).toHaveAttribute('data-danger-zone', 'ledger');
});

describe('applying a save', () => {
  const effects = { env: 'restart', build_env_prefixes: ['PUBLIC_'], fields: { branch: 'deploy', build: 'deploy', astro: 'deploy', runtime_profile: 'deploy', toolchains: 'deploy', domain: 'restart', additional_domains: 'immediate', customer_hosts: 'immediate', backup_before_deploy: 'immediate', env_kind: 'immediate' } };
  const portal = { id: 'portal', server_id: 'local', repository: 'acme/portal', branch: 'main', domain: 'portal.example.com', backup_before_deploy: true, apply_effects: effects, environment: [{ key: 'API_TOKEN', kind: 'credential', length: 12 }] };
  const agent = (apply: unknown, deployment?: unknown) => vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
    if (init.method === 'PATCH') return { ok: true, status: 200, json: async () => ({ id: 'portal', apply }) } as Response;
    if (String(input).includes('control/deployments/')) return { ok: true, status: 200, json: async () => deployment } as Response;
    return { ok: true, status: 200, json: async () => portal } as Response;
  });

  it('reads Save and deploy for a branch change and links to the deployment it starts', async () => {
    vi.stubGlobal('fetch', agent({ applied: 'deploy', deployment_id: 'deploy-1', deployment: { id: 'deploy-1', app_id: 'portal', status: 'queued', created: '' }, message: 'Settings saved. Deploying them.' }));
    render(<AppSettingsTab appID="portal" />);
    const branch = await screen.findByRole('combobox', { name: /Branch/ });
    expect(screen.getByRole('button', { name: 'Save changes' })).toBeEnabled();
    fireEvent.change(branch, { target: { value: 'next' } });
    expect(screen.getByRole('button', { name: 'Save and deploy' })).toBeEnabled();
    fireEvent.change(branch, { target: { value: 'main' } });
    expect(screen.getByRole('button', { name: 'Save changes' })).toBeEnabled();
    fireEvent.change(branch, { target: { value: 'next' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save and deploy' }));
    expect(await screen.findByText('Settings saved. Deploying…')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'View deployment' })).toHaveAttribute('href', expect.stringContaining('/deployments/deploy-1'));
  });

  it('keeps Save variables for a runtime variable, then reports the restart until it finishes', async () => {
    vi.stubGlobal('fetch', agent({ applied: 'restart', deployment_id: 'reload-1', deployment: { id: 'reload-1', app_id: 'portal', status: 'queued', kind: 'reload', created: '' }, message: 'Settings saved. Restarting the application with them.' }, { id: 'reload-1', app_id: 'portal', status: 'live', kind: 'reload', created: '' }));
    render(<AppEnvironmentTab appID="portal" />);
    fireEvent.click(await screen.findByRole('button', { name: 'Add variable' }));
    const names = screen.getAllByPlaceholderText('VARIABLE_NAME');
    fireEvent.change(names[names.length - 1], { target: { value: 'PUBLIC_API_URL' } });
    expect(screen.getByRole('button', { name: 'Save and deploy' })).toBeEnabled();
    fireEvent.change(names[names.length - 1], { target: { value: 'FEATURE_FLAG' } });
    fireEvent.change(screen.getAllByPlaceholderText('Enter value')[0], { target: { value: 'on' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save variables' }));
    expect(await screen.findByText('Settings saved. Restarting…')).toBeInTheDocument();
    expect(await screen.findByText('Restarted.', {}, { timeout: 4000 })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'View restart' })).toHaveAttribute('href', expect.stringContaining('/deployments/reload-1'));
  });

  it('says a restart is required when the panel cannot restart the application', async () => {
    vi.stubGlobal('fetch', agent({ applied: 'saved_restart_required', message: 'Settings saved. Restart the application to apply them; the panel cannot restart it: Service control is off for this server.' }));
    render(<AppEnvironmentTab appID="portal" />);
    fireEvent.click(await screen.findByRole('button', { name: 'Add variable' }));
    const names = screen.getAllByPlaceholderText('VARIABLE_NAME');
    fireEvent.change(names[names.length - 1], { target: { value: 'FEATURE_FLAG' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save variables' }));
    expect(await screen.findByText(/the panel cannot restart it: Service control is off/)).toBeInTheDocument();
    expect(screen.queryByText(/next deployment/)).not.toBeInTheDocument();
  });
});
