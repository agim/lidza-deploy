import { usePreference } from '../state/PreferencesContext';
import { resourceHref, withSticky } from '../lib/router';
import { AlertTriangle, Cloud, CloudOff, Database, Download, HardDrive, RefreshCw, RotateCcw, Search } from 'lucide-react';
import { type FormEvent, useEffect, useMemo, useRef, useState } from 'react';
import { get, patch, post } from '../lib/api';
import { formatBytes, formatDate, relativeTime } from '../lib/format';
import { useWorkspace } from '../state/WorkspaceContext';
import { useToast } from '../components/ToastProvider';
import { MoveLockCard, MoveLockFieldset, useAppMoveLock } from '../components/MoveLock';
import { Badge, Button, Card, ConfirmAction, EmptyState, ErrorBanner, Field, Loading, Notice, PromptDialog, SearchField, SectionHeader, StatusBadge, Toolbar } from '../components/ui';
import { ResourceDetailPage, ResourceIndexPage, ResourceName, visibleTabs } from '../components/resource';
import { SummaryList } from '../components/SummaryList';
import { RetainedResources } from '../components/RetainedResources';
import { ResourceList, type Column } from '../components/ResourceList';
import { LiveProgress, type OperationProgress } from '../components/LiveProgress';
import { BackupRetentionCard, DatabaseRetentionCard, type RetentionPolicy } from '../components/BackupRetention';
import { BeforeDeployControl, type BeforeDeployPolicy, beforeDeployLabel } from '../components/BeforeDeployBackup';
import { BackupExcludedTablesCard, skippedTablesLabel } from '../components/BackupExcludedTables';

interface Backup { id: string; created: string; size?: number; kind?: string; offsite?: boolean; offsite_requested?: boolean; upload_error?: string; object_key?: string; sha256?: string; excluded_table_data?: string[] }
interface BackupPolicy { hours?: number; cron?: string; timezone?: string; keep?: number; offsite?: boolean; retention?: RetentionPolicy | null }
interface DatabaseItem { app_id: string; server_id: string; mode?: string; engine?: string; size_bytes?: number; ready?: boolean; retained?: boolean; unavailable?: boolean; error?: string; operation?: string; progress?: OperationProgress; backup?: BackupPolicy; backups?: Backup[]; attachments?: Array<{ app_id: string; env_key?: string }>; before_deploy?: BeforeDeployPolicy; primary?: boolean; excluded_tables?: string[]; excluded_tables_supported?: boolean; warnings?: string[] }
function databaseLabel(database: DatabaseItem) {
  if (database.engine === 'sqlite') return 'Native SQLite';
  if (database.engine === 'clickhouse') return 'ClickHouse';
  if (database.engine) return `${database.engine} (plugin service)`;
  return database.mode === 'external' ? 'External PostgreSQL' : 'Private PostgreSQL';
}

const operationLabels: Record<string, string> = { backup: 'Backing up', restore: 'Restoring', upload: 'Uploading the off-site copy', provision: 'Provisioning' };

/**
 * DatabaseOperation shows a database's running backup, restore or upload.
 * The agent reports when each phase began, not the operation, so the
 * elapsed time counts from when this page first saw the operation.
 */
function DatabaseOperation({ database }: { database: DatabaseItem }) {
  if (!database.operation) return null;
  return <LiveProgress className="database-progress" label={operationLabels[database.operation] || database.operation} progress={database.progress} quietHint="database tools print nothing until they finish" />;
}

/** DatabaseWarnings shows what stops this database's backups, such as a backup tool that changed and was not re-approved. */
function DatabaseWarnings({ database }: { database: DatabaseItem }) {
  if (!database.warnings?.length) return null;
  return <div className="notice-stack">{database.warnings.map(warning => <div key={warning} className="alert notice-row alert-warning" role="status" aria-label="Database warning"><AlertTriangle size={17} aria-hidden="true" /><span>{warning}</span></div>)}</div>;
}

/** usePollWhile reloads every few seconds while busy is true. */
function usePollWhile(busy: boolean, load: () => unknown, ms = 3000) {
  const latest = useRef(load);
  latest.current = load;
  useEffect(() => {
    if (!busy) return;
    const timer = setInterval(() => { void latest.current(); }, ms);
    return () => clearInterval(timer);
  }, [busy, ms]);
}

