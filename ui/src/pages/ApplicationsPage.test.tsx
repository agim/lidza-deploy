import {cleanup,fireEvent,render,screen} from '@testing-library/react';
import {afterEach,it,expect,vi} from 'vitest';
import {ApplicationsPage} from './ApplicationsPage';
const state=vi.hoisted(()=>({deployment:undefined as {id:string;app_id:string;status:string;error?:string;created:string}|undefined}));
vi.mock('../state/WorkspaceContext',()=>({useWorkspace:()=>({apps:[{id:'new-app',repository:'org/app',branch:'main',domain:'app.example.com',server_id:'local'}],servers:[{id:'local'}],deployments:state.deployment?[state.deployment]:[],latestDeployment:()=>state.deployment,refresh:vi.fn()})}));
afterEach(()=>{cleanup();state.deployment=undefined;vi.unstubAllGlobals();});
it('does not label an undeployed branch as pending',()=>{render(<ApplicationsPage/>);expect(screen.getByText('No release deployed')).toBeTruthy();expect(screen.queryByText('pending')).toBeNull();expect(screen.getByRole('button',{name:'Build logs'})).toBeDisabled();});
it('shows the latest failure and keeps its build logs accessible',()=>{state.deployment={id:'failed',app_id:'new-app',status:'failed',error:'host port conflict',created:'2026-10-20T00:00:00Z'};render(<ApplicationsPage/>);expect(screen.getByRole('alert').textContent).toContain('host port conflict');expect(screen.getByRole('button',{name:'Build logs'})).toBeEnabled();});

it('surfaces a rejected Deploy request on the app card',async()=>{vi.stubGlobal('fetch',vi.fn().mockResolvedValue({ok:false,status:409,json:async()=>({error:'GitHub installation access unavailable'})}));render(<ApplicationsPage/>);fireEvent.click(screen.getByRole('button',{name:'Deploy'}));expect((await screen.findByRole('alert')).textContent).toContain('GitHub installation access unavailable');});

it('retains an accepted deployment when the history refresh has no rows',async()=>{vi.stubGlobal('fetch',vi.fn().mockResolvedValue({ok:true,status:202,json:async()=>({id:'accepted-job',status:'queued',created:new Date().toISOString()})}));render(<ApplicationsPage/>);fireEvent.click(screen.getByRole('button',{name:'Deploy'}));await screen.findByText('queued');expect(screen.getByRole('button',{name:'Build logs'})).toBeEnabled();});
