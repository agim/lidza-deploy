import {act,cleanup,fireEvent,render,screen} from '@testing-library/react';
import {afterEach,it,expect,vi} from 'vitest';
import {ApplicationProcesses} from './ApplicationProcesses';
afterEach(()=>{cleanup();vi.unstubAllGlobals();});
it('shows stopped state separately from disabled startup without actions',async()=>{
  const fetch=vi.fn().mockResolvedValue({ok:true,status:200,json:async()=>({processes:[{id:'worker',name:'Worker',kind:'Worker',state:'inactive',startup:'disabled'}]})});vi.stubGlobal('fetch',fetch);
  render(<ApplicationProcesses appID="app"/>);
  await screen.findByText('Startup: disabled');expect(screen.getByText('inactive')).toBeInTheDocument();
  expect(screen.queryByRole('button',{name:/start|restart|enable/i})).not.toBeInTheDocument();expect(fetch).toHaveBeenCalledTimes(1);expect(fetch.mock.calls[0][1].method).toBeUndefined();
});

it('labels the selected process logs and ignores stale responses when switching',async()=>{
  let finishWeb!:(value:unknown)=>void;
  let workerReads=0;
  const response=(data:unknown)=>({ok:true,status:200,json:async()=>data});
  const fetch=vi.fn((url:string)=>{
    if(url.endsWith('/processes'))return Promise.resolve(response({processes:[{id:'web',name:'web.service',kind:'Web',state:'active',log_path:'logs/web'},{id:'worker',name:'worker.service',kind:'Worker',state:'active',log_path:'logs/worker'}]}));
    if(url.endsWith('/logs/web'))return new Promise(resolve=>{finishWeb=resolve;});
    return Promise.resolve(response({log:++workerReads===1?'worker output':'refreshed worker output'}));
  });
  vi.stubGlobal('fetch',fetch);
  render(<ApplicationProcesses appID="app"/>);
  fireEvent.click(await screen.findByRole('button',{name:'View logs for web.service'}));
  fireEvent.click(screen.getByRole('button',{name:'View logs for worker.service'}));
  await screen.findByText('worker output');
  await act(async()=>{finishWeb(response({log:'stale web output'}));});
  expect(screen.queryByText('stale web output')).not.toBeInTheDocument();
  expect(screen.getByRole('region',{name:'worker.service log output'})).toHaveClass('process-log-output');
  expect(screen.getByRole('button',{name:'Copy log'})).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button',{name:'Refresh log'}));
  await screen.findByText('refreshed worker output');
  fireEvent.click(screen.getByRole('button',{name:'Close log'}));
  expect(screen.queryByRole('region',{name:'worker.service logs'})).not.toBeInTheDocument();
});

it('shows an empty log result instead of silently hiding the viewer',async()=>{
  vi.stubGlobal('fetch',vi.fn((url:string)=>Promise.resolve({ok:true,status:200,json:async()=>url.endsWith('/processes')?{processes:[{id:'web',name:'web.service',kind:'Web',state:'active',log_path:'logs/web'}]}:{log:''}})));
  render(<ApplicationProcesses appID="app"/>);
  fireEvent.click(await screen.findByRole('button',{name:'View logs for web.service'}));
  await screen.findByText('No log output available.');
});
