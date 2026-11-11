import { useCallback, useEffect, useState } from 'react';
import { ArrowRightLeft, RefreshCw } from 'lucide-react';
import { get, post } from '../lib/api';
import { formatBytes } from '../lib/format';
import { Input } from '../components/primitives/input';
import { Badge, Button, Card, ConfirmAction, ErrorBanner, Field, Loading, Notice, SectionHeader } from '../components/ui';
import { ResourceDetailPage } from '../components/resource';
import { Cluster, Stack } from '../components/layout';
import { CheckList, checkStatus, type CheckListItem, type CheckStatus } from '../components/CheckList';
import { SummaryList } from '../components/SummaryList';
import { ResourceList } from '../components/ResourceList';
import { DnsRecordsTable, dnsAddressRecords } from '../components/DnsRecords';

// Settings → Control panel: where this panel runs, and moving it to another
// server (GET/POST /api/control/panel-move). Built only from the shared
// blocks: SummaryList, CheckList, ResourceList, DnsRecordsTable, Cluster.

export interface PanelMoveCheck { name: string; ok: boolean; detail: string }
export interface PanelMoveServer { id: string; name: string; role: 'destination' | 'source' | 'remote'; url: string; new_url?: string; reachable: boolean; retire?: boolean; apps: number; detail?: string }
export interface PanelMovePlan {
  input: PanelMoveInput; hostname: string; target_name: string; source_server?: string; source_name?: string;
  servers: PanelMoveServer[]; addresses: string[]; data_files: number; data_bytes: number; database: string; database_bytes: number;
  ingress: string; ingress_detail: string; certificate: string; certificate_ok: boolean; version: string;
  checks: PanelMoveCheck[]; blockers: string[]; warnings: string[]; ready: boolean; keep_source?: boolean; source_link?: boolean;
}
export interface PanelMoveInput { target: string; keep_source?: boolean; source_url?: string; server_urls?: Record<string, string>; addresses?: string[] }
export interface PanelMoveStep { name: string; title: string; status: string; detail?: string; attempts?: number }
export interface PanelDNSStatus { hostname: string; addresses: string[]; ready: boolean; error?: string; source?: string }
export interface PanelMove {
  id: string; target: string; target_name: string; hostname: string; state: string; phase: string; read_only: boolean;
  plan: PanelMovePlan; addresses: string[]; steps: PanelMoveStep[]; log: Array<{ at: string; step?: string; message: string }>;
  error?: string; certificate?: string; dns?: PanelDNSStatus[]; dns_checked?: string; tls_ready?: boolean; tls_detail?: string; retire_detail?: string[];
}
export interface PanelMoveView {
  hostname: string; public_url: string; version: string; read_only: boolean; servers: Array<{ id: string; name: string }>;
  move?: PanelMove; arrival?: { from: string; at: string; hostname: string; retired?: PanelMoveServer[] } | null;
}

const activeStates = new Set(['running', 'standby', 'failed', 'dns', 'aborting']);
const stateLabels: Record<string, string> = { running: 'Running', standby: 'Standing by', failed: 'Failed', dns: 'Waiting for DNS', done: 'Moved', aborting: 'Aborting', aborted: 'Aborted' };
const stepStatus: Record<string, CheckStatus> = { done: 'ok', failed: 'failed', running: 'running', waiting: 'waiting' };

function message(reason: unknown) { return reason instanceof Error ? reason.message : String(reason); }

