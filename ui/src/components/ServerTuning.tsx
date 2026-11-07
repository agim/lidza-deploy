import { useEffect, useState } from 'react';
import { ArrowRightLeft, ListChecks, Play, RefreshCw, Save, TriangleAlert } from 'lucide-react';
import { get, post, put } from '../lib/api';
import { formatBytes, formatDate } from '../lib/format';
import type { RedisUse, ResourceTuningView, TuningCap, TuningChange, TuningOverrides, TuningRedis, TuningSetting } from '../lib/types';
import { useToast } from './ToastProvider';
import { ResourceList, type Column } from './ResourceList';
import { Badge, Button, Card, ConfirmAction, ErrorBanner, Loading, Notice, SectionHeader, StatusBadge } from './ui';
import { Input } from './primitives/input';
import { Switch } from './primitives/switch';

const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);
const capText = (value: number) => value < 0 ? 'unlimited' : String(value);
const applyLabels: Record<TuningChange['apply'], string> = { reload: 'reload', live: 'live', restart: 'maintenance-window restart', 'next-restart': 'next container restart', deferred: 'waits' };
const kindLabels: Record<TuningRedis['kind'], string> = { shared: 'Shared host Redis', own: 'Own instance', container: 'Container' };

/** Copies overrides so edits never mutate the loaded view. */
function cloneOverrides(value?: TuningOverrides): TuningOverrides {
  return { paused: !!value?.paused, postgres: { ...value?.postgres }, connection_caps: { ...value?.connection_caps }, redis_max_memory_mib: { ...value?.redis_max_memory_mib }, redis_use: { ...value?.redis_use }, clickhouse_memory_ratio: value?.clickhouse_memory_ratio };
}

/** Drops empty override entries before saving. */
function compact(value: TuningOverrides): TuningOverrides {
  const keep = <T,>(entries?: Record<string, T>) => Object.fromEntries(Object.entries(entries || {}).filter(([, v]) => v !== '' && v !== undefined && !(typeof v === 'number' && Number.isNaN(v))));
  return { paused: value.paused || undefined, postgres: keep(value.postgres), connection_caps: keep(value.connection_caps), redis_max_memory_mib: keep(value.redis_max_memory_mib), redis_use: keep(value.redis_use), clickhouse_memory_ratio: value.clickhouse_memory_ratio || undefined };
}

