import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { AppSettingsPage } from './AppSettingsPage';

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it('allows scoped settings edits while protecting routing, runtime and managed values',async()=>{
  vi.stubGlobal('fetch',vi.fn(async()=>({ok:true,status:200,json:async()=>({id:'site',runtime:'astro',native:{id:'site',name:'site',services:[]},repository:'org/site',branch:'main',domain:'site.example.com',astro:{mode:'static',output_directory:'dist'},readonly_env_keys:['HOST_VALUE'],environment:[{key:'HOST_VALUE',kind:'setting',value:'fixed-host-value'},{key:'TITLE',kind:'setting',value:'editable-title'}],capabilities:{configure:true,environment:true,routing:false,resources:false,runtime_mode:false}})})));
  render(<AppSettingsPage appID="site"/>);
  expect(await screen.findByRole('combobox',{name:/Branch/})).toBeEnabled();
  expect(screen.getByRole('textbox',{name:/Application hostname/})).toBeDisabled();
  expect(screen.getByRole('combobox',{name:'Astro output'})).toBeDisabled();
  expect(screen.getByRole('textbox',{name:'Build output directory'})).toBeEnabled();
  expect(screen.getByRole('button',{name:'Save changes'})).toBeEnabled();
  fireEvent.mouseDown(screen.getByRole('tab',{name:'Environment'}),{button:0,ctrlKey:false});
  expect(screen.getByDisplayValue('fixed-host-value')).toBeDisabled();
  expect(screen.getByDisplayValue('editable-title')).toBeEnabled();
  fireEvent.mouseDown(screen.getByRole('tab',{name:'Data & storage'}),{button:0,ctrlKey:false});
  // A native application shows the data services on its server instead of
  // the container database and cache controls.
  expect(await screen.findByText(/No database or cache was provisioned on the server/)).toBeInTheDocument();
  expect(screen.queryByRole('button',{name:'Manage database'})).not.toBeInTheDocument();
  expect(screen.queryByRole('button',{name:'Manage cache'})).not.toBeInTheDocument();
  expect(screen.queryByText(/Valkey|container/i)).not.toBeInTheDocument();
});

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

it('saves profile selection and preserves it when switching to environment', async () => {
  const requests: RequestInit[] = [];
  vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init: RequestInit = {}) => {
    requests.push(init);
    return { ok: true, status: 200, json: async () => ({ id: 'portal', branch: 'main', domain: 'portal.example.com', stack_profiles_supported: true, runtime_profile: 'minimum', runtime_deployed: true, environment: [] }) } as Response;
  }));
  render(<AppSettingsPage appID="portal" />);
  const profile = await screen.findByRole('combobox', { name: 'Runtime profile' });
  expect(profile).toHaveValue('minimum');
  expect(screen.queryByRole('option', { name: /Single container/ })).not.toBeInTheDocument();
  fireEvent.change(profile, { target: { value: 'standard' } });
  fireEvent.click(screen.getByRole('tab', { name: 'Environment' }));
  fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(requests.some(request => request.method === 'PATCH')).toBe(true));
  expect(JSON.parse(String(requests.find(item => item.method === 'PATCH')?.body))).toMatchObject({ runtime_profile: 'standard', domain: 'portal.example.com', branch: 'main' });
});

it('ends General with a Danger zone that stops, starts and deletes the application', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, status: 200, json: async () => ({ id: 'ledger', server_id: 'host', runtime: 'rails', repository: 'acme/ledger', branch: 'main', domain: 'ledger.example.com', native: { id: 'ledger', name: 'Ledger', services: ['ledger-server.service'] }, stopped: true, environment: [], capabilities: { configure: true, environment: true, deploy: true, reload: false, rollback: true, processes: true, logs: true, remove: true, lifecycle: true, adopted: true } }) })));
  const { container } = render(<AppSettingsPage appID="ledger" />);
  const zone = await screen.findByText('Danger zone');
  expect(container.querySelector('[data-danger-zone="ledger"]')).toContainElement(zone);
  expect(screen.getByRole('button', { name: 'Start application' })).toBeEnabled();
  expect(screen.getByRole('button', { name: 'Delete application' })).toBeEnabled();
  // The zone sits after the settings form, so its dialogs never submit it.
  expect(container.querySelector('form#settings-form [data-danger-zone]')).toBeNull();
  fireEvent.mouseDown(screen.getByRole('tab', { name: 'Environment' }), { button: 0, ctrlKey: false });
  await waitFor(() => expect(screen.queryByText('Danger zone')).not.toBeInTheDocument());
});
