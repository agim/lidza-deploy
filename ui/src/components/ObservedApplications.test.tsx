import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, expect, it } from 'vitest';
import { ObservedApplicationCard } from './ObservedApplications';

afterEach(cleanup);
it('shows an observed Rails app without management controls or an unverified live status',()=>{
  render(<ObservedApplicationCard app={{id:'observed:host:rails',repository:'Rails',branch:'',server_id:'observed:host',domain:'app.example.com',runtime:'rails',observed:{name:'Rails',source:'host',processes:['web','worker'],observed_at:''}}}/>);
  expect(screen.getByText('Observed')).toBeInTheDocument();
  expect(screen.getByText('worker')).toBeInTheDocument();
  expect(screen.queryByRole('button',{name:'Deploy'})).not.toBeInTheDocument();
  expect(screen.queryByText('live')).not.toBeInTheDocument();
});