/** ControlPanelPage: where this panel runs (Overview) and moving it to another server (Move). */
export function ControlPanelPage({ tab = 'overview' }: { tab?: string }) {
  const [view, setView] = useState<PanelMoveView>();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState('');
  const [target, setTarget] = useState('');
  const [sourceURL, setSourceURL] = useState('');
  const [keepSource, setKeepSource] = useState(true);
  const [addresses, setAddresses] = useState('');
  const [overrides, setOverrides] = useState<Record<string, string>>({});
  const [plan, setPlan] = useState<PanelMovePlan>();

  const refresh = useCallback(async () => {
    try { setView(await get<PanelMoveView>('control/panel-move')); setError(''); } catch (reason) { setError(message(reason)); }
  }, []);
  const move = view?.move;
  const active = Boolean(move && activeStates.has(move.state));
  useEffect(() => { void refresh(); }, [refresh]);
  useEffect(() => {
    if (!active) return;
    const timer = window.setInterval(() => { if (!document.hidden) void refresh(); }, 3_000);
    return () => clearInterval(timer);
  }, [active, refresh]);

  const input = (): PanelMoveInput => ({
    target,
    keep_source: keepSource,
    ...(sourceURL.trim() ? { source_url: sourceURL.trim() } : {}),
    ...(Object.values(overrides).some(Boolean) ? { server_urls: Object.fromEntries(Object.entries(overrides).filter(([, v]) => v.trim()).map(([k, v]) => [k, v.trim()])) } : {}),
    ...(addresses.trim() ? { addresses: addresses.split(/[\s,]+/).filter(Boolean) } : {}),
  });
  const run = async (name: string, fn: () => Promise<unknown>) => {
    setBusy(name); setError('');
    try { await fn(); await refresh(); } catch (reason) { setError(message(reason)); } finally { setBusy(''); }
  };
  const review = () => run('plan', async () => setPlan(await post<PanelMovePlan>('control/panel-move/plan', input())));
  const start = () => run('start', async () => { await post<PanelMove>('control/panel-move', input()); setPlan(undefined); });
  const action = (name: string) => run(name, () => post<PanelMove>(`control/panel-move/${name}`));


  const others = (view?.servers || []).filter(server => server.id !== move?.target);
  const retired = view?.arrival?.retired || [];
  return <>
    <ResourceDetailPage resource="control-panel" tab={tab} title="Control panel" description="Where this control panel runs, and moving it to another server without changing its address." status={view && (view.read_only ? <Badge tone="warning">Read-only: moving</Badge> : <Badge tone="success">Active</Badge>)} facts={view ? [view.public_url, view.version] : undefined} notices={error ? <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner> : undefined}>
    {!view && !error && <Loading label="Loading the control panel…" />}
    {view && <div className="settings-stack">
      {tab === 'overview' && <Card className="form-card">
        <SectionHeader title="This control panel" actions={view.read_only ? <Badge tone="warning">Read-only: moving</Badge> : <Badge tone="success">Active</Badge>} />
        <SummaryList items={[{ label: 'Address', value: view.public_url }, { label: 'Release', value: view.version }]} />
        {view.arrival && <Notice>Moved here from {view.arrival.from} on {new Date(view.arrival.at).toLocaleString()}.{retired.length ? <> Left the server list: {retired.map(s => s.name || s.id).join(', ')}. Shut {retired.length === 1 ? 'it' : 'them'} down when nothing else needs {retired.length === 1 ? 'it' : 'them'}.</> : null}</Notice>}
      </Card>}

      {move && (active || move.state === 'done' || move.state === 'aborted') && <MoveProgress move={move} busy={busy} onAction={action} />}

      {tab === 'move' && !active && move?.state !== 'done' && <Card className="form-card">
        <SectionHeader title="Move to another server" description="Installs this panel on a paired server in standby, copies its state, database and certificate, then switches over at your signal. The address stays the same; you change its DNS records." />
        <div className="form-grid two">
          <Field label="Destination server"><select aria-label="Destination server" value={target} onChange={event => { setTarget(event.target.value); setPlan(undefined); }}><option value="">Choose a server</option>{others.map(server => <option key={server.id} value={server.id}>{server.name}</option>)}</select></Field>
          <Field label="Destination addresses" hint="Optional: the public IPv4/IPv6 DNS should point to; detected when empty."><Input value={addresses} onChange={event => setAddresses(event.target.value)} placeholder="203.0.113.9" /></Field>
        </div>
        <Field label="This server after the move" hint={keepSource ? 'It stays in the server list for applications; its agent connects out to the new panel over HTTPS, so no port is opened.' : 'It leaves the server list and its panel stops; it must hold no applications. Shut it down afterwards.'}><select aria-label="This server after the move" value={keepSource ? 'keep' : 'retire'} onChange={event => { setKeepSource(event.target.value === 'keep'); setPlan(undefined); }}><option value="keep">Keep {plan?.source_name || 'this server'} as a server</option><option value="retire">Retire {plan?.source_name || 'this server'} after the move</option></select></Field>
        {keepSource && plan?.servers.some(s => s.role === 'source') && <Field label="Agent URL of this server" hint="Optional: an HTTPS agent URL of it the destination reaches; without one its agent connects out to the new panel."><Input value={sourceURL} onChange={event => setSourceURL(event.target.value)} placeholder="https://agent.example.com" /></Field>}
        {plan?.servers.filter(s => s.role === 'remote' && !s.reachable).map(s => <Field key={s.id} label={`Agent URL of ${s.name}`} hint={s.detail}><Input aria-label={`Agent URL of ${s.name}`} value={overrides[s.id] || ''} onChange={event => setOverrides(current => ({ ...current, [s.id]: event.target.value }))} placeholder="https://agent.example.com" /></Field>)}
        <Cluster><Button disabled={!target || Boolean(busy)} pending={busy === 'plan'} onClick={review}>Review the move</Button></Cluster>
        {plan && <PlanReview plan={plan} />}
        {plan && <Cluster><Button variant="primary" disabled={!plan.ready || Boolean(busy)} pending={busy === 'start'} onClick={start}><ArrowRightLeft size={15} />Start the move</Button></Cluster>}
      </Card>}
    </div>}
    </ResourceDetailPage>
  </>;
}

