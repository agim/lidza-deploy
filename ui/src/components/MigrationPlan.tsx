import * as AlertDialog from '@radix-ui/react-alert-dialog';
import { type FormEvent, useCallback, useEffect, useId, useRef, useState } from 'react';
import { AlertTriangle, LoaderCircle } from 'lucide-react';
import { APIError, get, post } from '../lib/api';
import { ActionForm, Badge, Button, ConfirmAction, ErrorBanner, Field, Notice, StatusBadge } from './ui';
import { DnsRecordsTable } from './DnsRecords';
import { LiveProgress, type OperationProgress } from './LiveProgress';
import { buildMemorySummary } from '../lib/buildMemory';
import type { BuildMemoryAssessment, RedirectDomain } from '../lib/types';
import { lastOutput, moveActive, moveDuration, moveHolds, movePhaseLabel, moveTone, stateLabels, terminal, working, type MoveExcludedTable, type MoveJob, type MovePTRStatus, type MoveRetainedData } from '../lib/move';
import { MoveFailureNote, MoveStatusLine, useAppMoveLock } from './MoveLock';

export type { MoveJob } from '../lib/move';

// Moving an application to another server: review the plan, start the move,
// follow its steps, switch DNS by hand and finish. The source keeps serving
// and stays usable while the move copies in the background; the final sync
// locks it, copies its last state and redeploys the destination. Databases
// and persistent files are copied by the move itself; DNS changes only at
// the Switch DNS step.

type ServerOption = { id: string; name: string };

type PlanFileSet = { id: string; path: string; source: string; files: number; bytes: number; action: string; method?: string; note?: string; skipped?: number };

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
  redirect_domains?: RedirectDomain[];
  redirect_note?: string;
  environment_keys: string[];
  copied_keys: string[];
  copied_sources?: Record<string, number>;
  destination_keys: string[];
  /** replication: how a PostgreSQL database is copied (continuous or dump), with the reason for a dump and whether Prepare restarts the source once. */
  databases: Array<{ id: string; engine: string; mode: string; tables: number; size_bytes?: number; action: string; note?: string; before_deploy?: string; excluded_tables?: MoveExcludedTable[]; replication?: string; replication_reason?: string; replication_restart?: boolean }>;
  redis: boolean;
  volumes: string[];
  files?: PlanFileSet[];
  addresses: string[];
  capacity?: { disk_available: number; memory_available: number; needed: number };
  build_memory?: BuildMemoryAssessment;
  estimate?: MoveEstimate;
  /** Host ports the destination publishes again; mail: the checks of an application that publishes a mail port. */
  public_ports?: Array<{ port: number; protocol: string }>;
  mail?: { ports: Array<{ port: number; protocol: string }>; source_addresses: string[]; ptr: MovePTRStatus[]; records: Array<{ hostname: string; type: string; value: string; note?: string }> };
  max_attempts?: number;
  /** The requested database copy mode: auto or dump. */
  replication?: string;
  /** The final sync and the finish start by themselves once DNS points to the destination (default: every database replicates continuously). */
  auto_cutover?: boolean;
  warnings: string[];
  notes: string[];
  blockers: string[];
};

type MoveEstimate = {
  engines: Array<{ kind: string; object: string; label: string; bytes: number; objects: number; object_label: string; measured: boolean; bulk_seconds: number; final_seconds: number; basis: string; final_basis?: string }>;
  bulk_seconds: number;
  final_seconds: number;
  basis: string;
  notes?: string[];
};

const rowEstimate = (rows: number) => rows >= 1e6 ? `about ${(rows / 1e6).toFixed(1)}M rows` : rows >= 1e4 ? `about ${Math.round(rows / 1000)}k rows` : `about ${rows} rows`;
/** Estimated rows and size of a table; an unknown row estimate is left out, never shown on its own. */
export function tableEstimate(table: { rows: number; bytes: number }) {
  const parts = [table.rows >= 0 ? rowEstimate(table.rows) : '', table.bytes >= 0 ? size(table.bytes) : ''].filter(Boolean);
  return parts.length ? parts.join(', ') : 'not measured';
}

const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);
const sourceLabels: Array<[string, string]> = [['panel', 'from settings'], ['deploy defaults', 'from deploy defaults'], ['image', 'from image']];
// " (14 from settings, 6 from deploy defaults, 2 from image)"; empty when every variable is a setting.
export const environmentSources = (counts?: Record<string, number>) => {
  const parts = sourceLabels.filter(([source]) => (counts?.[source] || 0) > 0).map(([source, label]) => `${counts![source]} ${label}`);
  return parts.length && (counts?.['deploy defaults'] || counts?.image) ? ` (${parts.join(', ')})` : '';
};
const actionLabels: Record<string, string> = { copy: 'copied', skip_empty: 'skipped: no tables', external: 'external: connection kept', manual: 'moved manually' };
/** How a PostgreSQL database is copied: "continuous replication", or "dump and restore" with the reason. */
export function replicationLabel(db: { replication?: string; replication_reason?: string }) {
  if (!db.replication) return '';
  if (db.replication === 'continuous') return 'continuous replication';
  return db.replication_reason ? `dump and restore (${db.replication_reason})` : 'dump and restore';
}
const fileActionLabels: Record<string, string> = { copy: 'copied', skip_empty: 'skipped: no files', manual: 'not moved', blocked: 'cannot be moved' };
const methodLabels: Record<string, string> = { s3: 'through workspace storage', direct: 'directly through the control panel' };