interface RemoteBackup { key: string; prefix?: string; size?: number; modifiedAt?: string; legacy?: boolean }

function backupScheduleLabel(policy?: BackupPolicy) {
  if (policy?.cron) return `${policy.cron} (${policy.timezone || 'UTC'})`;
  if (policy?.hours === 1) return 'Hourly';
  if (policy?.hours === 24) return 'Daily';
  if (policy?.hours === 168) return 'Weekly';
  return 'Manual';
}

function BackupScheduleForm({ database, onSaved }: { database: DatabaseItem; onSaved: () => Promise<void> }) {
  const policy = database.backup;
  const [frequency, setFrequency] = useState(policy?.cron ? 'calendar' : String(policy?.hours || 0));
  const [saving, setSaving] = useState(false);
  const notify = useToast();
  const calendar = frequency === 'calendar';
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const fields = new FormData(event.currentTarget);
    const cron = String(fields.get('cron') || '').trim();
    const timezone = String(fields.get('timezone') || '').trim();
    const next = { hours: calendar ? 0 : Number(frequency), keep: 1, offsite: fields.get('offsite') === 'on', ...(calendar ? { cron, timezone } : {}) };
    setSaving(true);
    try {
      await patch(`control/servers/${encodeURIComponent(database.server_id)}/databases/${encodeURIComponent(database.app_id)}/backups`, next);
      await onSaved();
      notify('Backup schedule saved.');
    } catch (reason) {
      notify(reason instanceof Error ? reason.message : String(reason), true);
    } finally {
      setSaving(false);
    }
  };
  return <Card className="form-card">
    <SectionHeader title="Backup schedule" description="Set when Līdza takes a backup of this database." />
    <form onSubmit={submit}>
      <div className="form-grid two">
        <Field label="Frequency"><select value={frequency} onChange={event => setFrequency(event.target.value)}>
          <option value="0">Manual only</option><option value="1">Hourly</option><option value="24">Daily</option>
          <option value="168">Weekly</option><option value="calendar">Calendar schedule</option>
        </select></Field>
        {calendar && <>
          <Field label="Cron schedule" hint="Use a fixed minute or an alias such as @monthly.">
            <input name="cron" required defaultValue={policy?.cron} placeholder="0 3 * * 1" maxLength={120} />
          </Field>
          <Field label="Timezone" hint="Use an IANA timezone such as Europe/Tirane.">
            <input name="timezone" defaultValue={policy?.timezone || 'UTC'} maxLength={80} />
          </Field>
        </>}
      </div>
      <label className="check-row"><input type="checkbox" name="offsite" defaultChecked={policy?.offsite} />Copy to off-site storage</label>
      <Button type="submit" variant="primary" disabled={saving || Boolean(database.operation)}>Save backup schedule</Button>
    </form>
  </Card>;
}

function databaseStatus(database: DatabaseItem) {
  return database.unavailable ? 'offline' : database.operation || (database.error || database.warnings?.length ? 'needs attention' : database.ready ? 'ready' : 'not ready');
}

function lastBackup(database: DatabaseItem) {
  return [...(database.backups || [])].sort((a, b) => +new Date(b.created) - +new Date(a.created))[0];
}

/** useDatabases loads the database inventory and polls while an operation runs. */
function useDatabases() {
  const [data, setData] = useState<DatabaseItem[]>([]), [loading, setLoading] = useState(true), [error, setError] = useState('');
  const load = async () => { try { const value = await get<{ databases?: DatabaseItem[] } | DatabaseItem[]>('control/databases'); setData(Array.isArray(value) ? value : value.databases || []); setError(''); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setLoading(false); } };
  useEffect(() => { void load(); }, []);
  usePollWhile(data.some(item => item.operation), load);
  return { data, loading, error, load };
}

