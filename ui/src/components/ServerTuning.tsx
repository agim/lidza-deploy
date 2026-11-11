import { useEffect, useState } from 'react';
import { ArrowRightLeft, ListChecks, Play, RefreshCw, Save, TriangleAlert } from 'lucide-react';
import { CheckList, type CheckListItem, type CheckStatus } from './CheckList';
import { SummaryList } from './SummaryList';
import { Cluster, Stack } from './layout';
import { get, post, put } from '../lib/api';
import { formatBytes, formatDate } from '../lib/format';
import type { HostJob, JournalPolicyState, RedisUse, ResourceTuningView, TuningCap, TuningChange, TuningOverrides, TuningRedis, TuningRedisApp, TuningSetting } from '../lib/types';
import { useToast } from './ToastProvider';
import { ResourceList, type Column } from './ResourceList';
import { Badge, Button, Card, ConfirmAction, ErrorBanner, Field, Loading, Notice, SectionHeader, StatusBadge } from './ui';
import { Input } from './primitives/input';
import { Switch } from './primitives/switch';

const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);
const capText = (value: number) => value < 0 ? 'unlimited' : String(value);
const applyLabels: Record<TuningChange['apply'], string> = { reload: 'reload', live: 'live', restart: 'maintenance-window restart', 'next-restart': 'next container restart', deferred: 'waits' };
const kindLabels: Record<TuningRedis['kind'], string> = { shared: 'Shared host Redis', own: 'Own instance', container: 'Container' };

/** Copies overrides so edits never mutate the loaded view. */
function cloneOverrides(value?: TuningOverrides): TuningOverrides {
  return { paused: !!value?.paused, postgres: { ...value?.postgres }, connection_caps: { ...value?.connection_caps }, redis_max_memory_mib: { ...value?.redis_max_memory_mib }, redis_use: { ...value?.redis_use }, clickhouse_memory_ratio: value?.clickhouse_memory_ratio, journal_max_use_mib: value?.journal_max_use_mib };
}

/** Drops empty override entries before saving. */
function compact(value: TuningOverrides): TuningOverrides {
  const keep = <T,>(entries?: Record<string, T>) => Object.fromEntries(Object.entries(entries || {}).filter(([, v]) => v !== '' && v !== undefined && !(typeof v === 'number' && Number.isNaN(v))));
  return { paused: value.paused || undefined, postgres: keep(value.postgres), connection_caps: keep(value.connection_caps), redis_max_memory_mib: keep(value.redis_max_memory_mib), redis_use: keep(value.redis_use), clickhouse_memory_ratio: value.clickhouse_memory_ratio || undefined, journal_max_use_mib: value.journal_max_use_mib || undefined };
}

/** Applying a change now (live, reload) or later (restart, deferred, next restart). */
const later = (apply: string) => apply === 'restart' || apply === 'deferred' || apply === 'next-restart';
const changeStatus = (apply: string): CheckStatus => later(apply) ? 'waiting' : 'pending';

/**
 * tuningOutputItems turns a tuning run's log (one change per line, as the
 * agent writes it: "resource setting: old → new (apply): note", "FAILED …")
 * into check list rows: failures failed, changes that wait (deferred, next
 * restart, maintenance-window restart) waiting, everything else done.
 */
export function tuningOutputItems(output = ''): CheckListItem[] {
  return output.split('\n').map(line => line.trim()).filter(Boolean).map((line, index) => {
    const id = `${index}`;
    if (/^FAILED\b/.test(line)) return { id, status: 'failed', children: line.replace(/^FAILED:?\s*/, '') };
    const change = line.match(/\((live|reload|restart|deferred|next-restart)\)(?::\s*(.*))?$/);
    if (change) {
      const apply = change[1] as TuningChange['apply'];
      return { id, status: later(apply) ? 'waiting' : 'ok', children: line.slice(0, change.index).trim(), detail: change[2] ? (apply === 'deferred' ? change[2] : `${applyLabels[apply]}: ${change[2]}`) : applyLabels[apply] };
    }
    return { id, status: /\bwaits?\b/.test(line) ? 'waiting' : 'ok', children: line };
  });
}

