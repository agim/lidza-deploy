import { useEffect, useState } from 'react';
import { get, put } from '../lib/api';
import type { LogAccessStatus } from '../lib/types';
import { useToast } from './ToastProvider';
import { Badge, Card, ErrorBanner, Loading, SectionHeader } from './ui';
import { Switch } from './primitives/switch';

const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);
export const logAccessEndpoint = (serverID: string) => `control/servers/${encodeURIComponent(serverID)}/host/log-access`;

/**
 * ServerLogAccess: the server setting that adds the shared deployment
 * account to the systemd-journal group, so it reads every service's logs
 * over SSH. Hidden on servers without a shared deployment account; the agent
 * applies a change at once and keeps the membership to the setting.
 */
export function ServerLogAccess({ serverID }: { serverID: string }) {
  const notify = useToast();
  const [status, setStatus] = useState<LogAccessStatus>();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    let active = true;
    setStatus(undefined);
    get<LogAccessStatus>(logAccessEndpoint(serverID)).then(value => { if (active) { setStatus(value); setError(''); } }).catch(reason => { if (active) setError(message(reason)); });
    return () => { active = false; };
  }, [serverID]);
  const change = async (enabled: boolean) => {
    setBusy(true);
    try {
      const next = await put<LogAccessStatus>(logAccessEndpoint(serverID), { enabled });
      setStatus(next);
      setError('');
      if (!next.error) notify(enabled ? `${next.account ?? 'The deployment account'} can read the system logs in new SSH sessions.` : `${next.account ?? 'The deployment account'} no longer reads the system logs.`);
    } catch (reason) { setError(message(reason)); } finally { setBusy(false); }
  };
  const membership = status?.supported && <Badge tone={status.member ? 'success' : 'neutral'}>{status.member ? `In ${status.group}` : `Not in ${status.group}`}</Badge>;
  const header = <SectionHeader title="System log access" description="Whether the shared deployment account reads the server’s system journal." actions={membership} />;
  if (error && !status) return <Card className="form-card" data-server-log-access={serverID}>{header}<ErrorBanner>{error}</ErrorBanner></Card>;
  if (!status) return <Card className="form-card">{header}<Loading label="Reading the system log access setting…" /></Card>;
  if (!status.supported) return null;
  const account = status.account ? <code>{status.account}</code> : 'the shared deployment account';
  return <Card className="form-card" data-server-log-access={serverID}>
    {header}
    {error && <ErrorBanner>{error}</ErrorBanner>}
    {status.error && <ErrorBanner>{status.error}</ErrorBanner>}
    <label className="check-row switch-row">
      <Switch aria-label="System log access for the deployment account" checked={status.enabled} disabled={busy} onCheckedChange={enabled => { void change(enabled); }} />
      <span><strong>System log access for the deployment account</strong><small>Lets {account} read every service’s logs on this server with <code>journalctl -u &lt;unit&gt;</code> (new SSH sessions). Applications’ logs may contain secrets.</small></span>
    </label>
    <p className="muted-copy">{status.member ? <>{account} is in the <code>{status.group}</code> group now.</> : <>{account} is not in the <code>{status.group}</code> group now.</>} {status.source === 'config' ? 'Until changed here, the agent leaves the membership as it is.' : 'The agent puts the membership back to this setting when it is changed by hand.'}</p>
  </Card>;
}
