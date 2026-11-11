import {cleanup,fireEvent,render,screen} from '@testing-library/react';
import {afterEach,it,expect,vi} from 'vitest';
import {ApplicationsPage} from './ApplicationsPage';
import {ApplicationDetailPage} from './ApplicationDetailPage';
const single={apps:[{id:'new-app',repository:'org/app',branch:'main',domain:'app.example.com',server_id:'local'}] as Array<Record<string,string>>,servers:[{id:'local'}] as Array<Record<string,string>>};
const state=vi.hoisted(()=>({deployment:undefined as {id:string;app_id:string;status:string;error?:string;created:string}|undefined,workspace:undefined as undefined|{apps:Array<Record<string,string>>;servers:Array<Record<string,string>>}}));
vi.mock('../state/WorkspaceContext',()=>({useWorkspace:()=>({apps:(state.workspace||single).apps,servers:(state.workspace||single).servers,deployments:state.deployment?[state.deployment]:[],latestDeployment:()=>state.deployment,refresh:vi.fn()})}));
const quiet=()=>vi.stubGlobal('fetch',vi.fn().mockResolvedValue({ok:true,status:204,json:async()=>({})}));
afterEach(()=>{cleanup();state.deployment=undefined;state.workspace=undefined;vi.unstubAllGlobals();});

it('lists applications in the index template, each name linking to its page, without detail content',()=>{
  const {container}=render(<ApplicationsPage/>);
  expect(container.querySelector('[data-template=index][data-resource=applications]')).not.toBeNull();
  expect(screen.getByRole('link',{name:'new-app'})).toHaveAttribute('href','/console/applications/new-app');
  expect(screen.getByText('No release deployed')).toBeTruthy();expect(screen.queryByText('pending')).toBeNull();
  expect(screen.getByText('Never deployed')).toBeTruthy();
  for(const name of ['Deploy','Build logs','More'])expect(screen.queryByRole('button',{name})).toBeNull();
  expect(container.querySelector('[role=tablist], [data-danger-zone], .migration-plan')).toBeNull();
});

it('opens on applications without manual inventory setup forms',()=>{render(<ApplicationsPage/>);expect(screen.getByRole('heading',{level:1})).toHaveTextContent('Applications');expect(screen.queryByText('Connect existing native Rails services')).not.toBeInTheDocument();expect(screen.queryByText('Import existing applications for observation')).not.toBeInTheDocument();expect(screen.queryByLabelText('Host inventory JSON')).not.toBeInTheDocument();expect(screen.queryByText('Add existing applications')).not.toBeInTheDocument();});

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
  for(const link of screen.getAllByRole('link',{name:'Apps'}))expect(link).toHaveAttribute('href','/console/servers/b');
});

it('shows the latest failure in the application header with a link to its log',()=>{quiet();state.deployment={id:'failed',app_id:'new-app',status:'failed',error:'host port conflict',created:'2026-10-20T00:00:00Z'};render(<ApplicationDetailPage appID="new-app" tab="overview"/>);expect(screen.getByRole('alert').textContent).toContain('host port conflict');expect(screen.getByRole('link',{name:'Open its log'})).toHaveAttribute('href','/console/deployments/failed/log');});

it('surfaces a rejected Deploy request in the application header',async()=>{vi.stubGlobal('fetch',vi.fn(async(url:string,init?:RequestInit)=>init?.method==='POST'?{ok:false,status:409,json:async()=>({error:'GitHub installation access unavailable'})}:{ok:true,status:204,json:async()=>({})}));render(<ApplicationDetailPage appID="new-app" tab="overview"/>);fireEvent.click(screen.getByRole('button',{name:'Deploy'}));expect((await screen.findByRole('alert')).textContent).toContain('GitHub installation access unavailable');});

it('keeps an accepted deployment when the history refresh has no rows',async()=>{vi.stubGlobal('fetch',vi.fn(async(url:string,init?:RequestInit)=>init?.method==='POST'?{ok:true,status:202,json:async()=>({id:'accepted-job',status:'queued',created:new Date().toISOString()})}:{ok:true,status:204,json:async()=>({})}));render(<ApplicationDetailPage appID="new-app" tab="overview"/>);fireEvent.click(screen.getByRole('button',{name:'Deploy'}));expect((await screen.findAllByText('queued')).length).toBeGreaterThan(0);expect(screen.getByRole('link',{name:'Follow the build log'})).toHaveAttribute('href','/console/deployments/accepted-job/log');});

it('shows a running deployment on the application page with the time since it started',()=>{quiet();state.deployment={id:'running',app_id:'new-app',status:'building',created:new Date(Date.now()-65_000).toISOString()};const {container}=render(<ApplicationDetailPage appID="new-app" tab="overview"/>);expect(container.querySelector('.app-progress-card .live-progress-compact')).toHaveTextContent(/Deploying1m 0\ds/);});

it('disables Deploy for an application a move holds, keeping read-only tabs',()=>{
  quiet();
  state.workspace={apps:[{id:'shop',repository:'org/shop',branch:'main',domain:'shop.example.com',server_id:'a',move:{id:'m1',state:'syncing',source_server:'a',target_server:'b',locked:true,phase:'locked'}} as unknown as Record<string,string>],servers:[{id:'a',name:'Edge'},{id:'b',name:'Apps'}]};
  const view=render(<ApplicationDetailPage appID="shop" tab="overview"/>);
  const deploy=screen.getByRole('button',{name:'Deploy'});
  expect(deploy).toBeDisabled();
  expect(deploy).toHaveAttribute('title','shop is in the final sync of its move to Apps; finish or abort the move first.');
  expect(screen.getByRole('tab',{name:'Settings'})).toBeEnabled();
  view.unmount();
  // While the move copies in the background the application stays usable.
  state.workspace={apps:[{id:'shop',repository:'org/shop',branch:'main',domain:'shop.example.com',server_id:'a',move:{id:'m1',state:'running',source_server:'a',target_server:'b',locked:false,phase:'live'}} as unknown as Record<string,string>],servers:[{id:'a',name:'Edge'},{id:'b',name:'Apps'}]};
  render(<ApplicationDetailPage appID="shop" tab="overview"/>);
  expect(screen.getByRole('button',{name:'Deploy'})).toBeEnabled();
});

it('shows the release warnings of an application on its page',()=>{
  quiet();
  const warning='Port 25/tcp receives mail, but no service declares public_tls: senders that require STARTTLS (Gmail, Microsoft) fail and defer mail.';
  state.workspace={apps:[{id:'mailer',repository:'org/mailer',branch:'main',domain:'mail.example.com',server_id:'local',warnings:[warning]} as unknown as Record<string,string>],servers:[{id:'local'}]};
  const {container}=render(<ApplicationDetailPage appID="mailer" tab="overview"/>);
  expect(container.querySelector('.app-warnings .alert-warning')).toHaveTextContent(warning);
  cleanup();
  state.workspace=undefined;
  const plain=render(<ApplicationDetailPage appID="new-app" tab="overview"/>);
  expect(plain.container.querySelector('.app-warnings')).toBeNull();
});

it('shows a missing application as such, not a blank page',()=>{quiet();render(<ApplicationDetailPage appID="gone" tab="overview"/>);expect(screen.getByText('Application not found')).toBeInTheDocument();expect(screen.queryByRole('tablist')).toBeNull();});
