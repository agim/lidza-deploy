import {cleanup,fireEvent,render,screen} from '@testing-library/react';
import {afterEach,it,expect,vi} from 'vitest';
import {AppSettingsPage} from './AppSettingsPage';
import {ApplicationsPage} from './ApplicationsPage';
vi.mock('../state/WorkspaceContext',()=>({useWorkspace:()=>({apps:[{id:'go',runtime:'go',repository:'org/go',branch:'main',domain:'go.example.com',server_id:'source'},{id:'rails',runtime:'rails',repository:'org/rails',branch:'main',domain:'rails.example.com',server_id:'host',native:{id:'rails',name:'rails',services:['rails-server.service']}},{id:'astro',runtime:'astro',repository:'org/site',branch:'main',domain:'example.com',server_id:'host',native:{id:'astro',name:'site',services:[]}}],servers:[{id:'source',name:'Source'},{id:'host',name:'Applications'}],deployments:[],latestDeployment:()=>undefined,refresh:vi.fn(),status:{}})}));
afterEach(()=>{cleanup();vi.unstubAllGlobals();});
it('uses the common application cards and settings links for all three runtimes',()=>{
  render(<ApplicationsPage/>);
  for(const id of ['go','rails','astro'])expect(screen.getByRole('button',{name:`Settings for ${id}`})).toBeEnabled();
  expect(screen.getByText('Astro')).toBeInTheDocument();expect(screen.getByText('Rails')).toBeInTheDocument();
  const deploy=screen.getAllByRole('button',{name:'Deploy'});expect(deploy.filter(button=>!button.hasAttribute('disabled'))).toHaveLength(1);
  expect(screen.queryByRole('button',{name:'Refresh service status'})).not.toBeInTheDocument();
});
it('provides the same settings tabs for native Rails while preserving host ownership and secrets',async()=>{
  const fetch=vi.fn().mockResolvedValue({ok:true,status:200,json:async()=>({id:'rails',runtime:'rails',native:{id:'rails',name:'rails',services:['rails-server.service']},repository:'org/rails',branch:'master',domain:'rails.example.com',server_id:'host',additional_domains_supported:true,environment:[{key:'HOST_APP_ID',kind:'setting',value:'12376'},{key:'LOG_LEVEL',kind:'setting',value:'debug'},{key:'RAILS_MASTER_KEY',kind:'credential',length:32}],capabilities:{configure:false,environment:false,deploy:false,reload:false,rollback:false,processes:true,logs:true,reason:'Existing runtime is preserved.'}})});vi.stubGlobal('fetch',fetch);
  render(<AppSettingsPage appID="rails"/>);
  await screen.findByDisplayValue('Ruby on Rails');
  for(const label of ['General','Environment','Data & storage','Processes'])expect(screen.getByRole('tab',{name:label})).toBeInTheDocument();
  expect(screen.getByRole('button',{name:'Save changes'})).toBeDisabled();expect(screen.getByDisplayValue('master')).toBeDisabled();
  fireEvent.mouseDown(screen.getByRole('tab',{name:'Environment'}),{button:0,ctrlKey:false});
  expect(screen.getByDisplayValue('12376')).toHaveAttribute('type','text');expect(screen.getByDisplayValue('debug')).toBeDisabled();
  expect(screen.queryByRole('button',{name:'Reveal credential'})).not.toBeInTheDocument();expect(screen.getByRole('button',{name:'Add variable'})).toBeDisabled();expect(fetch).toHaveBeenCalledTimes(1);
});

it('keeps auto-deploy enabling disabled for native apps without publication ownership',async()=>{
  const fetch=vi.fn();vi.stubGlobal('fetch',fetch);
  render(<ApplicationsPage/>);
  fireEvent.keyDown(screen.getAllByRole('button',{name:/^More$/})[1],{key:'ArrowDown'});
  const action=await screen.findByRole('menuitem',{name:'Enable auto-deploy'});
  expect(action).toHaveAttribute('aria-disabled','true');
  fireEvent.click(action);expect(fetch).not.toHaveBeenCalled();
});