/** DatabasesPage: the index of databases with their backup state. */
export function DatabasesPage() {
  const { apps } = useWorkspace(); const { data, loading, error, load } = useDatabases();
  const [selected, setSelected] = usePreference<string>('deploy.databases.app', 'all');
  // Unused: data no application uses any more (left by a removal or a move), removable here.
  const [use, setUse] = useState<'all' | 'unused'>('all');
  const visible = useMemo(() => data.filter(item => selected === 'all' || item.app_id === selected || item.attachments?.some(attachment => attachment.app_id === selected)), [data, selected]);
  const columns: Column<DatabaseItem>[] = [
    { id: 'name', label: 'Name', render: row => <ResourceName href={withSticky(resourceHref('databases', row.app_id))} name={row.app_id} detail={row.attachments?.length ? `Used by ${row.attachments.map(item => item.app_id).join(', ')}` : undefined} /> },
    { id: 'status', label: 'Status', render: row => <><StatusBadge status={databaseStatus(row)} /><DatabaseOperation database={row} /></> },
    { id: 'engine', label: 'Engine', render: row => `${databaseLabel(row)} · ${row.server_id}` },
    { id: 'schedule', label: 'Schedule', render: row => <span className="resource-fact"><span>{backupScheduleLabel(row.backup)}</span>{row.backup?.offsite && <small className="muted-copy">off-site copy</small>}</span> },
    { id: 'backups', label: 'Backups', render: row => { const last = lastBackup(row); return <span className="resource-fact"><span>{row.backups?.length || 0}</span>{last && <small className="muted-copy">latest {relativeTime(last.created)}</small>}</span>; } },
    { id: 'size', label: 'Size', render: row => row.size_bytes ? formatBytes(row.size_bytes) : '—' },
  ];
  return <ResourceIndexPage resource="databases" title="Databases" description="Every database the panel backs up, with its schedule and latest backup." count={data.length} loading={loading}
    actions={<Button onClick={load}><RefreshCw size={16} />Refresh</Button>}
    notices={error ? <ErrorBanner>{error}</ErrorBanner> : undefined}
    list={use === 'unused' ? <RetainedResources appID={selected === 'all' ? undefined : selected} unused /> : undefined}
    filters={<><Field label="Show" className="toolbar-filter"><select aria-label="Show" value={use} onChange={event => setUse(event.target.value as 'all' | 'unused')}><option value="all">All databases</option><option value="unused">Unused (no application uses them)</option></select></Field><Field label="Application" className="toolbar-filter"><select aria-label="Application" value={selected} onChange={event => setSelected(event.target.value)}><option value="all">All applications</option>{apps.map(app => <option key={app.id}>{app.id}</option>)}</select></Field></>}
    rows={visible} columns={columns} rowKey={row => `${row.server_id}-${row.app_id}`}
    empty={<EmptyState icon={<Database />} title="No databases found" description={selected === 'all' ? 'A database appears here once an application has one attached.' : `No database is attached to ${selected}.`} />} />;
}

