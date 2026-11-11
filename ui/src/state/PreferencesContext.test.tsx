import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { AppShell } from '../components/AppShell';
import { ApplicationsPage } from '../pages/ApplicationsPage';
import { DisplayPreferences } from './DisplayPreferences';
import { PreferencesProvider } from './PreferencesContext';

const fixture = vi.hoisted(() => ({ apps: [{ id:'portal', repository:'org/portal', branch:'main', domain:'portal.example.com', server_id:'local' }], error:'' }));
vi.mock('./WorkspaceContext', () => ({ useWorkspace: () => ({ apps:fixture.apps, servers:[{id:'local'}], deployments:[], status:{roles:['admin']}, loading:false, refreshing:false, unavailableServers:[], error:fixture.error, latestDeployment:()=>undefined, refresh:vi.fn() }) }));
vi.mock('../components/ToastProvider', () => ({ useToast:()=>vi.fn() }));
afterEach(() => { cleanup(); fixture.error=''; });

it('hydrates before showing the list and retains the search and DOM identity through inventory outages/recovery', async () => {
  let resolve!: (v:{subject:string;values:Record<string,unknown>})=>void;
  let values: Record<string,unknown> = { 'deploy.apps.query':'portal' };
  const store = new DisplayPreferences({ read:()=>new Promise(done=>{resolve=done;}), write:async (subject,patch)=>({subject,values:{...values,...patch}}) });
  const tree = <PreferencesProvider store={store}><AppShell route={{view:'index',collection:'applications'}}><ApplicationsPage/></AppShell></PreferencesProvider>;
  const page=render(tree);
  expect(screen.queryByRole('searchbox')).toBeNull();
  await act(async()=>{resolve({subject:'alice',values});});
  const search=await screen.findByRole('searchbox'), list=document.querySelector('[data-resource-list]');
  expect(search).toHaveValue('portal'); expect(list).not.toBeNull();
  fireEvent.change(search,{target:{value:'port'}});
  fixture.error='Inventory temporarily unavailable';
  page.rerender(<PreferencesProvider store={store}><AppShell route={{view:'index',collection:'applications'}}><ApplicationsPage/></AppShell></PreferencesProvider>);
  expect(screen.getByRole('searchbox')).toBe(search); expect(search).toHaveValue('port'); expect(document.querySelector('[data-resource-list]')).toBe(list);
  fixture.error=''; fixture.apps=[...fixture.apps];
  page.rerender(<PreferencesProvider store={store}><AppShell route={{view:'index',collection:'applications'}}><ApplicationsPage/></AppShell></PreferencesProvider>);
  expect(document.querySelector('[data-resource-list]')).toBe(list); expect(search).toHaveValue('port');
  await act(async()=>{await store.flush();});
});

it('retries a failed preference connection on browser reconnection while preserving the chosen filter', async () => {
  let offline=true;
  const store=new DisplayPreferences({read:async()=>{if(offline)throw new Error('offline');return {subject:'alice',values:{}};},write:async(subject,values)=>({subject,values})});
  render(<PreferencesProvider store={store}><AppShell route={{view:'index',collection:'applications'}}><ApplicationsPage/></AppShell></PreferencesProvider>);
  await screen.findByText(/Display choices could not sync/);
  fireEvent.change(screen.getByRole('searchbox'),{target:{value:'port'}});
  offline=false;
  await act(async()=>{fireEvent(window,new Event('online'));});
  await waitFor(()=>expect(screen.queryByText(/Display choices could not sync/)).toBeNull());
  expect(screen.getByRole('searchbox')).toHaveValue('port');
});