function gib(bytes: number) { return `${(bytes / 2 ** 30).toFixed(1)} GiB`; }
// "25/tcp, 587/tcp"
const portList = (ports?: Array<{ port: number; protocol: string }>) => (ports || []).map(p => `${p.port}/${p.protocol || 'tcp'}`).join(', ');
// Reverse DNS of one address: what it is and, when wrong, what it should be.
export function ptrSummary(entry: MovePTRStatus) {
  const current = entry.names.length ? entry.names.join(', ') : 'none';
  return entry.ok ? `${entry.address}: reverse DNS is ${current}` : `${entry.address}: reverse DNS is ${current}; set it to ${entry.want} at the server provider`;
}
function mib(bytes?: number) { return bytes ? `${(bytes / 2 ** 20).toFixed(1)} MiB` : ''; }
function size(bytes: number) {
  if (bytes >= 2 ** 30) return gib(bytes);
  if (bytes >= 2 ** 20) return mib(bytes);
  return `${(bytes / 2 ** 10).toFixed(1)} KiB`;
}
function fileSummary(set: PlanFileSet) {
  if (set.files < 0) return 'size unknown';
  return `${set.files.toLocaleString()} ${set.files === 1 ? 'file' : 'files'}, ${size(Math.max(set.bytes, 0))}`;
}
function fileAction(set: PlanFileSet) {
  const action = fileActionLabels[set.action] || set.action;
  return set.action === 'copy' && set.method ? `${action} ${methodLabels[set.method] || set.method}` : action;
}
// Sockets and pipes in a set are runtime state the service recreates; the
// copy leaves them behind and the plan says how many.
function fileSkipped(set: PlanFileSet) {
  if (!set.skipped) return '';
  return ` · ${set.skipped.toLocaleString()} runtime ${set.skipped === 1 ? 'socket' : 'sockets'} skipped`;
}
// dnsAhead reports DNS that reaches the destination before it serves the
// application: visitors get an error until the deploy finishes.
function dnsAhead(job: MoveJob) {
  if (terminal(job.state) || job.switched) return false;
  const deploy = job.steps.find(step => step.key === 'deploy');
  if (!deploy || deploy.status === 'done' || deploy.status === 'skipped') return false;
  return !!job.dns_ahead || job.dns.some(entry => entry.addresses.some(address => job.addresses.includes(address)));
}