/** DatabaseDetailPage: one database. Tabs: Overview, Backups, Schedule, Excluded tables, Settings. The id is the database's, or an application's it is attached to. */
export function DatabaseDetailPage({ appID, tab = 'overview' }: { appID: string; tab?: string }) {
  const { data, loading, error, load } = useDatabases();
  const database = data.find(item => item.app_id === appID) || data.find(item => item.attachments?.some(attachment => attachment.app_id === appID));
  const { apps } = useWorkspace();
  const notify = useToast();
  // Backups stay available during a move; schedules, policies and restores are locked.
  const move = useAppMoveLock(appID);
  const backup = async () => { if (!database) return; try { await post(`control/servers/${encodeURIComponent(database.server_id)}/databases/${encodeURIComponent(database.app_id)}/backup`); notify('Backup queued.'); await load(); } catch (reason) { notify(reason instanceof Error ? reason.message : String(reason), true); } };
  const configurable = Boolean(database?.ready && !database.retained);
  const tabs = visibleTabs('databases', [!(configurable && database?.excluded_tables_supported && !database?.engine) && 'excluded']);
  return <ResourceDetailPage resource="databases" id={appID} tab={tab} tabs={tabs} title={database?.app_id || appID}
    status={database && <StatusBadge status={databaseStatus(database)} />}
    facts={database ? [`${databaseLabel(database)} · ${database.server_id}`, database.size_bytes ? formatBytes(database.size_bytes) : undefined, `${database.backups?.length || 0} backups`] : undefined}
    actions={database ? <Button variant="primary" onClick={backup} disabled={!!database.operation}>Back up now</Button> : undefined}
    notices={<>{error && <ErrorBanner>{error}</ErrorBanner>}{database && <DatabaseWarnings database={database} />}{database && <DatabaseOperation database={database} />}<MoveLockCard appID={appID} lock={move} /></>}
    missing={loading ? <Loading label="Loading database…" /> : !database ? <EmptyState icon={<Database />} title="No database attached" description="A private or external PostgreSQL database is attached when an application is created." /> : undefined}>
    {database && tab === 'overview' && <DatabaseOverview database={database} apps={apps.map(app => app.id)} />}
    {database && tab === 'backups' && <DatabaseBackups database={database} appID={appID} nativeApp={apps.some(app => app.native && (app.id === appID || app.id === database.app_id || database.attachments?.some(attachment => attachment.app_id === app.id)))} locked={move} onChanged={load} />}
    {database && tab === 'schedule' && (configurable ? <MoveLockFieldset lock={move}><div className="settings-stack"><BackupScheduleForm key={JSON.stringify([database.server_id, database.app_id, database.backup])} database={database} onSaved={load} />{database.backup?.offsite && <DatabaseRetentionCard key={JSON.stringify([database.server_id, database.app_id, database.backup.retention])} serverID={database.server_id} databaseID={database.app_id} override={database.backup.retention} disabled={Boolean(database.operation)} onSaved={load} />}</div></MoveLockFieldset> : <Notice>{database.retained ? 'A retained database keeps its last schedule; it takes no new backups.' : 'The schedule can be set once the database is ready.'}</Notice>)}
    {database && tab === 'excluded' && <MoveLockFieldset lock={move}><BackupExcludedTablesCard key={JSON.stringify([database.server_id, database.app_id, database.excluded_tables])} serverID={database.server_id} databaseID={database.app_id} excluded={database.excluded_tables || []} disabled={Boolean(database.operation)} onSaved={load} /></MoveLockFieldset>}
    {database && tab === 'settings' && (configurable ? <MoveLockFieldset lock={move}><Card className="form-card"><SectionHeader title="Before deploys" description={database.primary ? 'This is a primary database: every deployment is backed up first, fresh or from a recent verified backup.' : 'What a deployment of an attached application does about this database before it changes anything.'} /><BeforeDeployControl key={JSON.stringify([database.server_id, database.app_id, database.before_deploy])} serverID={database.server_id} databaseID={database.app_id} policy={database.before_deploy} primary={Boolean(database.primary)} disabled={Boolean(database.operation)} onSaved={load} /></Card></MoveLockFieldset> : <Notice>{database.retained ? 'This database is retained after its application was removed; it has no deployment settings.' : 'Settings are available once the database is ready.'}</Notice>)}
  </ResourceDetailPage>;
}

/** Overview tab: what the database is and its backup state. */
function DatabaseOverview({ database, apps }: { database: DatabaseItem; apps: string[] }) {
  const last = lastBackup(database);
  return <Card><SectionHeader title="Summary" /><SummaryList items={[
    { label: 'Engine', value: databaseLabel(database) },
    { label: 'Server', value: <a href={withSticky(resourceHref('servers', database.server_id))}>{database.server_id}</a> },
    { label: 'Status', value: <StatusBadge status={databaseStatus(database)} /> },
    database.size_bytes ? { label: 'Size', value: formatBytes(database.size_bytes) } : false,
    { label: 'Used by', value: database.attachments?.length ? <span className="value-list">{database.attachments.map(item => apps.includes(item.app_id) ? <a key={item.app_id} href={withSticky(resourceHref('applications', item.app_id))}>{item.app_id}{item.env_key ? ` (${item.env_key})` : ''}</a> : <span key={item.app_id}>{item.app_id}</span>)}</span> : apps.includes(database.app_id) ? <a href={withSticky(resourceHref('applications', database.app_id))}>{database.app_id}</a> : 'No application' },
    { label: 'Schedule', value: backupScheduleLabel(database.backup) },
    { label: 'Local retention', value: 'Latest backup only' },
    { label: 'Off-site', value: database.backup?.offsite ? 'Enabled' : 'Off' },
    { label: 'Before deploys', value: beforeDeployLabel(database.before_deploy) },
    database.excluded_tables?.length ? { label: 'Skipped table data', value: `${database.excluded_tables.length} ${database.excluded_tables.length === 1 ? 'table' : 'tables'}` } : false,
    { label: 'Latest backup', value: last ? <a href={withSticky(resourceHref('databases', database.app_id, 'backups'))}>{formatDate(last.created)}</a> : 'None yet' },
    database.error && { label: 'Problem', value: database.error },
  ]} /></Card>;
}

