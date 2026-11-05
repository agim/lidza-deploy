import {cleanup,fireEvent,render,screen} from '@testing-library/react';
import {afterEach,expect,it,vi} from 'vitest';
import {NativeApplicationCard} from './NativeApplications';
import type {Application} from '../lib/types';
const app:Application={id:'native-rails',runtime:'rails',repository:'native/rails',branch:'main',domain:'rails.example.com',server_id:'native',native:{id:'native-rails',name:'MyRails',services:['myrails-server.service']}};
afterEach(()=>{cleanup();vi.unstubAllGlobals();});
it('does not perform service actions or status checks automatically',()=>{
  const fetch=vi.fn();vi.stubGlobal('fetch',fetch);render(<NativeApplicationCard app={app}/>);
  expect(fetch).not.toHaveBeenCalled();
  expect(screen.queryByRole('button',{name:'Deploy'})).not.toBeInTheDocument();
  expect(screen.queryByRole('button',{name:'restart'})).not.toBeInTheDocument();
});
it('keeps service mutations unavailable before host approval while exposing status and logs',async()=>{
  const fetch=vi.fn().mockResolvedValue({ok:true,status:200,json:async()=>({application:{name:'MyRails'},services:[{unit:'myrails-server.service',active:'active',state:'running'}],service_control:false,checked:'2026-11-01T12:00:00Z'})});
  vi.stubGlobal('fetch',fetch);render(<NativeApplicationCard app={app}/>);
  fireEvent.click(screen.getByRole('button',{name:'Refresh service status'}));
  await screen.findByText('active · running');
  expect(screen.getByRole('button',{name:'View logs'})).toBeInTheDocument();
  expect(screen.queryByRole('button',{name:'restart'})).not.toBeInTheDocument();
  expect(fetch.mock.calls[0][1].method).toBeUndefined();
});
it('shows stopped and disabled separately without any activation request',async()=>{
  const fetch=vi.fn().mockResolvedValue({ok:true,status:200,json:async()=>({application:{name:'MyRails'},services:[{unit:'myrails-server.service',active:'inactive',state:'dead',unit_file_state:'disabled'}],service_control:false,checked:'2026-11-01T12:00:00Z'})});
  vi.stubGlobal('fetch',fetch);render(<NativeApplicationCard app={app}/>);
  fireEvent.click(screen.getByRole('button',{name:'Refresh service status'}));
  await screen.findByText('inactive · dead');
  expect(screen.getByText('Startup: disabled')).toBeInTheDocument();
  expect(screen.queryByRole('button',{name:/^(start|enable|restart)$/i})).not.toBeInTheDocument();
  expect(fetch).toHaveBeenCalledTimes(1);
  expect(fetch.mock.calls[0][1].method).toBeUndefined();
});