/** planChecks lists the readiness checks, then the warnings, of a plan. */
export function planChecks(plan: PanelMovePlan): CheckListItem[] {
  return [
    ...plan.checks.map(check => ({ id: `check-${check.name}`, status: checkStatus(check.ok), children: check.detail })),
    ...plan.warnings.map(warning => ({ id: `warning-${warning}`, status: 'warning' as const, children: warning })),
  ];
}

export function PlanReview({ plan }: { plan: PanelMovePlan }) {
  return <Stack className="move-plan-review" aria-label="Move plan">
    <SummaryList aria-label="Move summary" items={[
      { label: 'Hostname', value: <code>{plan.hostname}</code> },
      { label: 'Destination', value: `${plan.target_name} · release ${plan.version}` },
      { label: 'State and keys', value: `${plan.data_files} files, ${formatBytes(plan.data_bytes)} (encrypted state, credentials and master key, setup marker)` },
      { label: 'Database', value: plan.database === 'managed' ? `${formatBytes(plan.database_bytes)}, copied with pg_dump and pg_restore` : 'External: the destination uses the same DATABASE_URL' },
      { label: 'HTTPS', value: plan.certificate },
      { label: 'Routing', value: plan.ingress_detail },
      { label: 'DNS', value: plan.addresses.join(', ') || 'unknown' },
    ]} />
    <CheckList aria-label="Checks" items={planChecks(plan)} />
    <ResourceList rows={plan.servers} rowKey={s => s.id} columns={[
      { id: 'server', label: 'Server', render: s => <Cluster>{s.name}<Badge tone={s.role === 'destination' ? 'accent' : 'neutral'}>{s.role}</Badge></Cluster> },
      { id: 'url', label: 'For the new panel', render: s => <Stack gap="sm">{s.retire ? <span>Leaves the server list (retire)</span> : <code>{s.new_url || s.url}</code>}{s.detail && <small className="muted-copy">{s.detail}</small>}</Stack> },
      { id: 'apps', label: 'Applications', render: s => s.apps },
    ]} />
    {plan.blockers.length > 0 && <ErrorBanner>{plan.blockers.join(' ')}</ErrorBanner>}
  </Stack>;
}