/** Backups tab: the backup history with download, restore and off-site upload. */
function DatabaseBackups({ database, appID, nativeApp, locked: move, onChanged }: { database: DatabaseItem; appID: string; nativeApp: boolean; locked: ReturnType<typeof useAppMoveLock>; onChanged: () => Promise<void> }) {
  const [restoreBackup, setRestoreBackup] = useState<Backup>();
  const [restoreTarget, setRestoreTarget] = useState('');
  const notify = useToast();
  const restore = async (target: string) => {
    if (!restoreBackup) return;
    await post(`control/apps/${encodeURIComponent(appID)}/restore`, { source: database.app_id, backup: restoreBackup.id, target, env_key: 'DATABASE_URL' });
    notify('Restore queued into a new database.');
  };
  const restoreInPlace = async (row: Backup) => {
    await post(`control/servers/${encodeURIComponent(database.server_id)}/databases/${encodeURIComponent(database.app_id)}/backups/${encodeURIComponent(row.id)}/restore`);
    notify(database.engine === 'clickhouse' ? 'ClickHouse restore queued.' : 'Restore queued.');
    await onChanged();
  };
  const columns: Column<Backup>[] = [
    { id: 'created', label: 'Created', render: row => formatDate(row.created) },
    { id: 'purpose', label: 'Purpose', render: row => row.kind === 'predeployment' ? 'Pre-deployment' : 'Scheduled / manual' },
    { id: 'size', label: 'Size', render: row => <div><span>{formatBytes(row.size)}</span>{row.excluded_table_data?.length ? <small className="muted-copy backup-skipped" title={row.excluded_table_data.join(', ')}>{skippedTablesLabel(row.excluded_table_data)}</small> : null}</div> },
    { id: 'copies', label: 'Copies', render: row => <div><strong>{row.offsite ? 'Local + S3 verified' : row.upload_error ? 'Local · upload failed' : row.offsite_requested ? 'Local · upload pending' : 'Local only'}</strong>{row.upload_error && <small className="text-danger">{row.upload_error}</small>}{row.object_key && <code className="object-key">{row.object_key}</code>}</div> },
    { id: 'actions', label: 'Actions', render: row => <div className="row-actions"><a className="button button-secondary button-sm" download href={`/api/control/servers/${encodeURIComponent(database.server_id)}/databases/${encodeURIComponent(database.app_id)}/backups/${encodeURIComponent(row.id)}`}><Download size={15} />Download</a>{database.engine && database.engine !== 'sqlite' ? <ConfirmAction title={database.engine === 'clickhouse' ? 'Restore this ClickHouse backup?' : 'Restore this backup?'} description={database.engine === 'clickhouse' ? <>The ClickHouse database <strong>{database.app_id}</strong> is replaced with backup <strong>{row.id}</strong> from {formatDate(row.created)}. Writes made since then are lost; the restored row counts are verified.</> : <>The data of <strong>{database.app_id}</strong> is replaced with backup <strong>{row.id}</strong> from {formatDate(row.created)} by the plugin that provides the service. Writes made since then are lost.</>} confirmLabel="Replace and restore" destructive onConfirm={() => restoreInPlace(row)}><Button size="sm" disabled={move.locked} title={move.reason || undefined}><RotateCcw size={15} />Restore</Button></ConfirmAction> : database.engine !== 'sqlite' && !nativeApp && <Button size="sm" disabled={move.locked} title={move.reason || undefined} onClick={() => { setRestoreBackup(row); setRestoreTarget(`restore-${Date.now().toString(36)}`); }}><RotateCcw size={15} />Restore</Button>}{!row.offsite && <Button size="sm" onClick={async () => { await post(`control/servers/${encodeURIComponent(database.server_id)}/databases/${encodeURIComponent(database.app_id)}/backups/${encodeURIComponent(row.id)}/upload`); notify('Off-site upload queued.'); }}><Cloud size={15} />Upload</Button>}</div> },
  ];
  return <><Card><SectionHeader title="Backup history" description={database.engine === 'clickhouse' ? 'A ClickHouse restore replaces the database in place and verifies the restored row counts.' : database.engine && database.engine !== 'sqlite' ? 'A restore replaces the service\'s data in place, through the plugin that provides it.' : 'Each row shows whether the off-site copy was verified.'} /><ResourceList rows={[...(database.backups || [])].sort((a, b) => +new Date(b.created) - +new Date(a.created))} columns={columns} rowKey={row => row.id} empty={<EmptyState icon={<HardDrive />} title="No backups yet" description="Back up now or wait for the next scheduled run." />} /></Card>
    <PromptDialog open={Boolean(restoreBackup)} title="Restore backup into a new database" description={<>Restore backup <strong>{restoreBackup?.id}</strong> from <strong>{database.app_id}</strong>. The existing database will not be overwritten.</>} label="New database ID" value={restoreTarget} submitLabel="Start restore" inputProps={{ autoComplete: 'off', spellCheck: false, maxLength: 48, pattern: '[a-z][a-z0-9-]{0,47}', title: 'Use lowercase letters, numbers, and hyphens; begin with a letter.' }} onValueChange={setRestoreTarget} onOpenChange={open => { if (!open) { setRestoreBackup(undefined); setRestoreTarget(''); } }} onSubmit={restore} /></>;
}

