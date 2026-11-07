import { useCallback, useEffect, useState } from 'react';
import { ArrowRightLeft, CheckCircle2, CircleAlert, Clock3, RefreshCw, XCircle } from 'lucide-react';
import { get, post } from '../lib/api';
import { formatBytes } from '../lib/format';
import { Input } from '../components/primitives/input';
import { Badge, Button, Card, ConfirmAction, CopyButton, ErrorBanner, Field, Loading, Notice, PageHeader, SectionHeader } from '../components/ui';

// Settings → Control panel: where this panel runs, and moving it to another
// server (GET/POST /api/control/panel-move).

export interface PanelMoveCheck { name: string; ok: boolean; detail: string }
export interface PanelMoveServer { id: string; name: string; role: 'destination' | 'source' | 'remote'; url: string; new_url?: string; reachable: boolean; retire?: boolean; apps: number; detail?: string }
export interface PanelMovePlan {
  input: PanelMoveInput; hostname: string; target_name: string; source_server?: string; source_name?: string;
  servers: PanelMoveServer[]; addresses: string[]; data_files: number; data_bytes: number; database: string; database_bytes: number;
  ingress: string; ingress_detail: string; certificate: string; certificate_ok: boolean; version: string;
  checks: PanelMoveCheck[]; blockers: string[]; warnings: string[]; ready: boolean;
}
export interface PanelMoveInput { target: string; source_url?: string; server_urls?: Record<string, string>; addresses?: string[] }
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

function message(reason: unknown) { return reason instanceof Error ? reason.message : String(reason); }

/** dnsRecords are the records the operator sets: one per destination address. */
export function dnsRecords(hostname: string, addresses: string[]) {
  return addresses.map(address => ({ name: hostname, type: address.includes(':') ? 'AAAA' : 'A', value: address }));
}

function StepIcon({ status }: { status: string }) {
  if (status === 'done') return <CheckCircle2 size={16} className="move-dns-ready" aria-hidden />;
  if (status === 'failed') return <XCircle size={16} className="move-frozen" aria-hidden />;
  if (status === 'running') return <RefreshCw size={16} className="spin" aria-hidden />;
  if (status === 'waiting') return <Clock3 size={16} className="move-dns-waiting" aria-hidden />;
  return <span aria-hidden style={{ width: 16, display: 'inline-block' }}>·</span>;
}

