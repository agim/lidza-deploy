import {cleanup,render,screen} from '@testing-library/react';
import {afterEach,it,expect,vi} from 'vitest';
import {ApplicationProcesses} from './ApplicationProcesses';
afterEach(()=>{cleanup();vi.unstubAllGlobals();});
it('shows stopped state separately from disabled startup without actions',async()=>{
  const fetch=vi.fn().mockResolvedValue({ok:true,status:200,json:async()=>({processes:[{id:'worker',name:'Worker',kind:'Worker',state:'inactive',startup:'disabled'}]})});vi.stubGlobal('fetch',fetch);
  render(<ApplicationProcesses appID="app"/>);
  await screen.findByText('Startup: disabled');expect(screen.getByText('inactive')).toBeInTheDocument();
  expect(screen.queryByRole('button',{name:/start|restart|enable/i})).not.toBeInTheDocument();expect(fetch).toHaveBeenCalledTimes(1);expect(fetch.mock.calls[0][1].method).toBeUndefined();
});