/** RemoteBackupsPage: the off-site bucket; Objects and Retention tabs. */
export function RemoteBackupsPage({ tab = 'objects' }: { tab?: string }) {
  const [items, setItems] = useState<RemoteBackup[]>([]), [meta, setMeta] = useState<{ bucket?: string; prefix?: string; checked?: string }>({}), [query, setQuery] = usePreference<string>('deploy.backups.query', ''), [loading, setLoading] = useState(true), [error, setError] = useState('');
  const load = async () => { try { const value = await get<{ objects?: RemoteBackup[]; bucket?: string; prefix?: string; checked?: string }>('control/storage/backups'); setItems(value.objects || []); setMeta(value); setError(''); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setLoading(false); } };
  useEffect(() => { if (tab === 'objects') void load(); }, [tab]); const visible = items.filter(item => item.key.toLowerCase().includes(query.toLowerCase()));
  const columns: Column<RemoteBackup>[] = [{ id: 'object', label: 'Object', render: row => <div><code className="object-key">{meta.bucket}/{row.prefix || meta.prefix}/{row.key}</code>{row.legacy && <Badge tone="warning">Legacy location</Badge>}</div> }, { id: 'size', label: 'Size', render: row => formatBytes(row.size) }, { id: 'modified', label: 'Modified', render: row => formatDate(row.modifiedAt) }, { id: 'actions', label: 'Download', render: row => <a className="button button-secondary button-sm" href={`/api/control/storage/backups/download?key=${encodeURIComponent(row.key)}${row.legacy ? '&legacy=1' : ''}`}><Download size={15} />Download</a> }];
  return <ResourceDetailPage resource="remote-backups" tab={tab} title="Remote backups" description="Backup objects confirmed in your private S3-compatible bucket, and how long they are kept." facts={meta.bucket ? [<code>{meta.bucket}</code>, `${items.length} objects`] : undefined} actions={tab === 'objects' ? <Button onClick={load}><RefreshCw size={16} />Check storage</Button> : undefined} notices={error ? <ErrorBanner>{error}</ErrorBanner> : undefined}>
    {tab === 'objects' && <Card><Toolbar><SearchField value={query} onChange={event => setQuery(event.target.value)} placeholder="Filter backup objects…" aria-label="Filter backup objects" /></Toolbar>{loading ? <Loading /> : <ResourceList rows={visible} columns={columns} rowKey={row => `${row.prefix}-${row.key}`} empty={<EmptyState icon={<CloudOff />} title="No remote backups found" description="Enable off-site copies on a database and run a backup, then check again." />} />}</Card>}
    {tab === 'retention' && <BackupRetentionCard />}
  </ResourceDetailPage>;
}