function Changes({ changes }: { changes: TuningChange[] }) {
  if (!changes.length) return <p className="muted-copy">Nothing to change: every setting matches its target.</p>;
  return <CheckList aria-label="Pending tuning changes" items={changes.map(change => ({ id: `${change.resource}/${change.setting}`, status: changeStatus(change.apply), children: <span><code>{change.resource}</code> {change.setting}: {change.old} → {change.new}</span>, detail: `${applyLabels[change.apply]}${change.note ? ` · ${change.note}` : ''}` }))} />;
}

const jobStatus = (job: HostJob) => job.status === 'succeeded' ? 'done' : job.status;

/** One tuning run: status, when and why it ran, its summary and its changes. */
function TuningJob({ job }: { job: HostJob }) {
  const items = tuningOutputItems(job.output);
  return <li><Stack gap="sm">
    <div className="host-job-heading"><StatusBadge status={jobStatus(job)} /><strong>Resource tuning</strong><small>{formatDate(job.started)} · {job.trigger}</small></div>
    {job.summary && <p className="muted-copy">{job.summary}</p>}
    {items.length > 0 && <details className="tuning-job-changes"><summary>Changes ({items.length})</summary><CheckList items={items} /></details>}
  </Stack></li>;
}

const mib = (bytes: number) => Math.round(bytes / 1048576);

/** The systemd journal size: computed default, override, what the files hold now. */
function SystemLog({ journal, value, onChange }: { journal: JournalPolicyState; value?: number; onChange: (value?: number) => void }) {
  const target = journal.target;
  return <Stack gap="sm" aria-label="System log">
    <SectionHeader title="System log" description={`The systemd journal is kept to ${journal.override_mib ? 'the override' : '1% of the disk, 1–4 GiB'}, with ${formatBytes(target.keep_free_bytes)} of disk always left free and entries older than 30 days removed. A smaller journal keeps every log read fast. Written to ${journal.drop_in}, recomputed at agent start and daily.`} />
    {journal.error && <div className="alert alert-warning" role="status" aria-label="System log warning"><TriangleAlert size={16} /><span>{journal.error}{journal.restart_pending ? ' The journald restart is retried at the next run.' : ''}</span></div>}
    <SummaryList aria-label="System log policy" items={[
      { label: 'In use now', value: journal.usage_bytes ? formatBytes(journal.usage_bytes) : 'unknown' },
      { label: 'Maximum size', value: `${formatBytes(target.max_use_bytes)}${journal.override_mib ? ` (override; computed ${formatBytes(journal.computed.max_use_bytes)})` : ''}` },
      { label: 'Journal file size', value: formatBytes(target.max_file_bytes) },
      !!journal.disk_bytes && { label: 'Disk', value: `${formatBytes(journal.disk_bytes)}${journal.disk_path ? ` (${journal.disk_path})` : ''}` },
      !!journal.last_applied && { label: 'Last applied', value: formatDate(journal.last_applied) },
      ...(journal.neutralized || []).map(item => ({ id: item.file, label: 'Disabled in operator file', value: <span><code>{item.file}</code>: {item.lines.join(', ')} · original kept at <code>{item.backup}</code></span> })),
    ]} />
    <Field label="System log size (MiB)" hint={`Empty uses the computed ${mib(journal.computed.max_use_bytes)} MiB. 256 MiB to 64 GiB; applied within a minute after saving.`}>
      <Input aria-label="System log size" type="number" min={256} max={65536} value={value ?? ''} placeholder={String(mib(journal.computed.max_use_bytes))} onChange={event => onChange(event.target.value === '' ? undefined : Number(event.target.value))} />
    </Field>
  </Stack>;
}