export function ControlPanelPage() {
  const [view, setView] = useState<PanelMoveView>();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState('');
  const [target, setTarget] = useState('');
  const [sourceURL, setSourceURL] = useState('');
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

  if (!view && !error) return <Loading label="Loading the control panel…" />;
  const others = (view?.servers || []).filter(server => server.id !== move?.target);
  return <>
    <PageHeader eyebrow="Settings" title="Control panel" description="Where this control panel runs, and moving it to another server without changing its address." />
    {error && <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner>}
    {view && <div className="settings-stack">
      <Card className="form-card">
        <SectionHeader title="This control panel" description={`${view.public_url} · release ${view.version}`} actions={view.read_only ? <Badge tone="warning">Read-only: moving</Badge> : <Badge tone="success">Active</Badge>} />
        {view.arrival && <Notice>Moved here from {view.arrival.from} on {new Date(view.arrival.at).toLocaleString()}.{view.arrival.retired?.length ? <> Left the server list: {view.arrival.retired.map(s => s.name || s.id).join(', ')}. Shut {view.arrival.retired.length === 1 ? 'it' : 'them'} down when nothing else needs {view.arrival.retired.length === 1 ? 'it' : 'them'}.</> : null}</Notice>}
      </Card>

      {move && (active || move.state === 'done' || move.state === 'aborted') && <MoveProgress move={move} busy={busy} onAction={action} />}

      {!active && move?.state !== 'done' && <Card className="form-card">
        <SectionHeader title="Move to another server" description="Installs this panel on a paired server in standby, copies its state, database and certificate, then switches over at your signal. The address stays the same; you change its DNS records." />
        <div className="form-grid two">
          <Field label="Destination server"><select aria-label="Destination server" value={target} onChange={event => { setTarget(event.target.value); setPlan(undefined); }}><option value="">Choose a server</option>{others.map(server => <option key={server.id} value={server.id}>{server.name}</option>)}</select></Field>
          <Field label="Destination addresses" hint="Optional: the public IPv4/IPv6 DNS should point to; detected when empty."><Input value={addresses} onChange={event => setAddresses(event.target.value)} placeholder="203.0.113.9" /></Field>
        </div>
        {plan?.servers.some(s => s.role === 'source' && s.apps > 0) && <Field label="Agent URL of this server" hint="This server still holds applications: give an HTTPS agent URL of it the destination reaches, or move its applications first."><Input value={sourceURL} onChange={event => setSourceURL(event.target.value)} placeholder="https://agent.example.com" /></Field>}
        {plan?.servers.filter(s => s.role === 'remote' && !s.reachable).map(s => <Field key={s.id} label={`Agent URL of ${s.name}`} hint={s.detail}><Input aria-label={`Agent URL of ${s.name}`} value={overrides[s.id] || ''} onChange={event => setOverrides(current => ({ ...current, [s.id]: event.target.value }))} placeholder="https://agent.example.com" /></Field>)}
        <div className="toolbar"><Button disabled={!target || Boolean(busy)} pending={busy === 'plan'} onClick={review}>Review the move</Button>{plan && <Button variant="primary" disabled={!plan.ready || Boolean(busy)} pending={busy === 'start'} onClick={start}><ArrowRightLeft size={16} />Start the move</Button>}</div>
        {plan && <PlanReview plan={plan} />}
      </Card>}
    </div>}
  </>;
}

export function PlanReview({ plan }: { plan: PanelMovePlan }) {
  return <div className="move-progress" aria-label="Move plan">
    <dl className="move-facts">
      <dt>Hostname</dt><dd>{plan.hostname}</dd>
      <dt>Destination</dt><dd>{plan.target_name} · release {plan.version}</dd>
      <dt>State and keys</dt><dd>{plan.data_files} files, {formatBytes(plan.data_bytes)} (encrypted state, credentials and master key, setup marker)</dd>
      <dt>Database</dt><dd>{plan.database === 'managed' ? `${formatBytes(plan.database_bytes)}, copied with pg_dump and pg_restore` : 'External: the destination uses the same DATABASE_URL'}</dd>
      <dt>HTTPS</dt><dd>{plan.certificate}</dd>
      <dt>Routing</dt><dd>{plan.ingress_detail}</dd>
      <dt>DNS</dt><dd>{plan.addresses.join(', ') || 'unknown'}</dd>
    </dl>
    <ul className="move-resolution" aria-label="Checks">{plan.checks.map(check => <li key={check.name}>{check.ok ? <CheckCircle2 size={14} className="move-dns-ready" aria-hidden /> : <CircleAlert size={14} className="move-frozen" aria-hidden />} {check.detail}</li>)}</ul>
    <table className="move-records"><thead><tr><th>Server</th><th>For the new panel</th><th>Applications</th></tr></thead><tbody>{plan.servers.map(s => <tr key={s.id}><td>{s.name} <Badge tone={s.role === 'destination' ? 'accent' : 'neutral'}>{s.role}</Badge></td><td>{s.retire ? 'Leaves the server list (retire)' : <code>{s.new_url || s.url}</code>}<span className="move-record-note">{s.detail}</span></td><td>{s.apps}</td></tr>)}</tbody></table>
    {plan.warnings.length > 0 && <div className="move-warnings"><strong>Warnings</strong><ul>{plan.warnings.map(w => <li key={w}>{w}</li>)}</ul></div>}
    {plan.blockers.length > 0 && <ErrorBanner>{plan.blockers.join(' ')}</ErrorBanner>}
  </div>;
}

