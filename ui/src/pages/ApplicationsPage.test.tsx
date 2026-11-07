import {cleanup,fireEvent,render,screen} from '@testing-library/react';
import {afterEach,it,expect,vi} from 'vitest';
import {ApplicationsPage} from './ApplicationsPage';
const single={apps:[{id:'new-app',repository:'org/app',branch:'main',domain:'app.example.com',server_id:'local'}] as Array<Record<string,string>>,servers:[{id:'local'}] as Array<Record<string,string>>};
const state=vi.hoisted(()=>({deployment:undefined as {id:string;app_id:string;status:string;error?:string;created:string}|undefined,workspace:undefined as undefined|{apps:Array<Record<string,string>>;servers:Array<Record<string,string>>}}));
vi.mock('../state/WorkspaceContext',()=>({useWorkspace:()=>({apps:(state.workspace||single).apps,servers:(state.workspace||single).servers,deployments:state.deployment?[state.deployment]:[],latestDeployment:()=>state.deployment,refresh:vi.fn()})}));
afterEach(()=>{cleanup();state.deployment=undefined;state.workspace=undefined;vi.unstubAllGlobals();});
it('does not label an undeployed branch as pending',()=>{render(<ApplicationsPage/>);expect(screen.getByText('No release deployed')).toBeTruthy();expect(screen.queryByText('pending')).toBeNull();expect(screen.getByRole('button',{name:'Build logs'})).toBeDisabled();});
it('shows the latest failure and keeps its build logs accessible',()=>{state.deployment={id:'failed',app_id:'new-app',status:'failed',error:'host port conflict',created:'2026-10-20T00:00:00Z'};render(<ApplicationsPage/>);expect(screen.getByRole('alert').textContent).toContain('host port conflict');expect(screen.getByRole('button',{name:'Build logs'})).toBeEnabled();});

it('surfaces a rejected Deploy request on the app card',async()=>{vi.stubGlobal('fetch',vi.fn().mockResolvedValue({ok:false,status:409,json:async()=>({error:'GitHub installation access unavailable'})}));render(<ApplicationsPage/>);fireEvent.click(screen.getByRole('button',{name:'Deploy'}));expect((await screen.findByRole('alert')).textContent).toContain('GitHub installation access unavailable');});

it('retains an accepted deployment when the history refresh has no rows',async()=>{vi.stubGlobal('fetch',vi.fn().mockResolvedValue({ok:true,status:202,json:async()=>({id:'accepted-job',status:'queued',created:new Date().toISOString()})}));render(<ApplicationsPage/>);fireEvent.click(screen.getByRole('button',{name:'Deploy'}));await screen.findByText('queued');expect(screen.getByRole('button',{name:'Build logs'})).toBeEnabled();});

it('opens on applications without manual inventory setup forms',()=>{render(<ApplicationsPage/>);expect(screen.getByText('Your applications')).toBeInTheDocument();expect(screen.queryByText('Connect existing native Rails services')).not.toBeInTheDocument();expect(screen.queryByText('Import existing applications for observation')).not.toBeInTheDocument();expect(screen.queryByLabelText('Host inventory JSON')).not.toBeInTheDocument();expect(screen.queryByText('Add existing applications')).not.toBeInTheDocument();});

it('offers runtime and server filters only when the workspace has more than one of each',()=>{
  render(<ApplicationsPage/>);
  expect(screen.queryByRole('combobox',{name:'Filter by runtime'})).toBeNull();
  expect(screen.queryByRole('combobox',{name:'Filter by server'})).toBeNull();
  cleanup();
  state.workspace={apps:[{id:'site',repository:'org/site',branch:'main',domain:'site.example.com',server_id:'a',runtime:'astro'},{id:'shop',repository:'org/shop',branch:'main',domain:'shop.example.com',server_id:'b',runtime:'rails'},{id:'api',repository:'org/api',branch:'main',domain:'api.example.com',server_id:'b'}],servers:[{id:'a',name:'Edge'},{id:'b',name:'Apps'}]};
  render(<ApplicationsPage/>);
  expect(screen.getByRole('combobox',{name:'Filter by runtime'})).toHaveTextContent('All runtimes');
  expect(screen.getByRole('combobox',{name:'Filter by server'})).toHaveTextContent('All servers');
  expect(screen.queryByRole('button',{name:'Clear filters'})).toBeNull();
});

it('shows a running deployment on its card with the time since it started',()=>{state.deployment={id:'running',app_id:'new-app',status:'building',created:new Date(Date.now()-65_000).toISOString()};const {container}=render(<ApplicationsPage/>);const progress=container.querySelector('[data-app-card="new-app"] .live-progress-compact')!;expect(progress).toHaveTextContent(/Deploying1m 0\ds/);});

it('disables changes on the card of an application a move holds, keeping read-only actions',async()=>{
  state.workspace={apps:[{id:'shop',repository:'org/shop',branch:'main',domain:'shop.example.com',server_id:'a',move:{id:'m1',state:'syncing',source_server:'a',target_server:'b',locked:true,phase:'locked'}} as unknown as Record<string,string>],servers:[{id:'a',name:'Edge'},{id:'b',name:'Apps'}]};
  vi.stubGlobal('fetch',vi.fn().mockResolvedValue({ok:true,status:204,json:async()=>({})}));
  render(<ApplicationsPage/>);
  const deploy=screen.getByRole('button',{name:'Deploy'});
  expect(deploy).toBeDisabled();
  expect(deploy).toHaveAttribute('title','shop is in the final sync of its move to Apps; finish or abort the move first.');
  expect(screen.getByRole('button',{name:'Settings for shop'})).toBeEnabled();
  cleanup();
  // While the move copies in the background the application stays usable.
  state.workspace={apps:[{id:'shop',repository:'org/shop',branch:'main',domain:'shop.example.com',server_id:'a',move:{id:'m1',state:'running',source_server:'a',target_server:'b',locked:false,phase:'live'}} as unknown as Record<string,string>],servers:[{id:'a',name:'Edge'},{id:'b',name:'Apps'}]};
  render(<ApplicationsPage/>);
  expect(screen.getByRole('button',{name:'Deploy'})).toBeEnabled();
  cleanup();
  state.workspace={apps:[{id:'shop',repository:'org/shop',branch:'main',domain:'shop.example.com',server_id:'a',move:{id:'m1',state:'finished',source_server:'a',target_server:'b'}} as unknown as Record<string,string>],servers:[{id:'a',name:'Edge'},{id:'b',name:'Apps'}]};
  render(<ApplicationsPage/>);
  expect(screen.getByRole('button',{name:'Deploy'})).toBeEnabled();
});

it('shows the release warnings of an application on its card',()=>{
  const warning='Port 25/tcp receives mail, but no service declares public_tls: senders that require STARTTLS (Gmail, Microsoft) fail and defer mail.';
  state.workspace={apps:[{id:'mailer',repository:'org/mailer',branch:'main',domain:'mail.example.com',server_id:'local',warnings:[warning]} as unknown as Record<string,string>],servers:[{id:'local'}]};
  const {container}=render(<ApplicationsPage/>);
  const card=container.querySelector('[data-app-card="mailer"]')!;
  expect(card.querySelector('.app-warnings .alert-warning')).toHaveTextContent(warning);
  cleanup();
  state.workspace=undefined;
  const plain=render(<ApplicationsPage/>);
  expect(plain.container.querySelector('.app-warnings')).toBeNull();
});