export function ServerTuning({ serverID }: { serverID: string }) {
  const notify = useToast();
  const base = `control/servers/${encodeURIComponent(serverID)}/host/resources`;
  const [view, setView] = useState<ResourceTuningView>();
  const [draft, setDraft] = useState<TuningOverrides>(cloneOverrides());
  const [review, setReview] = useState<TuningChange[]>();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState('');

  const adopt = (value: ResourceTuningView, keepDraft = false) => { setView(value); if (!keepDraft) setDraft(cloneOverrides(value.overrides)); };
  useEffect(() => {
    let active = true; let timer: ReturnType<typeof setTimeout>;
    setView(undefined); setReview(undefined); setError('');
    const load = async (initial: boolean) => {
      try { const value = await get<ResourceTuningView>(base); if (!active) return; adopt(value, !initial); setError(''); timer = setTimeout(() => { void load(false); }, value.running ? 3000 : 30000); }
      catch (reason) { if (active) { setError(message(reason)); timer = setTimeout(() => { void load(initial); }, 30000); } }
    };
    void load(true);
    return () => { active = false; clearTimeout(timer); };
  }, [serverID]);

  const run = async (label: string, action: () => Promise<void>) => { setBusy(label); try { await action(); setError(''); } catch (reason) { setError(message(reason)); } finally { setBusy(''); } };
  const refresh = () => run('refresh', async () => adopt(await get<ResourceTuningView>(`${base}?refresh=1`)));
  const dryRun = () => run('review', async () => { const value = await post<ResourceTuningView>(`${base}/apply`, { dry_run: true }); adopt(value, true); setReview(value.changes || []); });
  const apply = () => run('apply', async () => { adopt(await post<ResourceTuningView>(`${base}/apply`, {})); setReview(undefined); notify('Tuning is being applied; the results appear in the history below.'); });
  const save = () => run('save', async () => { const value = await put<ResourceTuningView>(`${base}/overrides`, compact(draft)); adopt(value); setReview(value.changes || []); notify('Overrides saved. Review the changes, then apply.'); });
  const migrate = (app: string) => run('migrate', async () => { adopt(await post<ResourceTuningView>(`${base}/redis/${encodeURIComponent(app)}/migrate`, { confirm: app }), true); notify(`Moving ${app} to its own Redis.`); });

  const setPG = (name: string, value: string) => setDraft(current => ({ ...current, postgres: { ...current.postgres, [name]: value.trim() } }));
  const setCap = (app: string, value: string) => setDraft(current => ({ ...current, connection_caps: { ...current.connection_caps, [app]: value.trim() === '' ? Number.NaN : Number(value) } }));
  const setUse = (app: string, value: string) => setDraft(current => { const next = { ...current.redis_use }; if (value) next[app] = value as RedisUse; else delete next[app]; return { ...current, redis_use: next }; });
  const setRedisLimit = (id: string, value: string) => setDraft(current => ({ ...current, redis_max_memory_mib: { ...current.redis_max_memory_mib, [id]: value.trim() === '' ? Number.NaN : Number(value) } }));

  if (error && !view) return <Card className="form-card"><SectionHeader title="Resource tuning" /><ErrorBanner>{error}</ErrorBanner></Card>;
  if (!view) return <Card className="form-card"><SectionHeader title="Resource tuning" /><Loading label="Reading resource tuning…" /></Card>;
  if (!view.supported) return <Card className="form-card"><SectionHeader title="Resource tuning" /><Notice>{view.reason || 'Resource tuning is not available on this server.'}</Notice></Card>;

  const host = view.host;
  const last = view.jobs?.[0];
  const pgColumns: Column<TuningSetting>[] = [
    { id: 'name', label: 'Setting', render: row => <Stack gap="sm"><Cluster><code>{row.name}</code>{row.restart && <Badge tone="neutral">restart</Badge>}</Cluster>{row.formula && <small className="muted-copy">{row.formula}</small>}</Stack> },
    { id: 'current', label: 'Running', render: row => <Cluster><span>{row.current || '—'}</span>{row.pending && <Badge tone="warning">restart pending</Badge>}</Cluster> },
    { id: 'computed', label: 'Computed', render: row => row.computed },
    { id: 'override', label: 'Override', render: row => <Input aria-label={`Override ${row.name}`} value={draft.postgres?.[row.name] || ''} placeholder="computed" onChange={event => setPG(row.name, event.target.value)} /> },
    { id: 'target', label: 'Target', render: row => <strong>{row.target}</strong> },
  ];
  const capColumns: Column<TuningCap>[] = [
    { id: 'app', label: 'Application', render: row => <Stack gap="sm"><strong>{row.app_id}</strong><small className="muted-copy">role {row.role}</small>{row.warning && <small className="cap-warning"><TriangleAlert size={13} />{row.warning}</small>}</Stack> },
    { id: 'use', label: 'In use / peak', render: row => `${row.in_use} / ${row.peak}` },
    { id: 'pool', label: 'App pool', render: row => row.pool ? <Stack gap="sm"><span>{row.pool}</span><small className="muted-copy">{row.pool_source}</small></Stack> : <span className="muted-copy">unknown</span> },
    { id: 'current', label: 'Cap now', render: row => capText(row.current) },
    { id: 'target', label: 'Target', render: row => <Cluster><span>{row.superuser ? 'not enforced' : capText(row.target)}</span>{row.deferred && <Badge tone="warning">waits</Badge>}</Cluster> },
    { id: 'override', label: 'Override', render: row => row.superuser ? null : <Input aria-label={`Connection cap ${row.app_id}`} type="number" min={-1} value={Number.isFinite(draft.connection_caps?.[row.app_id]) ? String(draft.connection_caps?.[row.app_id]) : ''} placeholder={String(row.computed)} onChange={event => setCap(row.app_id, event.target.value)} /> },
  ];
  const moving = (view.migrations || []).filter(item => item.status === 'running').map(item => item.app_id);
  const redisAppColumns = (instance: TuningRedis): Column<TuningRedisApp>[] => [
    { id: 'app', label: 'Application', render: app => <Stack gap="sm"><strong>{app.app_id}</strong>{app.dbs?.length ? <small className="muted-copy">db {app.dbs.join(', ')}</small> : null}</Stack> },
    { id: 'detected', label: 'Detected use', render: app => `${app.use} (${app.use_source})` },
    { id: 'use', label: 'Use', render: app => <select aria-label={`Redis use ${app.app_id}`} value={draft.redis_use?.[app.app_id] || ''} onChange={event => setUse(app.app_id, event.target.value)}><option value="">Detected ({app.use})</option><option value="cache">Cache</option><option value="queue">Queue</option><option value="persistent">Persistent data</option></select> },
    ...(instance.kind === 'shared' ? [{ id: 'move', label: 'Own instance', render: (app: TuningRedisApp) => app.can_move ? <ConfirmAction title={`Move ${app.app_id} to its own Redis?`} description={app.use === 'cache' ? 'A new private Redis instance starts empty and the application restarts on it; its cache refills. The old database is cleared only after the application runs healthily on the new instance.' : 'The application stops while its keys are copied to a new private Redis instance, then starts on it. The old database is cleared only after the application runs healthily and no client is left on it; a failure restores the old setting.'} confirmLabel="Move to its own Redis" onConfirm={() => migrate(app.app_id)}><Button size="sm" disabled={!!busy || moving.includes(app.app_id)}><ArrowRightLeft size={15} />Move to its own Redis</Button></ConfirmAction> : <small className="muted-copy">Cannot move: {app.move_blocked}</small> }] : []),
  ];
  // Things that need the operator's attention: restarts that wait, warnings, a pause.
  const attention: CheckListItem[] = [
    ...(view.paused ? [{ id: 'paused', status: 'info' as const, children: 'Automatic tuning is paused: targets are computed but nothing is applied.' }] : []),
    ...(view.pending_restart?.length ? [{ id: 'restart', status: 'waiting' as const, children: <span><strong>{view.pending_restart.join(', ')}</strong> {view.restart_note || 'apply at the next PostgreSQL restart'}.</span>, detail: view.next_window ? `Next maintenance window: ${formatDate(view.next_window)}.` : undefined }] : []),
    ...(view.warnings || []).map(item => ({ id: `warning-${item}`, status: 'warning' as const, children: item })),
  ];

  return <Stack gap="page" data-server-tuning={serverID}>
    <Card className="form-card" aria-label="Tuning status">
      <SectionHeader title="Resource tuning" description="Data services are sized for this server. Targets are recomputed at agent start (so after every reboot or resize) and hourly." actions={<><Button size="sm" onClick={refresh} pending={busy === 'refresh'} disabled={!!busy}><RefreshCw size={15} />Recompute</Button><Button size="sm" onClick={dryRun} pending={busy === 'review'} disabled={!!busy}><ListChecks size={15} />Review changes</Button></>} />
      {error && <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner>}
      <SummaryList aria-label="Tuning summary" items={[
        { label: 'Status', value: <Cluster>{view.running ? <StatusBadge status="running" /> : last ? <StatusBadge status={jobStatus(last)} /> : <Badge>Not run yet</Badge>}{view.paused && <Badge tone="warning">Paused</Badge>}</Cluster> },
        last && { label: 'Last run', value: `${formatDate(last.started)} · ${last.trigger}${last.summary ? ` · ${last.summary}` : ''}` },
        { label: 'Sized for', value: `${host.cpus} CPUs · ${formatBytes(host.memory_mib * 1048576)} memory · ${host.disk_gib} GiB disk · ${view.apps} application${view.apps === 1 ? '' : 's'}` },
        view.computed && { label: 'Computed', value: formatDate(view.computed) },
        view.next_window && { label: 'Maintenance window', value: formatDate(view.next_window) },
      ]} />
      {attention.length > 0 && <CheckList aria-label="Tuning notes" items={attention} />}
      {last?.output && <Stack gap="sm"><strong className="tuning-subheading">Last run changes</strong><CheckList aria-label="Last run changes" items={tuningOutputItems(last.output)} /></Stack>}
      {review && <div className="host-disruptive-block" aria-label="Dry run"><Stack gap="md"><strong>Dry run</strong><Changes changes={review} /><Cluster><ConfirmAction title="Apply resource tuning now?" description="Reload settings, connection caps, Redis and ClickHouse limits apply at once. Settings that need a PostgreSQL restart are written and wait for the maintenance window. Nothing is lowered below what is in use." confirmLabel="Apply tuning" onConfirm={apply}><Button variant="primary" disabled={!!busy || !review.length}><Play size={16} />Apply now</Button></ConfirmAction><Button onClick={() => setReview(undefined)}>Close</Button></Cluster></Stack></div>}
    </Card>

    {view.postgres && <Card className="form-card" aria-label="PostgreSQL settings">
      <SectionHeader title={`PostgreSQL ${view.postgres.cluster} · port ${view.postgres.port}`} description={`${view.postgres.in_use} sessions open. Applications share ${view.postgres.app_pool} connections; ${view.postgres.headroom} stay reserved for platform operations.`} />
      {view.postgres.conf_file && <SummaryList items={[{ label: 'Written to', value: <code>{view.postgres.conf_file}</code> }]} />}
      {view.postgres.note && <Notice>{view.postgres.note}</Notice>}
      <ResourceList rows={view.postgres.settings} columns={pgColumns} rowKey={row => row.name} />
    </Card>}
    {view.postgres && <Card className="form-card" aria-label="Connection caps">
      <SectionHeader title="Connection caps" description="Each application's share of the connections, from its observed 7-day peak and configured pool. A cap is never lowered below the sessions in use." />
      <ResourceList rows={view.caps || []} columns={capColumns} rowKey={row => row.role} empty={<p className="muted-copy">No application uses the host cluster.</p>} />
    </Card>}

    {!!view.redis?.length && <Card className="form-card" aria-label="Redis">
      <SectionHeader title="Redis" description="Caches evict least-recently-used keys; queues and persistent data never evict and alert above 80% of their limit." />
      {view.redis.map(instance => <Stack gap="md" className="tuning-redis" key={instance.id} data-redis={instance.id}>
        <Cluster><strong>{kindLabels[instance.kind]}</strong><code>{instance.id}</code>{instance.alert && <Badge tone="danger">above 80%</Badge>}</Cluster>
        <SummaryList items={[
          { label: 'Memory', value: `${formatBytes(instance.used_bytes)} used · limit ${instance.max_bytes ? formatBytes(instance.max_bytes) : 'none'} → ${instance.target_mib} MiB` },
          { label: 'Eviction', value: `${instance.policy || 'unknown'} → ${instance.target_policy}` },
          instance.error && { label: 'Error', value: instance.error },
        ]} />
        {instance.note && <p className="muted-copy">{instance.note}</p>}
        <Field label="Limit override (MiB)" className="tuning-field"><Input aria-label={`Redis limit ${instance.id}`} type="number" min={16} value={Number.isFinite(draft.redis_max_memory_mib?.[instance.id]) ? String(draft.redis_max_memory_mib?.[instance.id]) : ''} placeholder={String(instance.target_mib)} onChange={event => setRedisLimit(instance.id, event.target.value)} /></Field>
        <ResourceList rows={instance.apps || []} columns={redisAppColumns(instance)} rowKey={app => app.app_id} empty={<p className="muted-copy">No application uses this instance.</p>} />
      </Stack>)}
    </Card>}

    {!!view.clickhouse?.length && <Card className="form-card" aria-label="ClickHouse">
      <SectionHeader title="ClickHouse" description="Memory is shared by the ClickHouse users on this server." />
      {view.clickhouse.map(server => <Stack gap="md" className="tuning-redis" key={server.id}>
        <Cluster><strong>{server.kind === 'native' ? 'Host ClickHouse' : server.id}</strong><Badge>{server.users} user{server.users === 1 ? '' : 's'}</Badge></Cluster>
        <SummaryList items={[
          { label: 'Memory ratio', value: `${server.current_ratio ? server.current_ratio.toFixed(3) : 'default'} → ${server.target_ratio.toFixed(3)} (${server.server_mib} MiB)` },
          { label: 'Per query', value: `${server.query_memory_mib} MiB` },
        ]} />
        {server.kind === 'native' && <Field label="Ratio override" className="tuning-field"><Input aria-label="ClickHouse memory ratio" type="number" step="0.01" min={0.05} max={0.9} value={draft.clickhouse_memory_ratio ?? ''} placeholder={server.target_ratio.toFixed(3)} onChange={event => setDraft(current => ({ ...current, clickhouse_memory_ratio: event.target.value === '' ? undefined : Number(event.target.value) }))} /></Field>}
      </Stack>)}
    </Card>}

    {!!view.containers?.length && <Card className="form-card" aria-label="Container databases">
      <SectionHeader title={`Container databases (${view.containers.length})`} description="Settings of databases in application containers apply at the container's next restart; containers are not restarted for tuning." />
      {view.containers.map(db => <Stack gap="md" className="tuning-redis" key={db.app_id}>
        <Cluster><strong>{db.app_id}</strong><code>{db.container}</code></Cluster>
        {db.note && <p className="muted-copy">{db.note}</p>}
        <CheckList items={db.settings.map(setting => ({ id: setting.name, status: setting.current === setting.target ? 'ok' : setting.restart ? 'waiting' : 'pending', children: <span><code>{setting.name}</code> {setting.current || '—'} → {setting.target}</span>, detail: setting.current !== setting.target && setting.restart ? 'next container restart' : undefined }))} />
      </Stack>)}
    </Card>}

    {view.journal && <Card className="form-card" aria-label="System log"><SystemLog journal={view.journal} value={draft.journal_max_use_mib} onChange={value => setDraft(current => ({ ...current, journal_max_use_mib: value }))} /></Card>}

    <Card className="form-card" aria-label="Tuning overrides">
      <SectionHeader title="Overrides" description="Values entered above replace the computed targets once saved. Review the changes, then apply them." />
      <label className="check-row switch-row"><Switch aria-label="Pause automatic tuning" checked={!!draft.paused} onCheckedChange={paused => setDraft(current => ({ ...current, paused }))} /><span><strong>Pause automatic tuning</strong><small>Targets are still computed and shown; nothing is applied until tuning resumes.</small></span></label>
      <Cluster><Button variant="primary" onClick={save} pending={busy === 'save'} disabled={!!busy}><Save size={16} />Save overrides</Button></Cluster>
    </Card>

    {!!view.migrations?.length && <Card className="form-card" aria-label="Redis moves"><SectionHeader title="Redis moves" /><ul className="host-jobs">{view.migrations.map(item => <li key={item.app_id}><Stack gap="sm"><div className="host-job-heading"><StatusBadge status={item.status === 'succeeded' ? 'done' : item.status} /><strong>{item.app_id}</strong><small>{formatDate(item.started)}{item.step && item.status === 'running' ? ` · ${item.step}` : ''}</small></div>{item.error && <p className="muted-copy">{item.error}</p>}</Stack></li>)}</ul></Card>}
    <Card className="form-card" aria-label="Tuning history"><SectionHeader title="Tuning history" />
      {view.jobs?.length ? <ul className="host-jobs">{view.jobs.slice(0, 6).map(job => <TuningJob key={job.id} job={job} />)}</ul> : <p className="muted-copy">No tuning has been applied yet.</p>}
    </Card>
  </Stack>;
}