// held: the application list reports a move that holds the application; the
// collapsed summary then follows it and shows its status instead of its title.
export function MigrationPlan({ appID, serverID, servers = [], onChanged, pollMs = 3000, defaultOpen = false, onOpenChange, held = false }: { appID: string; serverID?: string; servers?: ServerOption[]; onChanged?: () => unknown; pollMs?: number; defaultOpen?: boolean; onOpenChange?: (open: boolean) => void; held?: boolean }) {
  const [open, setOpen] = useState(defaultOpen);
  const [loaded, setLoaded] = useState(false);
  const [job, setJob] = useState<MoveJob>();
  const [planning, setPlanning] = useState(false);
  const [error, setError] = useState('');
  const name = useCallback((id: string) => servers.find(server => server.id === id)?.name || id, [servers]);
  // Collapsed, the summary follows the move; open, the panel shows it in full.
  const lock = useAppMoveLock(appID, { watch: held && !open, pollMs });
  const status = open ? undefined : lock.job;

  const load = async () => {
    setError('');
    try {
      const latest = await get<MoveJob>(`control/apps/${encodeURIComponent(appID)}/move`);
      setJob(latest);
      setPlanning(!latest || terminal(latest.state));
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

  // A panel opened from the URL (?move=<app>) loads like one opened by hand.
  const loadOnOpen = useRef(load);
  loadOnOpen.current = load;
  useEffect(() => { if (open) void loadOnOpen.current(); }, [open]);

  return <details className="migration-plan move-flow" open={open} onToggle={event => {
    const isOpen = event.currentTarget.open;
    if (isOpen === open) return;
    setOpen(isOpen);
    onOpenChange?.(isOpen);
  }}>
    <summary className={moveActive(status) ? (moveHolds(status) ? 'move-summary-active move-summary-locked' : 'move-summary-active') : undefined}>{status && moveActive(status) ? <MoveStatusLine job={status} target={name(status.target_server)} /> : 'Move to another server'}</summary>
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
  const [attempts, setAttempts] = useState('6');
  // How PostgreSQL databases are copied: continuous replication when the
  // databases allow it (auto), or always a dump and restore.
  const [replication, setReplication] = useState('auto');
  // Choices for tables whose data the backups leave out: copy unless left behind.
  const [left, setLeft] = useState<Record<string, Record<string, boolean>>>({});
  // The automatic cutover: the plan's default until the operator chooses.
  const [autoCutover, setAutoCutover] = useState<boolean>();
  const addressList = addresses.split(/[\s,]+/).filter(Boolean);

  const review = async () => {
    setBusy(true); setError(''); setPlan(undefined); setAcknowledged(false); setAutoCutover(undefined);
    const query = new URLSearchParams({ target_server: target });
    if (runtime) query.set('runtime_override', runtime);
    if (replication !== 'auto') query.set('replication', replication);
    if (addressList.length) query.set('addresses', addressList.join(','));
    try { setPlan(await get<MovePlan>(`control/apps/${encodeURIComponent(appID)}/move-plan?${query}`)); }
    catch (reason) { setError(message(reason)); }
    finally { setBusy(false); }
  };
  const start = async () => {
    setBusy(true); setError('');
    try {
      const body: Record<string, unknown> = { target_server: target, acknowledge_warnings: acknowledged, max_attempts: Number(attempts) };
      if (runtime) body.runtime_override = runtime;
      if (replication !== 'auto') body.replication = replication;
      if (autoCutover !== undefined) body.auto_cutover = autoCutover;
      const choices: Record<string, Record<string, string>> = {};
      for (const db of plan?.databases || []) for (const table of db.excluded_tables || []) (choices[db.id] ||= {})[table.name] = left[db.id]?.[table.name] ? 'leave' : 'copy';
      if (Object.keys(choices).length) body.excluded_tables = choices;
      if (addressList.length) body.addresses = addressList;
      onStarted(await post<MoveJob>(`control/apps/${encodeURIComponent(appID)}/move`, body));
    } catch (reason) { setError(message(reason)); }
    finally { setBusy(false); }
  };

  if (!targets.length) return <p>Connect another server to move this application.</p>;
  const blocked = !plan || plan.blockers.length > 0 || (plan.warnings.length > 0 && !acknowledged);
  const automatic = autoCutover ?? plan?.auto_cutover ?? false;
  return <div className="move-planner">
    {previous && <p className="muted-copy">The last move to {name(previous.target_server)} was {stateLabels[previous.state]?.toLowerCase() || previous.state}.</p>}
    <p>The application keeps running here while the destination is prepared. You switch DNS yourself; the move finishes once DNS points to the destination.</p>
    <div className="form-grid two">
      <Field label="Destination server"><select aria-label="Destination server" value={target} onChange={event => { setTarget(event.target.value); setPlan(undefined); }}>{targets.map(server => <option key={server.id} value={server.id}>{server.name || server.id}</option>)}</select></Field>
      <Field label="Runtime on the destination"><select aria-label="Runtime on the destination" value={runtime} onChange={event => { setRuntime(event.target.value); setPlan(undefined); }}><option value="">Automatic</option><option value="native">Native</option><option value="container">Container</option></select></Field>
    </div>
    <div className="form-grid two">
      <Field label="Automatic attempts per step" hint="Transient failures (network, a restarting agent, a stalled transfer) are retried after 30s, 1m, 2m, 5m and 10m."><select aria-label="Automatic attempts per step" value={attempts} onChange={event => setAttempts(event.target.value)}>{['1', '3', '6', '8', '10'].map(value => <option key={value} value={value}>{value === '1' ? 'No automatic retry' : `${value} attempts`}</option>)}</select></Field>
      <Field label="Database copy" hint="Continuous replication keeps the destination following the source, so the locked final sync takes seconds; the plan shows the mode per database and falls back to a dump when a database cannot replicate."><select aria-label="Database copy" value={replication} onChange={event => { setReplication(event.target.value); setPlan(undefined); }}><option value="auto">Automatic (continuous replication when possible)</option><option value="dump">Dump and restore</option></select></Field>
    </div>
    <Field label="DNS addresses (optional)" hint="Public IPv4 or IPv6 addresses DNS should point to. Leave empty to use the addresses the destination detects."><input aria-label="DNS addresses" autoComplete="off" value={addresses} onChange={event => { setAddresses(event.target.value); setPlan(undefined); }} placeholder="203.0.113.10, 2001:db8::10" /></Field>
    <div className="move-actions"><Button type="button" onClick={() => { void review(); }} disabled={busy || !target}>Review move</Button></div>
    {error && <ErrorBanner>{error}</ErrorBanner>}
    {plan && <div className="move-plan-review">
      <dl className="move-facts">
        <dt>Runtime</dt><dd>{plan.runtime} · {plan.source_runtime} → <strong>{plan.target_runtime}</strong>{plan.detection?.mode && ` (repository detected as ${plan.detection.mode}${plan.detection.runtime ? ` ${plan.detection.runtime}` : ''})`}</dd>
        <dt>Release</dt><dd>{plan.repository}@{plan.branch} · commit <code>{plan.commit?.slice(0, 12) || 'not deployed'}</code></dd>
        <dt>Hostnames</dt><dd>{plan.hostnames.join(', ')}</dd>
        {(plan.redirect_domains?.length ?? 0) > 0 && <><dt>Redirect domains</dt><dd>{plan.redirect_domains!.map(item => `${item.host} → ${item.status || 301}`).join(', ')}{plan.redirect_note && <span className="muted-copy"> {plan.redirect_note}</span>}</dd></>}
        <dt>DNS will point to</dt><dd>{plan.addresses.length ? plan.addresses.join(', ') : 'unknown'}</dd>
        <dt>Environment</dt><dd>{plan.copied_keys.length} copied{environmentSources(plan.copied_sources)}{plan.copied_keys.length > 0 && <>: <code>{plan.copied_keys.join(', ')}</code></>}{plan.destination_keys.length > 0 && <>; set by the destination: <code>{plan.destination_keys.join(', ')}</code></>}</dd>
        <dt>Redis</dt><dd>{plan.redis ? 'provisioned on the destination; contents are not copied' : 'none'}</dd>
        {(plan.public_ports?.length ?? 0) > 0 && <><dt>Published ports</dt><dd>{portList(plan.public_ports)}{plan.target_runtime === 'container' ? ' · opened by the destination agent' : ' · not published by the native runtime'}</dd></>}
        {plan.mail && <><dt>Outgoing mail</dt><dd className="move-mail">port {portList(plan.mail.ports)}<ul className="move-ptr">{plan.mail.ptr.map(entry => <li key={entry.address} className={entry.ok ? 'move-dns-ready' : 'move-dns-waiting'}>{ptrSummary(entry)}</li>)}</ul>{plan.mail.records.length > 0 && <span className="muted-copy">The cutover lists {plan.mail.records.length === 1 ? 'the mail record' : `${plan.mail.records.length} mail records`} (SPF, reverse DNS) with the A/AAAA records.</span>}</dd></>}
        {plan.build_memory && <><dt>Build memory</dt><dd className={`move-build-memory move-build-memory-${plan.build_memory.level}`}>{buildMemorySummary(plan.build_memory)}</dd></>}
        {plan.capacity && <><dt>Destination capacity</dt><dd>{gib(plan.capacity.disk_available)} free disk (about {gib(plan.capacity.needed)} needed), {gib(plan.capacity.memory_available)} memory available</dd></>}
      </dl>
      <div className="move-caution" role="note"><strong>Don't change DNS until the move reaches Switch DNS.</strong> The destination serves the application only after it is deployed there; DNS pointing to it earlier takes the site down until the deploy finishes. The move shows the records to set when it is time.</div>
      {plan.databases.length > 0 && <><strong>Databases</strong><ul className="move-databases">{plan.databases.map(db => db.engine === 'clickhouse' ? <li key={`clickhouse-${db.id}`}>ClickHouse <code>{db.id}</code>: {[db.size_bytes ? mib(db.size_bytes) : '', db.tables >= 0 ? `${db.tables} tables` : ''].filter(Boolean).join(', ') || db.mode} · {actionLabels[db.action] || db.action}{db.note && db.action !== 'skip_empty' ? ` · ${db.note}` : ''}</li> : <li key={db.id}><code>{db.id}</code> {db.engine} ({db.mode}{db.tables >= 0 ? `, ${db.tables} tables` : ''}{db.size_bytes ? `, ${mib(db.size_bytes)}` : ''}) · {actionLabels[db.action] || db.action}{db.action === 'copy' && replicationLabel(db) ? <> · <strong>{replicationLabel(db)}</strong>{db.replication_restart ? ' · restarts the source once for wal_level=logical' : ''}</> : ''}{db.note && db.action !== 'skip_empty' ? ` · ${db.note}` : ''}{db.before_deploy ? ` · before deploys: ${db.before_deploy}` : ''}</li>)}</ul></>}
      {plan.databases.some(db => db.excluded_tables?.length) && <section className="move-excluded" aria-label="Tables the backups skip">
        <strong>Tables the backups skip</strong>
        <p className="muted-copy">The source's backups skip the rows of these tables. Choose before starting: the move copies their data separately (the default), or leaves a table behind (schema only, empty on the destination).</p>
        {plan.databases.filter(db => db.excluded_tables?.length).map(db => <ul key={db.id} className="move-excluded-tables">{db.excluded_tables!.map(table => {
          const leave = !!left[db.id]?.[table.name];
          return <li key={table.name}><span><strong><code>{db.id}</code> {table.name}</strong> <span className="muted-copy">{tableEstimate(table)} · {leave ? 'left behind (schema only)' : 'data copied'}</span></span>
            <span className="move-excluded-choice" role="group" aria-label={`Data of ${table.name}`}>{(['copy', 'leave'] as const).map(choice => <Button key={choice} type="button" size="sm" variant={(choice === 'leave') === leave ? 'primary' : undefined} aria-pressed={(choice === 'leave') === leave} onClick={() => setLeft(current => ({ ...current, [db.id]: { ...current[db.id], [table.name]: choice === 'leave' } }))}>{choice === 'copy' ? 'Copy data' : 'Leave behind'}</Button>)}</span></li>;
        })}</ul>)}
      </section>}
      {plan.estimate && <MoveEstimateTable estimate={plan.estimate} />}
      <label className="check-row move-auto-cutover"><input type="checkbox" checked={automatic} onChange={event => setAutoCutover(event.target.checked)} /><span><strong>Final sync and finish automatically once DNS points to the destination</strong><small>{plan.estimate ? `The application is locked for about ${moveDuration(plan.estimate.final_seconds)} while the final sync runs (estimate based on ${plan.estimate.basis}), within a minute of DNS resolving to the destination. ` : ''}Off, the destination serves the bulk copy until you run the final sync and finish yourself; what it receives meanwhile is replaced.</small></span></label>
      {(plan.files?.length ?? 0) > 0 && <><strong>Files</strong><ul className="move-files">{plan.files!.map(set => <li key={set.id}><code>{set.path}</code> {set.source} ({fileSummary(set)}) · {fileAction(set)}{set.note ? ` · ${set.note}` : ''}{fileSkipped(set)}</li>)}</ul></>}
      {plan.notes.length > 0 && <ul className="move-notes">{plan.notes.map(note => <li key={note}>{note}</li>)}</ul>}
      {plan.blockers.length > 0 && <div className="move-blockers" role="alert"><strong>Resolve before moving</strong><ul>{plan.blockers.map(item => <li key={item}>{item}</li>)}</ul></div>}
      {plan.warnings.length > 0 && <div className="move-warnings"><strong>Warnings</strong><ul>{plan.warnings.map(item => <li key={item}>{item}</li>)}</ul>
        <label className="check-row"><input type="checkbox" checked={acknowledged} onChange={event => setAcknowledged(event.target.checked)} /><span><strong>I have read these warnings</strong><small>Anything listed above as not moved automatically is up to you.</small></span></label></div>}
      <div className="move-actions"><Button type="button" variant="primary" disabled={busy || blocked} onClick={() => { void start(); }}>Start move</Button></div>
    </div>}
  </div>;
}

/**
 * RemoveRetainedAction removes, from the move panel, the retained data on
 * the destination that stopped the Prepare step: the same confirmation and
 * final-backup choice as the retained-resources page. The move continues
 * once the removal finished.
 */
function RemoveRetainedAction({ retained, serverName, busy, onConfirm }: { retained: MoveRetainedData; serverName: string; busy: boolean; onConfirm: (body: { confirm: string; final_backup: boolean }) => Promise<void> }) {
  const inputID = useId();
  const [open, setOpen] = useState(false);
  const [confirm, setConfirm] = useState('');
  const [finalBackup, setFinalBackup] = useState(retained.final_backup_required);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState('');
  const required = retained.final_backup_required;
  const ready = confirm.trim() === retained.id;
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!ready || pending) return;
    setPending(true); setError('');
    try { await onConfirm({ confirm: retained.id, final_backup: finalBackup || required }); setOpen(false); }
    catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); }
    finally { setPending(false); }
  };
  const lastBackup = retained.last_backup ? `${retained.offsite ? 'Off-site' : 'Local'} backup from ${new Date(retained.last_backup).toLocaleString()}.` : 'No backup of its databases is recorded.';
  return <AlertDialog.Root open={open} onOpenChange={value => { if (!pending) { setOpen(value); if (value) { setConfirm(''); setError(''); setFinalBackup(required); } } }}>
    <AlertDialog.Trigger asChild><Button type="button" variant="danger" disabled={busy}>{retained.label}</Button></AlertDialog.Trigger>
    <AlertDialog.Portal><AlertDialog.Overlay className="dialog-overlay" /><AlertDialog.Content className="dialog">
      <ActionForm className="dialog-form" onSubmit={submit}>
        <AlertDialog.Title>Remove retained data of {retained.app_id} on {serverName}?</AlertDialog.Title>
        <AlertDialog.Description>{serverName} keeps data of an application with this id ({retained.databases === 1 ? '1 database' : `${retained.databases} databases`}). {lastBackup} Removing it deletes the data on {serverName}; backup records and off-site copies are kept. The move then creates the application there and continues.</AlertDialog.Description>
        {retained.databases > 0 && <label className="check-row"><input type="checkbox" checked={finalBackup || required} disabled={required || pending} onChange={event => setFinalBackup(event.target.checked)} /><span><strong>Take a final backup first</strong><small>{required ? 'Required: no verified backup was taken after the application stopped writing.' : 'Uploaded off-site when off-site storage is configured.'}</small></span></label>}
        <label className="field" htmlFor={inputID}><span className="field-label">Type <code>{retained.id}</code> to confirm</span><input id={inputID} autoComplete="off" spellCheck={false} value={confirm} onChange={event => setConfirm(event.target.value)} disabled={pending} required /></label>
        {error && <div className="dialog-error" role="alert"><AlertTriangle size={17} /><span>{error}</span></div>}
        <div className="dialog-actions"><AlertDialog.Cancel asChild><Button type="button" disabled={pending}>Cancel</Button></AlertDialog.Cancel><Button type="submit" variant="danger" disabled={pending || !ready}>{pending && <LoaderCircle className="spin" size={16} />}Remove and continue</Button></div>
      </ActionForm>
    </AlertDialog.Content></AlertDialog.Portal>
  </AlertDialog.Root>;
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
  const [acceptLock, setAcceptLock] = useState(false);
  const lockEstimate = job.lock_estimate;
  const excluded = (job.databases || []).filter(db => db.excluded_tables?.length);
  const replicated = (job.databases || []).filter(db => db.copyable && db.replication);
  return <div className="move-progress">
    <div className="move-heading"><strong>Moving to {name(job.target_server)}</strong><Badge tone={moveTone(job.state)}>{stateLabels[job.state] || job.state}</Badge>{moveActive(job) && <span className={moveHolds(job) ? 'move-phase move-phase-locked' : 'move-phase move-phase-live'}>{movePhaseLabel(job)}</span>}<span className="muted-copy">{job.runtime} · {job.target_runtime}</span></div>
    {job.failure ? <MoveFailureNote failure={job.failure} /> : job.error && <ErrorBanner>{job.error}</ErrorBanner>}
    {job.stall && <div className="move-caution" role="status"><strong>No progress since {new Date(job.stall.since).toLocaleTimeString()}</strong> while {job.stall.phase}. The attempt is cancelled and retried if nothing moves by {new Date(job.stall.cancel_at).toLocaleTimeString()}.</div>}
    {job.destination_stale && !terminal(job.state) && <p className="move-stale" role="status">The source was deployed during the move; the destination redeploys the source's current commit at the final sync.</p>}
    {replicated.length > 0 && !terminal(job.state) && <p className="muted-copy move-replication" role="status">{replicated.map(db => `${db.source_id}: ${replicationLabel(db)}${db.replication === 'continuous' && db.replication_link ? ` over the ${db.replication_link.path} path${db.replication_link.tls ? ' with TLS' : ''}` : ''}`).join('; ')}.</p>}
    {error && <ErrorBanner>{error}</ErrorBanner>}
    {job.state === 'finished' && <Notice>The application now runs on {name(job.target_server)}. It was retired on {name(job.source_server)}, where its databases and last backups remain as retained resources.</Notice>}
    {job.state === 'aborted' && <Notice>The move was aborted. The application keeps running on {name(job.source_server)}.</Notice>}
    {dnsAhead(job) && <div className="move-caution" role="alert"><strong>DNS already points to {name(job.target_server)}, but the application is not deployed there yet.</strong> The site is down for visitors who reach {name(job.target_server)} until the deploy finishes. Restore the previous records to keep it up, or wait for the deploy step.</div>}
    {!cutover && !terminal(job.state) && !dnsAhead(job) && <p className="muted-copy">Keep DNS unchanged until the move reaches Switch DNS.</p>}
    <ol className="move-steps">{job.steps.map(step => <li key={step.key} className={`move-step move-step-${step.status}`}>
      <div className="move-step-heading"><StatusBadge status={step.status} /><span>{step.title}</span></div>
      {step.status === 'running' && working(job.state) && <LiveProgress startedAt={step.started} activityAt={lastOutput(step)} activityKey={step.log.filter(line => !line.heartbeat).length} progress={step.progress} />}
      {step.log.length > 0 && <details className="move-log"><summary>Log ({step.log.length})</summary><ul>{step.log.map((line, index) => <li key={index}><time dateTime={line.time}>{new Date(line.time).toLocaleTimeString()}</time> {line.message}</li>)}</ul></details>}
    </li>)}</ol>
    {cutover && <section className="move-dns" aria-label="DNS records">
      <h4>DNS records</h4>
      <p>Set these records at your DNS provider and remove every other A and AAAA record of these hostnames. {name(job.source_server)} keeps serving until DNS points to {name(job.target_server)}.{job.records.some(record => record.type === 'PTR') && ' A PTR record is set at the server provider, where the server was created.'}</p>
      <DnsRecordsTable records={job.records} />
      <ul className="move-resolution">{job.dns.map(entry => <li key={entry.hostname} className={entry.ready ? 'move-dns-ready' : 'move-dns-waiting'}><code>{entry.hostname}</code> {entry.ready ? 'points to the destination' : entry.error ? entry.error : `resolves to ${entry.addresses.join(', ') || 'nothing'}`}{entry.source && <small className="muted-copy"> (answered by {entry.source})</small>}</li>)}{(job.ptr || []).map(entry => <li key={`ptr-${entry.address}`} className={entry.ok ? 'move-dns-ready' : 'move-dns-optional'}>{ptrSummary(entry)}{entry.ok ? '' : <small className="muted-copy"> Optional: recommended for outgoing mail; the move does not wait for it.</small>}</li>)}</ul>
      {job.dns_checked && <p className="muted-copy">Checked {new Date(job.dns_checked).toLocaleTimeString()}. DNS is checked every minute at the zone's own nameservers, so resolver caches do not delay the cutover.</p>}
      {job.tls_detail && <p className="muted-copy">{job.tls_detail}</p>}
      {can('auto-cutover') && <label className="check-row move-auto-cutover"><input type="checkbox" role="switch" aria-checked={!!job.auto_cutover} checked={!!job.auto_cutover} disabled={!!busy} onChange={event => { void act('auto-cutover', { enabled: event.target.checked }); }} /><span><strong>Final sync and finish start automatically when DNS points here</strong><small>{job.auto_cutover ? `On: the application is locked${lockEstimate ? ` for about ${moveDuration(lockEstimate.seconds)}` : ''} as soon as DNS resolves to ${name(job.target_server)} and HTTPS works there; the move then finishes by itself.` : 'Off: run the final sync and finish the move yourself once DNS points here. Until then the destination serves the bulk copy, and what it receives is replaced by the final sync.'}</small></span></label>}
      {job.source_frozen && <p className="move-frozen">The source is in maintenance after the final sync. Switch DNS and finish soon.</p>}
    </section>}
    <div className="move-actions">
      {can('recheck-dns') && <Button type="button" disabled={!!busy} onClick={() => { void act('recheck-dns'); }}>I've switched DNS — check again</Button>}
      {can('final-sync') && <ConfirmAction title="Run the final sync?" description={`The application is locked and the source goes into maintenance, so writes stop. Changes made during the move are picked up: the environment is exported again, the databases and files are copied again and the destination is redeployed with the source's current commit. The source stays in maintenance until the move finishes or is aborted.${lockEstimate ? ` Expected lock: about ${moveDuration(lockEstimate.seconds)} (estimate from ${lockEstimate.basis}).` : ''}`} confirmLabel="Run final sync" onConfirm={() => act('final-sync', lockEstimate?.confirm ? { confirm_lock: true } : undefined)}><Button type="button" disabled={!!busy || (!!lockEstimate?.confirm && !acceptLock)} title={lockEstimate?.confirm && !acceptLock ? 'Confirm the expected lock duration first.' : undefined}>Final sync</Button></ConfirmAction>}
      {can('retry-now') && <Button type="button" disabled={!!busy} onClick={() => { void act('retry-now'); }}>Retry now</Button>}
      {can('finish') && <Button type="button" variant="primary" disabled={!!busy || job.state !== 'ready'} title={job.state === 'ready' ? undefined : 'Available once every hostname resolves to the destination and HTTPS answers there.'} onClick={() => { void act('finish'); }}>Finish move</Button>}
      {can('remove-retained') && job.failure?.retained && <RemoveRetainedAction retained={job.failure.retained} serverName={name(job.failure.retained.server)} busy={!!busy} onConfirm={body => act('remove-retained', body)} />}
      {can('continue') && <Button type="button" disabled={!!busy} onClick={() => { void act('continue'); }}>{job.state === 'failed' ? 'Retry' : 'Resume'}</Button>}
      {can('abort') && <ConfirmAction title="Abort the move?" description={`The application created on ${name(job.target_server)} is removed. ${name(job.source_server)} keeps serving${job.source_frozen ? ' and leaves maintenance' : ''}.`} confirmLabel="Abort move" destructive onConfirm={() => act('abort')}><Button type="button" variant="danger" disabled={!!busy}>Abort move</Button></ConfirmAction>}
      {terminal(job.state) && <Button type="button" onClick={onPlanAnother}>Plan another move</Button>}
    </div>
    {can('final-sync') && lockEstimate && <div className="move-lock-estimate">
      <p>Expected lock during the final sync: <strong>about {moveDuration(lockEstimate.seconds)}</strong> <span className="muted-copy">(estimate from {lockEstimate.basis})</span></p>
      {lockEstimate.confirm && <label className="check-row"><input type="checkbox" checked={acceptLock} onChange={event => setAcceptLock(event.target.checked)} /><span><strong>Lock the application for about {moveDuration(lockEstimate.seconds)}</strong><small>The final sync takes longer than 15 minutes; the site is in maintenance meanwhile.</small></span></label>}
    </div>}
    {excluded.length > 0 && (can('excluded-tables') && excluded.some(db => db.excluded_tables!.some(table => !table.choice))
      ? <ExcludedTableChoices job={job} databases={excluded} busy={!!busy} onChoose={(database, tables) => act('excluded-tables', { database, tables })} />
      : <ExcludedTableSummary databases={excluded} />)}
    {job.state === 'cutover' && job.tls_ready && can('finish') && <div className="move-override">
      <label className="check-row"><input type="checkbox" checked={proxied} onChange={event => setProxied(event.target.checked)} /><span><strong>These hostnames are served through a proxy or CDN</strong><small>DNS then never resolves to the destination. HTTPS already works there; finish on your confirmation.</small></span></label>
      {proxied && <><Field label="Type the application id to confirm"><input aria-label="Application id" autoComplete="off" value={confirm} onChange={event => setConfirm(event.target.value)} /></Field>
        <Button type="button" variant="primary" disabled={!!busy || confirm !== job.app_id} onClick={() => { void act('finish', { override_dns: true, confirm }); }}>Finish without DNS match</Button></>}
    </div>}
  </div>;
}

/** MoveEstimateTable lists each engine's size and estimated durations. */
function MoveEstimateTable({ estimate }: { estimate: MoveEstimate }) {
  return <div className="move-estimate"><strong>Estimate</strong>
    <table className="move-estimate-table"><thead><tr><th>Data</th><th>Size</th><th>Bulk copy (app live)</th><th>Final sync (app locked)</th></tr></thead>
      <tbody>{estimate.engines.map(engine => <tr key={`${engine.kind}-${engine.object}`}><td>{engine.label}</td><td>{engine.kind === 'redis' ? 'not copied' : `${engine.measured ? '' : '≈ '}${size(Math.max(engine.bytes, 0))}${engine.objects > 0 ? `, ${engine.objects.toLocaleString()} ${engine.object_label}` : ''}`}</td><td>{engine.kind === 'redis' ? '—' : `≈ ${moveDuration(engine.bulk_seconds)}`}</td><td>{engine.kind === 'redis' ? '—' : `≈ ${moveDuration(engine.final_seconds)}${engine.final_basis === 'replication lag' ? ' (replication lag)' : ''}`}</td></tr>)}</tbody>
      <tfoot><tr><th scope="row">Total with deploy</th><td></td><td>≈ {moveDuration(estimate.bulk_seconds)}</td><td>≈ {moveDuration(estimate.final_seconds)}</td></tr></tfoot></table>
    <p className="muted-copy">Estimate based on {estimate.basis}. {estimate.notes?.join(' ')}</p>
  </div>;
}

/** ExcludedTableSummary shows the choices made in the plan for tables the backups skip. */
function ExcludedTableSummary({ databases }: { databases: NonNullable<MoveJob['databases']> }) {
  return <section className="move-excluded" aria-label="Tables the backups leave out">
    <h4>Tables the backups leave out</h4>
    <ul className="move-excluded-tables">{databases.flatMap(db => db.excluded_tables!.map(table => <li key={`${db.source_id}-${table.name}`}><code>{db.source_id}</code> {table.name} <span className="muted-copy">{tableEstimate(table)} · {table.choice === 'leave' ? 'left behind (schema only)' : 'data copied'}{table.copied_rows !== undefined ? ` · ${table.copied_rows.toLocaleString()} rows copied` : ''}</span></li>))}</ul>
  </section>;
}

/**
 * ExcludedTableChoices asks for the tables a move has no choice for: a job
 * created before plans offered the choice, or a backup setting changed
 * during the move. New moves record the choice when they start.
 */
function ExcludedTableChoices({ job, databases, busy, onChoose }: { job: MoveJob; databases: NonNullable<MoveJob['databases']>; busy: boolean; onChoose: (database: string, tables: Record<string, string>) => unknown }) {
  return <section className="move-excluded" aria-label="Tables the backups leave out">
    <h4>Tables the backups leave out</h4>
    <p className="move-caution">Choose for each table whether the move copies its data or leaves it behind (schema only), then continue the move.</p>
    {databases.map(db => <ul key={db.source_id} className="move-excluded-tables">{db.excluded_tables!.map(table => <li key={table.name}><code>{db.source_id}</code> {table.name} <span className="muted-copy">{tableEstimate(table)}{table.copied_rows !== undefined ? ` · ${table.copied_rows.toLocaleString()} rows copied` : ''}</span>
      <span className="move-excluded-choice">{(['copy', 'leave'] as const).map(choice => <Button key={choice} type="button" size="sm" variant={table.choice === choice ? 'primary' : undefined} disabled={busy || job.state === 'syncing'} aria-pressed={table.choice === choice} onClick={() => { void onChoose(db.source_id, { [table.name]: choice }); }}>{choice === 'copy' ? 'Copy data' : 'Leave behind'}</Button>)}</span></li>)}</ul>)}
  </section>;
}