function MoveProgress({ move, busy, onAction }: { move: PanelMove; busy: string; onAction: (name: string) => void }) {
  const steps: CheckListItem[] = move.steps.map(step => ({ id: step.name, status: stepStatus[step.status] || 'pending', children: <span>{step.title}{(step.attempts || 0) > 1 && <span className="muted-copy"> · attempt {step.attempts}</span>}</span>, detail: step.detail }));
  const dns: CheckListItem[] = [
    ...(move.dns || []).map(status => ({ id: status.hostname, status: checkStatus(status.ready, 'waiting'), children: <span><code>{status.hostname}</code>: {status.error || status.addresses.join(', ') || 'no answer'}</span>, detail: status.source ? `Answered by ${status.source}` : undefined })),
    ...(move.tls_detail ? [{ id: 'tls', status: checkStatus(Boolean(move.tls_ready), 'waiting'), children: move.tls_detail }] : []),
  ];
  return <Card className="form-card">
    <SectionHeader title={`Moving to ${move.target_name}`} description={move.read_only ? 'This panel is read-only while it moves: changes answer “the control panel is moving”.' : 'This panel keeps working normally until the cutover.'} actions={<Badge tone={move.state === 'failed' ? 'danger' : move.state === 'done' ? 'success' : 'warning'}>{stateLabels[move.state] || move.state}</Badge>} />
    {move.error && <ErrorBanner>{move.error}</ErrorBanner>}
    <CheckList aria-label="Steps" items={steps} />
    {move.state === 'standby' && !move.certificate && <Notice>No certificate was copied: Caddy on {move.target_name} requests one once DNS points there, so HTTPS answers a minute or so after the switch.</Notice>}
    {(move.state === 'dns' || move.state === 'done') && <section className="move-dns" aria-label="DNS records">
      <h4>DNS records for {move.hostname}</h4>
      <p>Change these records at your DNS provider. The check asks the zone’s own nameservers, so it sees the change as soon as you save it.{move.state === 'dns' && ` Remove any other A/AAAA records of ${move.hostname} that point elsewhere.`}</p>
      <DnsRecordsTable records={dnsAddressRecords(move.hostname, move.addresses)} />
      <CheckList aria-label="DNS answers" items={dns} />
    </section>}
    {move.retire_detail?.length ? <CheckList aria-label="Retired servers" items={move.retire_detail.map(line => ({ id: line, status: 'info', children: line }))} /> : null}
    <Cluster>
      {move.state === 'standby' && <ConfirmAction title="Start the cutover?" description={<>This panel becomes read-only, copies its latest state and database to {move.target_name} (seconds) and activates it there. Then you change DNS. The move can no longer be aborted after this.</>} confirmLabel="Start the cutover" onConfirm={() => onAction('cutover')}><Button variant="primary" pending={busy === 'cutover'}><ArrowRightLeft size={15} />Cut over</Button></ConfirmAction>}
      {move.state === 'dns' && <Button pending={busy === 'check-dns'} onClick={() => onAction('check-dns')}><RefreshCw size={15} />Check DNS now</Button>}
      {move.state === 'failed' && <Button pending={busy === 'retry'} onClick={() => onAction('retry')}>Retry</Button>}
      {move.phase === 'prepare' && (move.state === 'running' || move.state === 'standby' || move.state === 'failed') && <ConfirmAction destructive title="Abort the move?" description={<>Removes the standby panel, its data and database from {move.target_name}. This panel stays as it is.</>} confirmLabel="Abort the move" onConfirm={() => onAction('abort')}><Button variant="danger" pending={busy === 'abort'}>Abort</Button></ConfirmAction>}
    </Cluster>
    <details className="move-log"><summary>Log ({move.log.length})</summary><ul>{move.log.map((line, index) => <li key={index}><time>{new Date(line.at).toLocaleTimeString()}</time> {line.message}</li>)}</ul></details>
  </Card>;
}
