import { useEffect, useState } from 'react';
import { get, post } from '../lib/api';
import { useToast } from './ToastProvider';
import { Badge, Button, Card, ErrorBanner, SectionHeader } from './ui';

/** What on a server would stop an application from running or moving there (control/servers/{id}/host/readiness). */
export type HostReadiness = { ok: boolean; checks: Array<{ id: string; title: string; ok: boolean; detail?: string; repairable?: boolean }>; repaired?: string[]; failed?: string[] };

const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);
export const readinessEndpoint = (serverID: string) => `control/servers/${encodeURIComponent(serverID)}/host/readiness`;

/**
 * ServerReadiness: the services applications depend on, on this server. The
 * agent repairs what has one safe answer by itself when an application needs
 * it; Repair now does the same on request. Hidden while every check passes
 * on servers whose agent has nothing to report.
 */
export function ServerReadiness({ serverID }: { serverID: string }) {
  const notify = useToast();
  const [report, setReport] = useState<HostReadiness>();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    let active = true;
    setReport(undefined); setError('');
    // An agent without this report (an older release) simply shows no card.
    get<HostReadiness>(readinessEndpoint(serverID)).then(value => { if (active) setReport(value); }).catch(() => { if (active) setReport(undefined); });
    return () => { active = false; };
  }, [serverID]);
  const repair = async () => {
    setBusy(true);
    try {
      const next = await post<HostReadiness>(`${readinessEndpoint(serverID)}/repair`, {});
      setReport(next); setError((next.failed || []).join(' '));
      if (next.repaired?.length) notify(`Repaired: ${next.repaired.join('; ')}.`);
    } catch (reason) { setError(message(reason)); } finally { setBusy(false); }
  };
  if (!report || !report.checks?.length) return null;
  const repairable = report.checks.some(check => !check.ok && check.repairable && check.id !== 'native');
  return <Card data-server-readiness={report.ok ? 'ready' : 'attention'}>
    <SectionHeader title="Ready for applications" description="The services applications need on this server. The agent repairs what it safely can when an application needs it." actions={<>{repairable && <Button type="button" size="sm" variant="primary" disabled={busy} onClick={() => { void repair(); }}>{busy ? 'Repairing…' : 'Repair now'}</Button>}<Badge tone={report.ok ? 'success' : 'warning'}>{report.ok ? 'Ready' : 'Needs attention'}</Badge></>} />
    {error && <ErrorBanner>{error}</ErrorBanner>}
    <div className="simple-list">{report.checks.map(check => <div key={check.id}><div><strong>{check.title}</strong><small>{check.detail}</small></div><Badge tone={check.ok ? 'success' : check.repairable ? 'warning' : 'danger'}>{check.ok ? 'OK' : check.repairable ? 'Repairable' : 'Needs you'}</Badge></div>)}</div>
  </Card>;
}
