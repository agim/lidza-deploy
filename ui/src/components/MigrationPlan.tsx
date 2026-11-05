import { useCallback, useEffect, useState } from 'react';
import { APIError, get, post } from '../lib/api';
import { Badge, Button, ConfirmAction, CopyButton, ErrorBanner, Field, Notice, StatusBadge } from './ui';

// Moving an application to another server: review the plan, start the move,
// follow its steps, switch DNS by hand and finish. The source keeps serving
// until the move finishes.

type ServerOption = { id: string; name: string };

type MovePlan = {
  app_id: string;
  source_server: string;
  target_server: string;
  source_runtime: string;
  runtime: string;
  target_runtime: string;
  runtime_choices: string[];
  detection?: { mode?: string; runtime?: string; framework?: string; error?: string };
  repository: string;
  branch: string;
  commit?: string;
  hostnames: string[];
  environment_keys: string[];
  copied_keys: string[];
  destination_keys: string[];
  databases: Array<{ id: string; engine: string; mode: string; tables: number; size_bytes?: number; action: string; note?: string }>;
  redis: boolean;
  volumes: string[];
  addresses: string[];
  capacity?: { disk_available: number; memory_available: number; needed: number };
  warnings: string[];
  notes: string[];
  blockers: string[];
};

type MoveStep = { key: string; title: string; status: string; log: Array<{ time: string; message: string }> };

export type MoveJob = {
  id: string;
  app_id: string;
  source_server: string;
  target_server: string;
  target_runtime: string;
  runtime: string;
  state: string;
  error?: string;
  steps: MoveStep[];
  hostnames: string[];
  addresses: string[];
  dns: Array<{ hostname: string; addresses: string[]; ready: boolean; error?: string }>;
  dns_checked?: string;
  tls_ready: boolean;
  tls_detail?: string;
  records: Array<{ hostname: string; type: string; value: string }>;
  actions: string[];
  source_frozen?: boolean;
  final_synced?: boolean;
  switched: boolean;
};

const terminal = (state?: string) => state === 'finished' || state === 'aborted';
const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);
const stateLabels: Record<string, string> = { running: 'In progress', failed: 'Failed', cutover: 'Waiting for DNS', ready: 'Ready to finish', syncing: 'Final sync', finishing: 'Finishing', finished: 'Finished', aborting: 'Aborting', aborted: 'Aborted' };
const actionLabels: Record<string, string> = { copy: 'copied', skip_empty: 'skipped: no tables', external: 'external: connection kept', manual: 'moved manually' };

function gib(bytes: number) { return `${(bytes / 2 ** 30).toFixed(1)} GiB`; }
function mib(bytes?: number) { return bytes ? `${(bytes / 2 ** 20).toFixed(1)} MiB` : ''; }

export function MigrationPlan({ appID, serverID, servers = [], onChanged, pollMs = 3000 }: { appID: string; serverID?: string; servers?: ServerOption[]; onChanged?: () => unknown; pollMs?: number }) {
  const [open, setOpen] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [job, setJob] = useState<MoveJob>();
  const [planning, setPlanning] = useState(false);
  const [error, setError] = useState('');
  const name = useCallback((id: string) => servers.find(server => server.id === id)?.name || id, [servers]);

  const load = async () => {
    setError('');
    try {
      const latest = await get<MoveJob>(`control/apps/${encodeURIComponent(appID)}/move`);
      setJob(latest);
      setPlanning(terminal(latest.state));
    } catch (reason) {
      if (!(reason instanceof APIError && reason.status === 404)) setError(message(reason));
      setJob(undefined);
      setPlanning(true);
    } finally { setLoaded(true); }
  };

  const jobID = job?.id;
  const jobState = job?.state;
  useEffect(() => {
    if (!open || !jobID || terminal(jobState)) return;
    let stopped = false;
    const timer = setInterval(async () => {
      try {
        const next = await get<MoveJob>(`control/moves/${encodeURIComponent(jobID)}`);
        if (stopped) return;
        setJob(next);
        if (terminal(next.state)) void onChanged?.();
      } catch (reason) { if (!stopped) setError(message(reason)); }
    }, pollMs);
    return () => { stopped = true; clearInterval(timer); };
  }, [open, jobID, jobState, pollMs, onChanged]);

  return <details className="migration-plan move-flow" onToggle={event => {
    const isOpen = event.currentTarget.open;
    setOpen(isOpen);
    if (isOpen) void load();
  }}>
    <summary>Move to another server</summary>
    <div className="settings-stack">
      {error && <ErrorBanner>{error}</ErrorBanner>}
      {!loaded && open && <p role="status">Checking for a move in progress…</p>}
      {loaded && job && !planning && <MoveProgress job={job} name={name} onUpdate={next => { setJob(next); if (terminal(next.state)) void onChanged?.(); }} onPlanAnother={() => setPlanning(true)} />}
      {loaded && planning && <MovePlanner appID={appID} serverID={serverID} servers={servers} name={name} previous={job} onStarted={next => { setJob(next); setPlanning(false); }} />}
    </div>
  </details>;
}

