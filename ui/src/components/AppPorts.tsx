import { Network, RefreshCw } from 'lucide-react';
import { useEffect, useState } from 'react';
import { get } from '../lib/api';
import type { ForwardedPort } from '../lib/types';
import { Button, Card, ErrorBanner, Loading, Notice, SectionHeader } from './ui';

const publications: Record<string, string> = { published: 'Published', private_proxy: 'Private proxy', stopped: 'Stopped', missing: 'Container missing', mismatch: 'Publication mismatch', undeclared: 'Undeclared publication' };
const firewallStates: Record<string, string> = { allow_rule_installed: 'Allow rule installed', not_installed: 'Allow rule missing', unsupported: 'Unsupported / needs review', not_required: 'Not required for loopback', not_managed: 'Not managed' };
export function PortSummary({ ports }: { ports: ForwardedPort[] }) {
 return <div className="port-inventory">{ports.map((port, index) => <div className="port-item" key={`${port.container}-${port.host_ip}-${port.host_port}-${port.protocol}-${index}`}>
  <strong><Network size={15} />{port.host_ip}:{port.host_port}/{port.protocol.toUpperCase()}</strong>
  <span>{port.declared ? 'Declared public port' : 'Observed binding'} → {port.service} · {port.container}:{port.container_port}</span>
  <span>Docker: {publications[port.publication] || port.publication}</span>
  {port.publication === 'mismatch' && <span>Actual bindings: {port.bindings?.length ? port.bindings.map(binding => `${binding.HostIp}:${binding.HostPort}`).join(', ') : 'none'}</span>}
  <span>Host firewall: {firewallStates[port.firewall] || port.firewall}</span>
  <span>External reachability: {port.external === 'not_applicable' ? 'Not applicable' : 'Unverified'}</span>
 </div>)}</div>;
}
export function AppPorts({ appID }: { appID: string }) {
 const [ports, setPorts] = useState<ForwardedPort[]>([]);
 const [warning, setWarning] = useState(''); const [error, setError] = useState(''); const [loading, setLoading] = useState(true);
 const load = async () => { try { const result = await get<{ ports: ForwardedPort[]; warning?: string }>(`control/apps/${encodeURIComponent(appID)}/ports`); setPorts(result.ports || []); setWarning(result.warning || ''); setError(''); } catch(reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setLoading(false); } };
 useEffect(() => { load(); const timer = setInterval(load, 15000); return () => clearInterval(timer); }, [appID]);
 return <Card className="form-card"><SectionHeader title="Forwarded ports" description="Declared public ports and actual Docker bindings for the current release." actions={<Button type="button" onClick={load}><RefreshCw size={16} />Refresh ports</Button>} />
  {error && <ErrorBanner>{error}</ErrorBanner>}{warning && <Notice>{warning}</Notice>}
  {loading ? <Loading label="Loading forwarded ports…" /> : ports.length ? <PortSummary ports={ports} /> : <p>No forwarded ports reported for the current release.</p>}
  <p className="muted-copy">Host allow rules do not configure a cloud-provider firewall or prove internet access. Allow the declared ports in your provider firewall and test from another network. SMTP port 25 may also be restricted by your provider. Loopback bindings stay private behind the HTTPS proxy.</p>
 </Card>;
}
