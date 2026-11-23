import { useEffect, useState } from 'react';
import { get, put } from '../lib/api';
import { formatBytes, relativeTime } from '../lib/format';
import type { AppAccessLogStatus } from '../lib/types';
import { useToast } from './ToastProvider';
import { SummaryList } from './SummaryList';
import { Badge, Card, CopyButton, ErrorBanner, Loading, Notice, SectionHeader } from './ui';
import { Switch } from './primitives/switch';

const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);
export const accessLogEndpoint = (appID: string) => `control/apps/${encodeURIComponent(appID)}/access-log`;

/**
 * AppAccessLog: the application's own access log, a file of the Caddy
 * access entries for its hostnames that only its account reads. On by
 * default; switching it off stops writing and removes the files. The agent
 * applies a change at once, without a restart or deploy.
 */
export function AppAccessLog({ appID, disabled = false }: { appID: string; disabled?: boolean }) {
  const notify = useToast();
  const [status, setStatus] = useState<AppAccessLogStatus>();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    let active = true;
    setStatus(undefined);
    get<AppAccessLogStatus>(accessLogEndpoint(appID)).then(value => { if (active) { setStatus(value); setError(''); } }).catch(reason => { if (active) setError(message(reason)); });
    return () => { active = false; };
  }, [appID]);
  const change = async (enabled: boolean) => {
    setBusy(true);
    try {
      const next = await put<AppAccessLogStatus>(accessLogEndpoint(appID), { enabled });
      setStatus(next);
      setError('');
      if (!next.error) notify(enabled ? 'Access log on. Caddy writes this application’s requests to its file.' : 'Access log off. Its files were removed.');
    } catch (reason) { setError(message(reason)); } finally { setBusy(false); }
  };
  const state = status?.supported && (status.enabled ? <Badge tone={status.active ? 'success' : status.error ? 'danger' : 'warning'}>{status.active ? 'Writing' : status.error ? 'Not writing' : 'Pending'}</Badge> : <Badge>Off</Badge>);
  const header = <SectionHeader title="Access log" description="This application’s own requests, as Caddy logs them, in a file only its account can read." actions={state} />;
  if (error && !status) return <Card className="form-card" data-access-log={appID}>{header}<ErrorBanner>{error}</ErrorBanner></Card>;
  if (!status) return <Card className="form-card">{header}<Loading label="Reading the access log setting…" /></Card>;
  if (!status.supported) return <Card className="form-card" data-access-log={appID}>{header}<p className="muted-copy">{status.reason || 'This application has no access log.'}</p></Card>;
  const account = status.account ? <code>{status.account}</code> : 'the application’s account';
  return <Card className="form-card" data-access-log={appID}>
    {header}
    {error && <ErrorBanner>{error}</ErrorBanner>}
    {status.enabled && status.error && <ErrorBanner>{status.error}</ErrorBanner>}
    {status.enabled && status.warning && <Notice>{status.warning}</Notice>}
    <label className="check-row switch-row">
      <Switch aria-label="Write an access log for this application" checked={status.enabled} disabled={busy || disabled} onCheckedChange={enabled => { void change(enabled); }} />
      <span><strong>Write an access log for this application</strong><small>Caddy writes each request to this application’s hostnames once, to a file that {account} and root read; no other application can. The requests are no longer repeated in the system journal. Switching it off removes the files and sends the requests to the journal again.</small></span>
    </label>
    {status.enabled && <SummaryList aria-label="Access log" items={[
      { label: 'File', value: <span className="input-action"><code>{status.path}</code><CopyButton value={status.path} label="Copy access log path" /></span> },
      { label: 'Environment', value: status.env_set ? <><code>{status.env}</code> names the file. {status.restart_note}</> : <><code>{status.env}</code> is set once Caddy writes the file. {status.restart_note}</> },
      { label: 'Readable by', value: <>{account}{status.group && <> (group <code>{status.group}</code>)</>} and root</> },
      Boolean(status.hosts?.length) && { label: 'Hostnames', value: status.hosts!.join(', ') },
      { label: 'Kept', value: <>Rotated files next to it (<code>access-*.log.gz</code>), up to 14 days{status.files ? ` · ${status.files} ${status.files === 1 ? 'file' : 'files'}, ${formatBytes(status.bytes)}` : ''}</> },
      status.checked && { label: 'Checked', value: `${relativeTime(status.checked)}${status.caddy_version ? ` · Caddy ${status.caddy_version}` : ''}` },
    ]} />}
  </Card>;
}