function MovePlanner({ appID, serverID, servers, name, previous, onStarted }: { appID: string; serverID?: string; servers: ServerOption[]; name: (id: string) => string; previous?: MoveJob; onStarted: (job: MoveJob) => void }) {
  const targets = servers.filter(server => server.id !== serverID);
  const [target, setTarget] = useState(targets[0]?.id || '');
  const [runtime, setRuntime] = useState('');
  const [addresses, setAddresses] = useState('');
  const [plan, setPlan] = useState<MovePlan>();
  const [acknowledged, setAcknowledged] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const addressList = addresses.split(/[\s,]+/).filter(Boolean);

  const review = async () => {
    setBusy(true); setError(''); setPlan(undefined); setAcknowledged(false);
    const query = new URLSearchParams({ target_server: target });
    if (runtime) query.set('runtime_override', runtime);
    if (addressList.length) query.set('addresses', addressList.join(','));
    try { setPlan(await get<MovePlan>(`control/apps/${encodeURIComponent(appID)}/move-plan?${query}`)); }
    catch (reason) { setError(message(reason)); }
    finally { setBusy(false); }
  };
  const start = async () => {
    setBusy(true); setError('');
    try {
      const body: Record<string, unknown> = { target_server: target, acknowledge_warnings: acknowledged };
      if (runtime) body.runtime_override = runtime;
      if (addressList.length) body.addresses = addressList;
      onStarted(await post<MoveJob>(`control/apps/${encodeURIComponent(appID)}/move`, body));
    } catch (reason) { setError(message(reason)); }
    finally { setBusy(false); }
  };

  if (!targets.length) return <p>Connect another server to move this application.</p>;
  const blocked = !plan || plan.blockers.length > 0 || (plan.warnings.length > 0 && !acknowledged);
  return <div className="move-planner">
    {previous && <p className="muted-copy">The last move to {name(previous.target_server)} was {stateLabels[previous.state]?.toLowerCase() || previous.state}.</p>}
    <p>The application keeps running here while the destination is prepared. You switch DNS yourself; the move finishes once DNS points to the destination.</p>
    <div className="form-grid two">
      <Field label="Destination server"><select aria-label="Destination server" value={target} onChange={event => { setTarget(event.target.value); setPlan(undefined); }}>{targets.map(server => <option key={server.id} value={server.id}>{server.name || server.id}</option>)}</select></Field>
      <Field label="Runtime on the destination"><select aria-label="Runtime on the destination" value={runtime} onChange={event => { setRuntime(event.target.value); setPlan(undefined); }}><option value="">Automatic</option><option value="native">Native</option><option value="container">Container</option></select></Field>
    </div>
    <Field label="DNS addresses (optional)" hint="Public IPv4 or IPv6 addresses DNS should point to. Leave empty to use the addresses the destination detects."><input aria-label="DNS addresses" value={addresses} onChange={event => { setAddresses(event.target.value); setPlan(undefined); }} placeholder="203.0.113.10, 2001:db8::10" /></Field>
    <div className="move-actions"><Button type="button" onClick={() => { void review(); }} disabled={busy || !target}>Review move</Button></div>
    {error && <ErrorBanner>{error}</ErrorBanner>}
    {plan && <div className="move-plan-review">
      <dl className="move-facts">
        <dt>Runtime</dt><dd>{plan.runtime} · {plan.source_runtime} → <strong>{plan.target_runtime}</strong>{plan.detection?.mode && ` (repository detected as ${plan.detection.mode}${plan.detection.runtime ? ` ${plan.detection.runtime}` : ''})`}</dd>
        <dt>Release</dt><dd>{plan.repository}@{plan.branch} · commit <code>{plan.commit?.slice(0, 12) || 'not deployed'}</code></dd>
        <dt>Hostnames</dt><dd>{plan.hostnames.join(', ')}</dd>
        <dt>DNS will point to</dt><dd>{plan.addresses.length ? plan.addresses.join(', ') : 'unknown'}</dd>
        <dt>Environment</dt><dd>{plan.copied_keys.length} copied{plan.copied_keys.length > 0 && <>: <code>{plan.copied_keys.join(', ')}</code></>}{plan.destination_keys.length > 0 && <>; set by the destination: <code>{plan.destination_keys.join(', ')}</code></>}</dd>
        <dt>Redis</dt><dd>{plan.redis ? 'provisioned on the destination; contents are not copied' : 'none'}</dd>
        {plan.capacity && <><dt>Destination capacity</dt><dd>{gib(plan.capacity.disk_available)} free disk (about {gib(plan.capacity.needed)} needed), {gib(plan.capacity.memory_available)} memory available</dd></>}
      </dl>
      {plan.databases.length > 0 && <><strong>Databases</strong><ul className="move-databases">{plan.databases.map(db => <li key={db.id}><code>{db.id}</code> {db.engine} ({db.mode}{db.tables >= 0 ? `, ${db.tables} tables` : ''}{db.size_bytes ? `, ${mib(db.size_bytes)}` : ''}) · {actionLabels[db.action] || db.action}{db.note && db.action !== 'skip_empty' ? ` · ${db.note}` : ''}</li>)}</ul></>}
      {plan.notes.length > 0 && <ul className="move-notes">{plan.notes.map(note => <li key={note}>{note}</li>)}</ul>}
      {plan.blockers.length > 0 && <div className="move-blockers" role="alert"><strong>Resolve before moving</strong><ul>{plan.blockers.map(item => <li key={item}>{item}</li>)}</ul></div>}
      {plan.warnings.length > 0 && <div className="move-warnings"><strong>Warnings</strong><ul>{plan.warnings.map(item => <li key={item}>{item}</li>)}</ul>
        <label className="check-row"><input type="checkbox" checked={acknowledged} onChange={event => setAcknowledged(event.target.checked)} /><span><strong>I have read these warnings</strong><small>Files and services listed above are handled manually.</small></span></label></div>}
      <div className="move-actions"><Button type="button" variant="primary" disabled={busy || blocked} onClick={() => { void start(); }}>Start move</Button></div>
    </div>}
  </div>;
}

