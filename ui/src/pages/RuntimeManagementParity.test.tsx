import {cleanup,fireEvent,render,screen} from '@testing-library/react';
import {afterEach,it,expect,vi} from 'vitest';
import {AppEnvironmentTab,AppSettingsTab} from './AppSettingsPage';
import {ApplicationsPage} from './ApplicationsPage';
import {ApplicationDetailPage} from './ApplicationDetailPage';
vi.mock('../state/WorkspaceContext',()=>({useWorkspace:()=>({apps:[{id:'go',runtime:'go',repository:'org/go',branch:'main',domain:'go.example.com',server_id:'source'},{id:'rails',runtime:'rails',repository:'org/rails',branch:'main',domain:'rails.example.com',server_id:'host',native:{id:'rails',name:'rails',services:['rails-server.service']}},{id:'astro',runtime:'astro',repository:'org/site',branch:'main',domain:'example.com',server_id:'host',native:{id:'astro',name:'site',services:[]}}],servers:[{id:'source',name:'Source'},{id:'host',name:'Applications'}],deployments:[],latestDeployment:()=>undefined,refresh:vi.fn(),status:{}})}));
afterEach(()=>{cleanup();vi.unstubAllGlobals();});
it('lists all three runtimes in the one applications index, each linking to its page',()=>{
  render(<ApplicationsPage/>);
  for(const id of ['go','rails','astro'])expect(screen.getByRole('link',{name:id})).toHaveAttribute('href',`/console/applications/${id}`);
  expect(screen.getByText('Astro')).toBeInTheDocument();expect(screen.getByText('Ruby on Rails')).toBeInTheDocument();
  expect(screen.queryByRole('button',{name:'Deploy'})).not.toBeInTheDocument();
  expect(screen.queryByRole('button',{name:'Refresh service status'})).not.toBeInTheDocument();
});
it('offers Deploy only where the panel owns deployments',()=>{
  vi.stubGlobal('fetch',vi.fn().mockResolvedValue({ok:true,status:204,json:async()=>({})}));
  const enabled=(id:string)=>{const view=render(<ApplicationDetailPage appID={id} tab="overview"/>);const state=!screen.getByRole('button',{name:'Deploy'}).hasAttribute('disabled');view.unmount();return state;};
  expect(['go','rails','astro'].filter(enabled)).toEqual(['go']);
});
it('provides the same settings tabs for native Rails while preserving host ownership and secrets',async()=>{
  const fetch=vi.fn().mockResolvedValue({ok:true,status:200,json:async()=>({id:'rails',runtime:'rails',native:{id:'rails',name:'rails',services:['rails-server.service']},repository:'org/rails',branch:'master',domain:'rails.example.com',server_id:'host',additional_domains_supported:true,environment:[{key:'HOST_APP_ID',kind:'setting',value:'12376'},{key:'LOG_LEVEL',kind:'setting',value:'debug'},{key:'RAILS_MASTER_KEY',kind:'credential',length:32}],capabilities:{configure:false,environment:false,deploy:false,reload:false,rollback:false,processes:true,logs:true,reason:'Existing runtime is preserved.'}})});vi.stubGlobal('fetch',fetch);
  vi.stubGlobal('fetch',fetch);
  const settings=render(<AppSettingsTab appID="rails"/>);
  await screen.findByDisplayValue('Ruby on Rails');
  expect(screen.getByRole('button',{name:'Save changes'})).toBeDisabled();expect(screen.getByDisplayValue('master')).toBeDisabled();
  settings.unmount();
  render(<AppEnvironmentTab appID="rails"/>);
  await screen.findByDisplayValue('12376');
  expect(screen.getByDisplayValue('12376')).toHaveAttribute('type','text');expect(screen.getByDisplayValue('debug')).toBeDisabled();
  expect(screen.queryByRole('button',{name:'Reveal credential'})).not.toBeInTheDocument();expect(screen.getByRole('button',{name:'Add variable'})).toBeDisabled();
});

it('keeps auto-deploy enabling disabled for native apps without publication ownership',async()=>{
  const fetch=vi.fn().mockResolvedValue({ok:true,status:204,json:async()=>({})});vi.stubGlobal('fetch',fetch);
  render(<ApplicationDetailPage appID="rails" tab="overview"/>);
  fireEvent.keyDown(screen.getByRole('button',{name:'Actions'}),{key:'ArrowDown'});
  const action=await screen.findByRole('menuitem',{name:'Enable auto-deploy'});
  expect(action).toHaveAttribute('aria-disabled','true');
  const calls=fetch.mock.calls.length;
  fireEvent.click(action);expect(fetch.mock.calls.length).toBe(calls);
});
