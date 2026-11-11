import { useEffect, useState } from 'react';
import { get, put } from '../lib/api';
import type { ServiceControlStatus } from '../lib/types';
import { useToast } from './ToastProvider';
import { Badge, Card, ErrorBanner, Loading, SectionHeader } from './ui';
import { Switch } from './primitives/switch';

const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);
export const serviceControlEndpoint = (serverID: string) => `control/servers/${encodeURIComponent(serverID)}/host/service-control`;

/**
 * ServerServiceControl: the server setting that lets the panel start, stop
 * and restart applications adopted from the existing host setup. Created
 * applications are always controllable. Hidden on servers without native
 * management; the change applies at once, without restarting the agent.
 */
export function ServerServiceControl({ serverID }: { serverID: string }) {
  const notify = useToast();
  const [status, setStatus] = useState<ServiceControlStatus>();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    let active = true;
    setStatus(undefined);
    get<ServiceControlStatus>(serviceControlEndpoint(serverID)).then(value => { if (active) { setStatus(value); setError(''); } }).catch(reason => { if (active) setError(message(reason)); });
    return () => { active = false; };
  }, [serverID]);
  const change = async (enabled: boolean) => {
    setBusy(true);
    try {
      setStatus(await put<ServiceControlStatus>(serviceControlEndpoint(serverID), { enabled }));
      setError('');
      notify(enabled ? 'Adopted applications can be started, stopped and restarted here.' : 'Service control of adopted applications is off.');
    } catch (reason) { setError(message(reason)); } finally { setBusy(false); }
  };
  const header = <SectionHeader title="Service control" description="Whether the panel starts, stops and restarts adopted applications." actions={status?.supported && <Badge tone={status.source === 'setting' ? 'accent' : 'neutral'}>{status.source === 'setting' ? 'Set here' : 'Agent configuration default'}</Badge>} />;
  if (error && !status) return <Card className="form-card" data-server-service-control={serverID}>{header}<ErrorBanner>{error}</ErrorBanner></Card>;
  if (!status) return <Card className="form-card">{header}<Loading label="Reading the service control setting…" /></Card>;
  if (!status.supported) return null;
  const account = status.account ? <>the shared deployment account <code>{status.account}</code></> : 'the shared deployment account';
  return <Card className="form-card" data-server-service-control={serverID}>
    {header}
    {error && <ErrorBanner>{error}</ErrorBanner>}
    <label className="check-row switch-row">
      <Switch aria-label="Service control for adopted applications" checked={status.enabled} disabled={busy} onCheckedChange={enabled => { void change(enabled); }} />
      <span><strong>Service control for adopted applications</strong><small>On: Restart, Stop and Start in an adopted application’s Actions menu and Processes tab start, stop and restart its systemd units. Those units belong to {account}, which every adopted application shares, not to an account of their own. Applications created here always have service control. Applies at once.</small></span>
    </label>
    {status.source === 'config' && <p className="muted-copy">Until changed here, the agent configuration’s default applies: {status.config_default ? 'on' : 'off'}.</p>}
  </Card>;
}
