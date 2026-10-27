import {cleanup,render,screen,within} from '@testing-library/react';
import {afterEach,it,expect,vi} from 'vitest';
import {ContainersPage} from './ServerDetailPages';
vi.mock('../state/WorkspaceContext',()=>({useWorkspace:()=>({servers:[{id:'local',name:'Host'}],apps:[{id:'one',domain:'one.example.com'}]})}));
afterEach(()=>{cleanup();vi.unstubAllGlobals();});
it('groups application, database and sidecars under their app and isolates unassigned containers',async()=>{
 vi.stubGlobal('fetch',vi.fn().mockResolvedValue({ok:true,json:async()=>({containers:[{name:'one-app',app_id:'one',role:'Web',state:'running',restartable:false},{name:'one-db',app_id:'one',role:'PostgreSQL',state:'running',restartable:false},{name:'two-mail',app_id:'two',role:'Mail',state:'running',restartable:false},{name:'system',state:'running',restartable:false}]})}));
 const {container}=render(<ContainersPage serverID="local"/>);await screen.findByText('one-db');
 const one=container.querySelector('[data-app-containers="one"]') as HTMLElement;
 expect(within(one).getByText('one-app')).toBeTruthy();expect(within(one).getByText('one-db')).toBeTruthy();expect(within(one).queryByText('two-mail')).toBeNull();expect(within(one).getByText('2 containers · one.example.com')).toBeTruthy();
 expect(container.querySelector('[data-app-containers="two"]')?.textContent).toContain('two-mail');expect(container.querySelector('[data-app-containers="__unassigned"]')?.textContent).toContain('system');
});

it('shows public certificate pending and renewal metadata without certificate material',async()=>{
 vi.stubGlobal('fetch',vi.fn().mockResolvedValue({ok:true,json:async()=>({containers:[{name:'one-mail',app_id:'one',state:'running',restartable:false,public_tls:{hostname:'one.example.com',status:'pending'}},{name:'two-mail',app_id:'two',state:'running',restartable:false,public_tls:{hostname:'two.example.com',status:'ready',not_after:'2030-01-01T00:00:00Z'}}]})}));
 render(<ContainersPage serverID="local"/>);
 expect(await screen.findByText('Service TLS: waiting for certificate')).toBeTruthy();
 expect(screen.getByText('Service TLS: certificate ready')).toBeTruthy();
 expect(screen.getByText(/two.example.com · expires/)).toBeTruthy();
});