function MoveProgress({ move, busy, onAction }: { move: PanelMove; busy: string; onAction: (name: string) => void }) {
  const records = dnsRecords(move.hostname, move.addresses);
  return <Card className="form-card">
    <SectionHeader title={`Moving to ${move.target_name}`} description={move.read_only ? 'This panel is read-only while it moves: changes answer “the control panel is moving”.' : 'This panel keeps working normally until the cutover.'} actions={<Badge tone={move.state === 'failed' ? 'danger' : move.state === 'done' ? 'success' : 'warning'}>{stateLabels[move.state] || move.state}</Badge>} />
    {move.error && <ErrorBanner>{move.error}</ErrorBanner>}
    <ol className="move-steps">{move.steps.map(step => <li key={step.name} className={`move-step move-step-${step.status}`}><div className="move-step-heading"><StepIcon status={step.status} /><strong>{step.title}</strong>{(step.attempts || 0) > 1 && <small>attempt {step.attempts}</small>}<span>{step.detail}</span></div></li>)}</ol>
    {move.state === 'standby' && !move.certificate && <Notice>No certificate was copied: Caddy on {move.target_name} requests one once DNS points there, so HTTPS answers a minute or so after the switch.</Notice>}
    {(move.state === 'dns' || move.state === 'done') && <div className="move-dns">
      <h4>DNS records for {move.hostname}</h4>
      <p>Change these records at your DNS provider. The check asks the zone’s own nameservers, so it sees the change as soon as you save it.</p>
      <table className="move-records"><thead><tr><th>Name</th><th>Type</th><th>Value</th><th /></tr></thead><tbody>{records.map(record => <tr key={record.value}><td><code>{record.name}</code></td><td>{record.type}</td><td><code>{record.value}</code></td><td><CopyButton value={record.value} label={`Copy ${record.type}`} /></td></tr>)}</tbody></table>
      <ul className="move-resolution">{(move.dns || []).map(status => <li key={status.hostname} className={status.ready ? 'move-dns-ready' : 'move-dns-waiting'}>{status.hostname}: {status.error || status.addresses.join(', ') || 'no answer'}{status.source ? ` (${status.source})` : ''}</li>)}</ul>
      {move.tls_detail && <p className={move.tls_ready ? 'move-dns-ready' : 'move-dns-waiting'}>{move.tls_detail}</p>}
      {move.state === 'dns' && <p>Remove any other A/AAAA records of {move.hostname} that point elsewhere.</p>}
    </div>}
    {move.retire_detail?.length ? <ul className="move-resolution">{move.retire_detail.map(line => <li key={line}>{line}</li>)}</ul> : null}
    <div className="toolbar">
      {move.state === 'standby' && <ConfirmAction title="Start the cutover?" description={<>This panel becomes read-only, copies its latest state and database to {move.target_name} (seconds) and activates it there. Then you change DNS. The move can no longer be aborted after this.</>} confirmLabel="Start the cutover" onConfirm={() => onAction('cutover')}><Button variant="primary" pending={busy === 'cutover'}><ArrowRightLeft size={16} />Cut over</Button></ConfirmAction>}
      {move.state === 'dns' && <Button pending={busy === 'check-dns'} onClick={() => onAction('check-dns')}><RefreshCw size={16} />Check DNS now</Button>}
      {move.state === 'failed' && <Button pending={busy === 'retry'} onClick={() => onAction('retry')}>Retry</Button>}
      {move.phase === 'prepare' && (move.state === 'running' || move.state === 'standby' || move.state === 'failed') && <ConfirmAction destructive title="Abort the move?" description={<>Removes the standby panel, its data and database from {move.target_name}. This panel stays as it is.</>} confirmLabel="Abort the move" onConfirm={() => onAction('abort')}><Button variant="danger" pending={busy === 'abort'}>Abort</Button></ConfirmAction>}
    </div>
    <details className="move-log"><summary>Log ({move.log.length})</summary><ul>{move.log.map((line, index) => <li key={index}><time>{new Date(line.at).toLocaleTimeString()}</time> {line.message}</li>)}</ul></details>
  </Card>;
}
