import { useEffect, useState } from 'react';
import { get, post } from '../lib/api';
import { useToast } from './ToastProvider';
import { Badge, Button, Card, ConfirmAction, ErrorBanner, Loading, SectionHeader } from './ui';

/** Whether a server can run native applications (control/servers/{id}/host/native). */
export type HostNativeStatus = { configured: boolean; user?: string; root?: string; supported: boolean; reason?: string; restarting?: boolean };

const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);
export const nativeEndpoint = (serverID: string) => `control/servers/${encodeURIComponent(serverID)}/host/native`;

/**
 * ServerNativeApps: whether this server runs native applications (built and
 * run directly on the host) besides containers. Servers set up by the
 * installer are ready from the start; an older server is enabled here, which
 * creates the deployment account and restarts the server's agent once.
 */
export function ServerNativeApps({ serverID }: { serverID: string }) {
  const notify = useToast();
  const [status, setStatus] = useState<HostNativeStatus>();
  const [error, setError] = useState('');
  const [missing, setMissing] = useState(false);
  const load = () => get<HostNativeStatus>(nativeEndpoint(serverID)).then(value => { setStatus(value); setError(''); return value; });
  useEffect(() => {
    let active = true;
    setStatus(undefined); setMissing(false);
    get<HostNativeStatus>(nativeEndpoint(serverID)).then(value => { if (active) { setStatus(value); setError(''); } }).catch(reason => { if (active) { setMissing(true); setError(message(reason)); } });
    return () => { active = false; };
  }, [serverID]);
  // While the agent restarts, ask again until it reports the new configuration.
  useEffect(() => {
    if (!status?.restarting) return;
    const timer = setInterval(() => { void load().then(value => { if (value.configured) notify('Native applications are enabled on this server.'); }).catch(() => { /* the agent is restarting; the next poll retries */ }); }, 3000);
    return () => clearInterval(timer);
  }, [status?.restarting, serverID]);
  const enable = async () => {
    try { setStatus(await post<HostNativeStatus>(nativeEndpoint(serverID), {})); setError(''); } catch (reason) { setError(message(reason)); }
  };
  const badge = !status ? undefined : status.configured ? <Badge tone="success">Enabled</Badge> : status.restarting ? <Badge tone="warning">Restarting agent</Badge> : <Badge tone="neutral">Not enabled</Badge>;
  return <Card className="form-card" data-native-apps={status?.configured ? 'enabled' : 'disabled'}>
    <SectionHeader title="Native applications" description="Applications built and run directly on this server, without a container. Moving a native application here needs this." actions={badge} />
    {error && <ErrorBanner>{error}</ErrorBanner>}
    {!status && !missing && <Loading />}
    {status?.configured && <p className="muted-copy">Deployment account <code>{status.user}</code>, application root <code>{status.root}</code>.</p>}
    {status?.restarting && <p role="status">The deployment account is ready; the agent on this server is restarting to load it.</p>}
    {status && !status.configured && !status.restarting && (status.supported
      ? <><p className="muted-copy">Enabling creates the <code>deploy</code> account on this server if it is missing and restarts the server's agent once. Running applications are not touched.</p><ConfirmAction title="Enable native applications?" description="The agent on this server restarts once. Deployments or backups running on it right now are interrupted." confirmLabel="Enable" onConfirm={enable}><Button type="button" variant="primary">Enable native applications</Button></ConfirmAction></>
      : <p className="muted-copy">{status.reason}</p>)}
  </Card>;
}