function Changes({ changes }: { changes: TuningChange[] }) {
  if (!changes.length) return <p className="muted-copy">Nothing to change: every setting matches its target.</p>;
  return <ul className="host-disruptive" aria-label="Pending tuning changes">{changes.map(change => <li key={`${change.resource}/${change.setting}`}><code>{change.resource}</code> {change.setting}: {change.old} → {change.new} <Badge tone={change.apply === 'restart' || change.apply === 'deferred' ? 'warning' : 'neutral'}>{applyLabels[change.apply]}</Badge>{change.note && <small>{change.note}</small>}</li>)}</ul>;
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

  if (error && !view) return <Card className="form-card"><SectionHeader title="Resources" /><ErrorBanner>{error}</ErrorBanner></Card>;
  if (!view) return <Card className="form-card"><SectionHeader title="Resources" /><Loading label="Reading resource tuning…" /></Card>;
  if (!view.supported) return <Card className="form-card"><SectionHeader title="Resources" /><Notice>{view.reason || 'Resource tuning is not available on this server.'}</Notice></Card>;

  const host = view.host;
  const pgColumns: Column<TuningSetting>[] = [
    { id: 'name', label: 'Setting', render: row => <div><code>{row.name}</code>{row.restart && <Badge tone="neutral">restart</Badge>}{row.formula && <small className="muted-copy">{row.formula}</small>}</div> },
    { id: 'current', label: 'Running', render: row => <span>{row.current || '—'}{row.pending && <Badge tone="warning">restart pending</Badge>}</span> },
    { id: 'computed', label: 'Computed', render: row => row.computed },
    { id: 'override', label: 'Override', render: row => <Input aria-label={`Override ${row.name}`} value={draft.postgres?.[row.name] || ''} placeholder="computed" onChange={event => setPG(row.name, event.target.value)} /> },
    { id: 'target', label: 'Target', render: row => <strong>{row.target}</strong> },
  ];
  const capColumns: Column<TuningCap>[] = [
    { id: 'app', label: 'Application', render: row => <div><strong>{row.app_id}</strong><small className="muted-copy">role {row.role}</small>{row.warning && <small className="cap-warning"><TriangleAlert size={13} /> {row.warning}</small>}</div> },
    { id: 'use', label: 'In use / peak', render: row => `${row.in_use} / ${row.peak}` },
    { id: 'pool', label: 'App pool', render: row => row.pool ? <span>{row.pool}<small className="muted-copy">{row.pool_source}</small></span> : <span className="muted-copy">unknown</span> },
    { id: 'current', label: 'Cap now', render: row => capText(row.current) },
    { id: 'target', label: 'Target', render: row => <span>{row.superuser ? 'not enforced' : capText(row.target)}{row.deferred && <Badge tone="warning">waits</Badge>}</span> },
    { id: 'override', label: 'Override', render: row => row.superuser ? null : <Input aria-label={`Connection cap ${row.app_id}`} type="number" min={-1} value={Number.isFinite(draft.connection_caps?.[row.app_id]) ? String(draft.connection_caps?.[row.app_id]) : ''} placeholder={String(row.computed)} onChange={event => setCap(row.app_id, event.target.value)} /> },
  ];
  const moving = (view.migrations || []).filter(item => item.status === 'running').map(item => item.app_id);
  return <Card className="form-card" data-server-tuning={serverID}>
    <SectionHeader title="Resources" description={`Data services sized for ${host.cpus} CPUs, ${formatBytes(host.memory_mib * 1048576)} memory, ${host.disk_gib} GiB disk and ${view.apps} application${view.apps === 1 ? '' : 's'}. Recomputed at agent start (so after every reboot or resize) and hourly.${view.computed ? ` Computed ${formatDate(view.computed)}.` : ''}`} actions={<div className="maintenance-actions"><Button size="sm" onClick={refresh} pending={busy === 'refresh'} disabled={!!busy}><RefreshCw size={15} />Recompute</Button><Button size="sm" onClick={dryRun} pending={busy === 'review'} disabled={!!busy}><ListChecks size={15} />Review changes</Button></div>} />
    {error && <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner>}
    {view.paused && <Notice>Automatic tuning is paused: targets are computed but nothing is applied.</Notice>}
    {!!view.pending_restart?.length && <div className="alert alert-warning" role="status" aria-label="Pending restart"><TriangleAlert size={16} /><span><strong>{view.pending_restart.join(', ')}</strong> {view.restart_note || 'apply at the next PostgreSQL restart'}.{view.next_window ? ` Next maintenance window: ${formatDate(view.next_window)}.` : ''}</span></div>}
    {!!view.warnings?.length && <div className="alert alert-warning" role="status" aria-label="Tuning warnings"><TriangleAlert size={16} /><div>{view.warnings.map(item => <p key={item}>{item}</p>)}</div></div>}
    {review && <div className="host-disruptive-block"><strong>Dry run</strong><Changes changes={review} /><div className="maintenance-actions"><ConfirmAction title="Apply resource tuning now?" description="Reload settings, connection caps, Redis and ClickHouse limits apply at once. Settings that need a PostgreSQL restart are written and wait for the maintenance window. Nothing is lowered below what is in use." confirmLabel="Apply tuning" onConfirm={apply}><Button variant="primary" disabled={!!busy || !review.length}><Play size={16} />Apply now</Button></ConfirmAction><Button onClick={() => setReview(undefined)}>Close</Button></div></div>}

    {view.postgres && <><SectionHeader title={`PostgreSQL ${view.postgres.cluster} · port ${view.postgres.port}`} description={`${view.postgres.in_use} sessions open. Applications share ${view.postgres.app_pool} connections; ${view.postgres.headroom} stay reserved for platform operations.${view.postgres.conf_file ? ` Written to ${view.postgres.conf_file}.` : ''}`} />
      {view.postgres.note && <Notice>{view.postgres.note}</Notice>}
      <ResourceList rows={view.postgres.settings} columns={pgColumns} rowKey={row => row.name} />
      <SectionHeader title="Connection caps" description="Each application's share of the connections, from its observed 7-day peak and configured pool. A cap is never lowered below the sessions in use." />
      <ResourceList rows={view.caps || []} columns={capColumns} rowKey={row => row.role} empty={<p className="muted-copy">No application uses the host cluster.</p>} /></>}

    {!!view.redis?.length && <><SectionHeader title="Redis" description="Caches evict least-recently-used keys; queues and persistent data never evict and alert above 80% of their limit." />
      {view.redis.map(instance => <div className="tuning-redis" key={instance.id} data-redis={instance.id}>
        <div className="host-job-heading"><strong>{kindLabels[instance.kind]}</strong><code>{instance.id}</code>{instance.alert && <Badge tone="danger">above 80%</Badge>}</div>
        <p className="muted-copy">{formatBytes(instance.used_bytes)} used · limit {instance.max_bytes ? formatBytes(instance.max_bytes) : 'none'} → {instance.target_mib} MiB · {instance.policy || 'unknown'} → {instance.target_policy}{instance.error ? ` · ${instance.error}` : ''}</p>
        {instance.note && <p className="muted-copy">{instance.note}</p>}
        <label className="field-inline"><span>Limit override (MiB)</span><Input aria-label={`Redis limit ${instance.id}`} type="number" min={16} value={Number.isFinite(draft.redis_max_memory_mib?.[instance.id]) ? String(draft.redis_max_memory_mib?.[instance.id]) : ''} placeholder={String(instance.target_mib)} onChange={event => setRedisLimit(instance.id, event.target.value)} /></label>
        {!!instance.apps.length && <ul className="host-jobs">{instance.apps.map(app => <li key={app.app_id}><div className="host-job-heading"><strong>{app.app_id}</strong>{app.dbs?.length ? <small>db {app.dbs.join(', ')}</small> : null}<small>{app.use} ({app.use_source})</small></div>
          <label className="field-inline"><span>Use</span><select aria-label={`Redis use ${app.app_id}`} value={draft.redis_use?.[app.app_id] || ''} onChange={event => setUse(app.app_id, event.target.value)}><option value="">Detected ({app.use})</option><option value="cache">Cache</option><option value="queue">Queue</option><option value="persistent">Persistent data</option></select></label>
          {instance.kind === 'shared' && (app.can_move ? <ConfirmAction title={`Move ${app.app_id} to its own Redis?`} description={app.use === 'cache' ? 'A new private Redis instance starts empty and the application restarts on it; its cache refills. The old database is cleared only after the application runs healthily on the new instance.' : 'The application stops while its keys are copied to a new private Redis instance, then starts on it. The old database is cleared only after the application runs healthily and no client is left on it; a failure restores the old setting.'} confirmLabel="Move to its own Redis" onConfirm={() => migrate(app.app_id)}><Button size="sm" disabled={!!busy || moving.includes(app.app_id)}><ArrowRightLeft size={15} />Move to its own Redis</Button></ConfirmAction> : <small className="muted-copy">Cannot move: {app.move_blocked}</small>)}
        </li>)}</ul>}
      </div>)}</>}

    {!!view.clickhouse?.length && <><SectionHeader title="ClickHouse" description="Memory is shared by the ClickHouse users on this server." />
      <ul className="host-jobs">{view.clickhouse.map(server => <li key={server.id}><div className="host-job-heading"><strong>{server.kind === 'native' ? 'Host ClickHouse' : server.id}</strong><small>{server.users} user{server.users === 1 ? '' : 's'}</small></div><p className="muted-copy">Memory ratio {server.current_ratio ? server.current_ratio.toFixed(3) : 'default'} → {server.target_ratio.toFixed(3)} ({server.server_mib} MiB) · per query {server.query_memory_mib} MiB</p>
        {server.kind === 'native' && <label className="field-inline"><span>Ratio override</span><Input aria-label="ClickHouse memory ratio" type="number" step="0.01" min={0.05} max={0.9} value={draft.clickhouse_memory_ratio ?? ''} placeholder={server.target_ratio.toFixed(3)} onChange={event => setDraft(current => ({ ...current, clickhouse_memory_ratio: event.target.value === '' ? undefined : Number(event.target.value) }))} /></label>}</li>)}</ul></>}

    {!!view.containers?.length && <details className="host-packages"><summary>Container databases ({view.containers.length})</summary>{view.containers.map(db => <div key={db.app_id}><strong>{db.app_id}</strong>{db.note && <small className="muted-copy"> {db.note}</small>}<ul>{db.settings.map(s => <li key={s.name}><code>{s.name}</code> {s.current || '—'} → {s.target}{s.restart && <Badge tone="neutral">next restart</Badge>}</li>)}</ul></div>)}</details>}

    <label className="check-row switch-row"><Switch aria-label="Pause automatic tuning" checked={!!draft.paused} onCheckedChange={paused => setDraft(current => ({ ...current, paused }))} /><span><strong>Pause automatic tuning</strong><small>Targets are still computed and shown; nothing is applied until tuning resumes.</small></span></label>
    <div className="maintenance-actions"><Button variant="primary" onClick={save} pending={busy === 'save'} disabled={!!busy}><Save size={16} />Save overrides</Button></div>

    {!!view.migrations?.length && <><SectionHeader title="Redis moves" /><ul className="host-jobs">{view.migrations.map(item => <li key={item.app_id}><div className="host-job-heading"><StatusBadge status={item.status === 'succeeded' ? 'done' : item.status} /><strong>{item.app_id}</strong><small>{formatDate(item.started)}{item.step && item.status === 'running' ? ` · ${item.step}` : ''}</small></div>{item.error && <p className="muted-copy">{item.error}</p>}</li>)}</ul></>}
    <SectionHeader title="Tuning history" />
    {view.jobs?.length ? <ul className="host-jobs">{view.jobs.slice(0, 6).map(job => <li key={job.id}><div className="host-job-heading"><StatusBadge status={job.status === 'succeeded' ? 'done' : job.status} /><strong>Resource tuning</strong><small>{formatDate(job.started)} · {job.trigger}</small></div>{job.summary && <p className="muted-copy">{job.summary}</p>}{job.output && <details><summary>Changes</summary><pre className="maintenance-report">{job.output}</pre></details>}</li>)}</ul> : <p className="muted-copy">No tuning has been applied yet.</p>}
  </Card>;
}