function MoveProgress({ job, name, onUpdate, onPlanAnother }: { job: MoveJob; name: (id: string) => string; onUpdate: (job: MoveJob) => void; onPlanAnother: () => void }) {
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [proxied, setProxied] = useState(false);
  const [confirm, setConfirm] = useState('');
  const act = async (action: string, body?: unknown) => {
    setBusy(action); setError('');
    try { onUpdate(await post<MoveJob>(`control/moves/${encodeURIComponent(job.id)}/${action}`, body)); }
    catch (reason) { setError(message(reason)); }
    finally { setBusy(''); }
  };
  const can = (action: string) => job.actions.includes(action);
  const cutover = job.state === 'cutover' || job.state === 'ready';
  return <div className="move-progress">
    <div className="move-heading"><strong>Moving to {name(job.target_server)}</strong><Badge tone={job.state === 'failed' ? 'danger' : terminal(job.state) || job.state === 'ready' ? 'success' : 'warning'}>{stateLabels[job.state] || job.state}</Badge><span className="muted-copy">{job.runtime} · {job.target_runtime}</span></div>
    {job.error && <ErrorBanner>{job.error}</ErrorBanner>}
    {error && <ErrorBanner>{error}</ErrorBanner>}
    {job.state === 'finished' && <Notice>The application now runs on {name(job.target_server)}. It was retired on {name(job.source_server)}, where its databases and last backups remain as retained resources.</Notice>}
    {job.state === 'aborted' && <Notice>The move was aborted. The application keeps running on {name(job.source_server)}.</Notice>}
    <ol className="move-steps">{job.steps.map(step => <li key={step.key} className={`move-step move-step-${step.status}`}>
      <div className="move-step-heading"><StatusBadge status={step.status} /><span>{step.title}</span></div>
      {step.log.length > 0 && <details className="move-log"><summary>Log ({step.log.length})</summary><ul>{step.log.map((line, index) => <li key={index}><time dateTime={line.time}>{new Date(line.time).toLocaleTimeString()}</time> {line.message}</li>)}</ul></details>}
    </li>)}</ol>
    {cutover && <section className="move-dns" aria-label="DNS records">
      <h4>DNS records</h4>
      <p>Set these records at your DNS provider and remove every other A and AAAA record of these hostnames. {name(job.source_server)} keeps serving until DNS points to {name(job.target_server)}.</p>
      <table className="move-records"><thead><tr><th>Hostname</th><th>Type</th><th>Value</th><th><span className="sr-only">Copy</span></th></tr></thead>
        <tbody>{job.records.map(record => <tr key={`${record.hostname}-${record.type}-${record.value}`}><td><code>{record.hostname}</code></td><td>{record.type}</td><td><code>{record.value}</code></td><td><CopyButton value={record.value} label={`Copy ${record.type} value for ${record.hostname}`} /></td></tr>)}</tbody></table>
      <ul className="move-resolution">{job.dns.map(entry => <li key={entry.hostname} className={entry.ready ? 'move-dns-ready' : 'move-dns-waiting'}><code>{entry.hostname}</code> {entry.ready ? 'points to the destination' : entry.error ? entry.error : `resolves to ${entry.addresses.join(', ') || 'nothing'}`}</li>)}</ul>
      {job.dns_checked && <p className="muted-copy">Checked {new Date(job.dns_checked).toLocaleTimeString()}. DNS is checked every minute.</p>}
      {job.tls_detail && <p className="muted-copy">{job.tls_detail}</p>}
      {job.source_frozen && <p className="move-frozen">The source is in maintenance after the final sync. Switch DNS and finish soon.</p>}
    </section>}
    <div className="move-actions">
      {can('recheck-dns') && <Button type="button" disabled={!!busy} onClick={() => { void act('recheck-dns'); }}>I've switched DNS — check again</Button>}
      {can('final-sync') && <ConfirmAction title="Run the final sync?" description="The source goes into maintenance, so writes stop. Its databases are copied to the destination again and the destination is redeployed. The source stays in maintenance until the move finishes or is aborted." confirmLabel="Run final sync" onConfirm={() => act('final-sync')}><Button type="button" disabled={!!busy}>Final sync</Button></ConfirmAction>}
      {can('finish') && <Button type="button" variant="primary" disabled={!!busy || job.state !== 'ready'} onClick={() => { void act('finish'); }}>Finish move</Button>}
      {can('continue') && <Button type="button" disabled={!!busy} onClick={() => { void act('continue'); }}>{job.state === 'failed' ? 'Retry' : 'Resume'}</Button>}
      {can('abort') && <ConfirmAction title="Abort the move?" description={`The application created on ${name(job.target_server)} is removed. ${name(job.source_server)} keeps serving${job.source_frozen ? ' and leaves maintenance' : ''}.`} confirmLabel="Abort move" destructive onConfirm={() => act('abort')}><Button type="button" variant="danger" disabled={!!busy}>Abort move</Button></ConfirmAction>}
      {terminal(job.state) && <Button type="button" onClick={onPlanAnother}>Plan another move</Button>}
    </div>
    {job.state === 'cutover' && job.tls_ready && can('finish') && <div className="move-override">
      <label className="check-row"><input type="checkbox" checked={proxied} onChange={event => setProxied(event.target.checked)} /><span><strong>These hostnames are served through a proxy or CDN</strong><small>DNS then never resolves to the destination. HTTPS already works there; finish on your confirmation.</small></span></label>
      {proxied && <><Field label="Type the application id to confirm"><input aria-label="Application id" value={confirm} onChange={event => setConfirm(event.target.value)} /></Field>
        <Button type="button" variant="primary" disabled={!!busy || confirm !== job.app_id} onClick={() => { void act('finish', { override_dns: true, confirm }); }}>Finish without DNS match</Button></>}
    </div>}
  </div>;
}
