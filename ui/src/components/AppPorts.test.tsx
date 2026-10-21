import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { AppPorts, PortSummary } from './AppPorts';
import { get } from '../lib/api';
import type { ForwardedPort } from '../lib/types';
vi.mock('../lib/api', async original => ({ ...await original<typeof import('../lib/api')>(), get: vi.fn() }));
afterEach(() => {cleanup();vi.resetAllMocks();});
const port: ForwardedPort = {app_id:'fixture',service:'app',container:'lidza-fixture-app',release:'one',declared:true,protocol:'udp',host_ip:'0.0.0.0',host_port:7882,container_port:7882,publication:'published',firewall:'allow_rule_installed',external:'unverified'};
it('separates Docker publication, host rule and external reachability', async () => {
 vi.mocked(get).mockResolvedValue({ports:[port]});render(<AppPorts appID="fixture" />);
 await screen.findByText('0.0.0.0:7882/UDP');
 expect(screen.getByText('Docker: Published')).toBeTruthy();expect(screen.getByText('Host firewall: Allow rule installed')).toBeTruthy();expect(screen.getByText('External reachability: Unverified')).toBeTruthy();
 expect(screen.getByText(/SMTP port 25 may/)).toBeTruthy();expect(screen.getByRole('button',{name:'Refresh ports'}).getAttribute('type')).toBe('button');
});
it('shows mismatched actual publication and private bindings without claiming access', () => {
 render(<PortSummary ports={[{...port,publication:'mismatch',firewall:'not_installed',bindings:[{HostIp:'127.0.0.1',HostPort:'7882'}]},{...port,host_port:32790,container_port:3000,declared:false,publication:'private_proxy',firewall:'not_required',external:'not_applicable'}]} />);
 expect(screen.getByText('Actual bindings: 127.0.0.1:7882')).toBeTruthy();expect(screen.getByText('Host firewall: Allow rule missing')).toBeTruthy();expect(screen.getByText('External reachability: Not applicable')).toBeTruthy();
});
it('shows unsupported host warning and retries a failed inventory request', async () => {
 vi.mocked(get).mockResolvedValue({ports:[{...port,firewall:'unsupported'}],warning:'DOCKER-USER unavailable'});render(<AppPorts appID="fixture" />);
 await screen.findByText('DOCKER-USER unavailable');expect(screen.getByText('Host firewall: Unsupported / needs review')).toBeTruthy();
});
