import { usePreference } from '../state/PreferencesContext';
import { detailHref, href, navigate } from '../lib/router';
import { Cloud, CloudOff, Database, Download, HardDrive, RefreshCw, RotateCcw, Search } from 'lucide-react';
import { type FormEvent, useEffect, useMemo, useRef, useState } from 'react';
import { get, patch, post } from '../lib/api';
import { formatBytes, formatDate } from '../lib/format';
import { useWorkspace } from '../state/WorkspaceContext';
import { useToast } from '../components/ToastProvider';
import { MoveLockCard, MoveLockFieldset, useAppMoveLock } from '../components/MoveLock';
import { Badge, Button, Card, ConfirmAction, EmptyState, ErrorBanner, Field, Loading, PageHeader, PromptDialog, SearchField, SectionHeader, StatusBadge, Toolbar } from '../components/ui';
import { ResourceList, type Column } from '../components/ResourceList';
import { LiveProgress, type OperationProgress } from '../components/LiveProgress';
import { BackupRetentionCard, DatabaseRetentionCard, type RetentionPolicy } from '../components/BackupRetention';
import { RetainedResources } from '../components/RetainedResources';
import { BeforeDeployControl, type BeforeDeployPolicy, beforeDeployLabel } from '../components/BeforeDeployBackup';
import { BackupExcludedTablesCard, skippedTablesLabel } from '../components/BackupExcludedTables';

interface Backup { id: string; created: string; size?: number; kind?: string; offsite?: boolean; offsite_requested?: boolean; upload_error?: string; object_key?: string; sha256?: string; excluded_table_data?: string[] }
interface BackupPolicy { hours?: number; cron?: string; timezone?: string; keep?: number; offsite?: boolean; retention?: RetentionPolicy | null }
interface DatabaseItem { app_id: string; server_id: string; mode?: string; engine?: 'sqlite' | 'clickhouse'; size_bytes?: number; ready?: boolean; retained?: boolean; unavailable?: boolean; error?: string; operation?: string; progress?: OperationProgress; backup?: BackupPolicy; backups?: Backup[]; attachments?: Array<{ app_id: string; env_key?: string }>; before_deploy?: BeforeDeployPolicy; primary?: boolean; excluded_tables?: string[]; excluded_tables_supported?: boolean }
function databaseLabel(database: DatabaseItem) {
  if (database.engine === 'sqlite') return 'Native SQLite';
  if (database.engine === 'clickhouse') return 'ClickHouse';
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

export function DatabasesPage() {
  const { apps } = useWorkspace(); const [data, setData] = useState<DatabaseItem[]>([]), [selected, setSelected] = usePreference<string>('deploy.databases.app', 'all'), [loading, setLoading] = useState(true), [error, setError] = useState('');
  const load = async () => { try { const value = await get<{ databases?: DatabaseItem[] } | DatabaseItem[]>('control/databases'); setData(Array.isArray(value) ? value : value.databases || []); setError(''); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setLoading(false); } };
  useEffect(() => { load(); }, []);
  usePollWhile(data.some(item => item.operation), load);
  const visible = useMemo(() => data.filter(item => selected === 'all' || item.app_id === selected || item.attachments?.some(attachment => attachment.app_id === selected)), [data, selected]);
  return <><PageHeader eyebrow="Data protection" title="Databases & backups" description="A searchable inventory. Open one application to configure schedules, create backups, or restore." actions={<Button onClick={load}><RefreshCw size={16} />Refresh</Button>} />{error && <ErrorBanner>{error}</ErrorBanner>}<Toolbar><div><h2>Attached databases</h2><Badge>{data.length}</Badge></div><Field label="Application" className="toolbar-filter"><select value={selected} onChange={event => setSelected(event.target.value)}><option value="all">All applications</option>{apps.map(app => <option key={app.id}>{app.id}</option>)}</select></Field></Toolbar>{loading ? <Loading /> : <div className="database-grid">{visible.map(database => <Card className="database-card" key={`${database.server_id}-${database.app_id}`}><div className="database-card-heading"><span className="server-mark"><Database size={21} /></span><div><h2>{database.app_id}</h2><p>{databaseLabel(database)} · {database.server_id}</p></div><StatusBadge status={database.unavailable ? 'offline' : database.operation || (database.error ? 'needs attention' : database.ready ? 'ready' : 'not ready')} /></div><DatabaseOperation database={database} /><div className="database-policy"><span>Schedule <strong>{backupScheduleLabel(database.backup)}</strong></span><span>Local retention <strong>Latest backup only</strong></span><span>Off-site <strong>{database.backup?.offsite ? 'Enabled' : 'Off'}</strong></span><span>Before deploys <strong>{beforeDeployLabel(database.before_deploy)}</strong></span>{database.excluded_tables?.length ? <span>Skipped table data <strong>{database.excluded_tables.length} {database.excluded_tables.length === 1 ? 'table' : 'tables'}</strong></span> : null}<span>Backups <strong>{database.backups?.length || 0}</strong></span>{database.size_bytes ? <span>Size <strong>{formatBytes(database.size_bytes)}</strong></span> : null}</div><Button variant="primary" onClick={() => { navigate(detailHref('database', database.app_id)); }}>Manage database & backups</Button></Card>)}{!visible.length && <EmptyState icon={<Database />} title="No databases found" description={selected === 'all' ? 'Attach a database from an application’s settings.' : `No database is attached to ${selected}.`} />}</div>}<RetainedResources appID={selected === 'all' ? undefined : selected} /></>;
}

export function DatabaseDetailPage({ appID }: { appID: string }) {
  const [database, setDatabase] = useState<DatabaseItem>(), [loading, setLoading] = useState(true), [error, setError] = useState('');
  // Restoring into a new database creates a container database; it is not
  // offered for a native application's databases.
  const { apps } = useWorkspace();
  const nativeApp = apps.some(app => app.native && (app.id === appID || app.id === database?.app_id || database?.attachments?.some(attachment => attachment.app_id === app.id)));
  const [restoreBackup, setRestoreBackup] = useState<Backup>();
  const [restoreTarget, setRestoreTarget] = useState('');
  const notify = useToast();
  // Backups stay available during a move; schedules, policies and restores are locked.
  const move = useAppMoveLock(appID);
  const load = async () => { try { const value = await get<{ databases?: DatabaseItem[] } | DatabaseItem[]>('control/databases'); const list = Array.isArray(value) ? value : value.databases || []; setDatabase(list.find(item => item.app_id === appID || item.attachments?.some(attachment => attachment.app_id === appID))); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setLoading(false); } };
  useEffect(() => { load(); }, [appID]);
  usePollWhile(!!database?.operation, load);
  const backup = async () => { if (!database) return; try { await post(`control/servers/${encodeURIComponent(database.server_id)}/databases/${encodeURIComponent(database.app_id)}/backup`); notify('Backup queued.'); await load(); } catch (reason) { notify(reason instanceof Error ? reason.message : String(reason), true); } };
  const restore = async (target: string) => {
    if (!database || !restoreBackup) return;
    await post(`control/apps/${encodeURIComponent(appID)}/restore`, { source: database.app_id, backup: restoreBackup.id, target, env_key: 'DATABASE_URL' });
    notify('Restore queued into a new database.');
  };
  const restoreInPlace = async (row: Backup) => {
    if (!database) return;
    await post(`control/servers/${encodeURIComponent(database.server_id)}/databases/${encodeURIComponent(database.app_id)}/backups/${encodeURIComponent(row.id)}/restore`);
    notify('ClickHouse restore queued.');
    await load();
  };
  if (loading) return <Loading overlay label="Loading database…" />;
  if (!database) return <><PageHeader title={`${appID} database`} back={href('databases')} /><EmptyState icon={<Database />} title="No database attached" description="Attach a private or external PostgreSQL database from application settings." /></>;
  const columns: Column<Backup>[] = [
    { id: 'created', label: 'Created', render: row => formatDate(row.created) },
    { id: 'purpose', label: 'Purpose', render: row => row.kind === 'predeployment' ? 'Pre-deployment' : 'Scheduled / manual' },
    { id: 'size', label: 'Size', render: row => <div><span>{formatBytes(row.size)}</span>{row.excluded_table_data?.length ? <small className="muted-copy backup-skipped" title={row.excluded_table_data.join(', ')}>{skippedTablesLabel(row.excluded_table_data)}</small> : null}</div> },
    { id: 'copies', label: 'Copies', render: row => <div><strong>{row.offsite ? 'Local + S3 verified' : row.upload_error ? 'Local · upload failed' : row.offsite_requested ? 'Local · upload pending' : 'Local only'}</strong>{row.upload_error && <small className="text-danger">{row.upload_error}</small>}{row.object_key && <code className="object-key">{row.object_key}</code>}</div> },
    { id: 'actions', label: 'Actions', render: row => <div className="row-actions"><a className="button button-secondary button-sm" download href={`/api/control/servers/${encodeURIComponent(database.server_id)}/databases/${encodeURIComponent(database.app_id)}/backups/${encodeURIComponent(row.id)}`}><Download size={15} />Download</a>{database.engine === 'clickhouse' ? <ConfirmAction title="Restore this ClickHouse backup?" description={<>The ClickHouse database <strong>{database.app_id}</strong> is replaced with backup <strong>{row.id}</strong> from {formatDate(row.created)}. Writes made since then are lost; the restored row counts are verified.</>} confirmLabel="Replace and restore" destructive onConfirm={() => restoreInPlace(row)}><Button size="sm" disabled={move.locked} title={move.reason || undefined}><RotateCcw size={15} />Restore</Button></ConfirmAction> : database.engine !== 'sqlite' && !nativeApp && <Button size="sm" disabled={move.locked} title={move.reason || undefined} onClick={() => { setRestoreBackup(row); setRestoreTarget(`restore-${Date.now().toString(36)}`); }}><RotateCcw size={15} />Restore</Button>}{!row.offsite && <Button size="sm" onClick={async () => { await post(`control/servers/${encodeURIComponent(database.server_id)}/databases/${encodeURIComponent(database.app_id)}/backups/${encodeURIComponent(row.id)}/upload`); notify('Off-site upload queued.'); }}><Cloud size={15} />Upload</Button>}</div> },
  ];
  return <><PageHeader eyebrow="Database & backups" title={appID} description={`${databaseLabel(database)} · ${database.server_id}`} back={href('databases')} actions={<Button variant="primary" onClick={backup} disabled={!!database.operation}>Back up now</Button>} />{error && <ErrorBanner>{error}</ErrorBanner>}<DatabaseOperation database={database} /><MoveLockCard appID={appID} lock={move} /><MoveLockFieldset lock={move}>{database.ready && !database.retained && <BackupScheduleForm key={JSON.stringify([database.server_id, database.app_id, database.backup])} database={database} onSaved={load} />}{database.ready && !database.retained && <Card className="form-card"><SectionHeader title="Before deploys" description={database.primary ? 'This is a primary database: every deployment is backed up first, fresh or from a recent verified backup.' : 'What a deployment of an attached application does about this database before it changes anything.'} /><BeforeDeployControl key={JSON.stringify([database.server_id, database.app_id, database.before_deploy])} serverID={database.server_id} databaseID={database.app_id} policy={database.before_deploy} primary={Boolean(database.primary)} disabled={Boolean(database.operation)} onSaved={load} /></Card>}{database.ready && !database.retained && database.excluded_tables_supported && !database.engine && <BackupExcludedTablesCard key={JSON.stringify([database.server_id, database.app_id, database.excluded_tables])} serverID={database.server_id} databaseID={database.app_id} excluded={database.excluded_tables || []} disabled={Boolean(database.operation)} onSaved={load} />}{database.ready && !database.retained && database.backup?.offsite && <DatabaseRetentionCard key={JSON.stringify([database.server_id, database.app_id, database.backup.retention])} serverID={database.server_id} databaseID={database.app_id} override={database.backup.retention} disabled={Boolean(database.operation)} onSaved={load} />}</MoveLockFieldset><Card><SectionHeader title="Backup history" description="Each row explicitly shows whether the off-site object was verified." /><ResourceList rows={[...(database.backups || [])].sort((a, b) => +new Date(b.created) - +new Date(a.created))} columns={columns} rowKey={row => row.id} empty={<EmptyState icon={<HardDrive />} title="No backups yet" description="Create one now or wait for the next scheduled run." />} /></Card><PromptDialog open={Boolean(restoreBackup)} title="Restore backup into a new database" description={<>Restore backup <strong>{restoreBackup?.id}</strong> from <strong>{database.app_id}</strong>. The existing database will not be overwritten.</>} label="New database ID" value={restoreTarget} submitLabel="Start restore" inputProps={{ autoComplete: 'off', spellCheck: false, maxLength: 48, pattern: '[a-z][a-z0-9-]{0,47}', title: 'Use lowercase letters, numbers, and hyphens; begin with a letter.' }} onValueChange={setRestoreTarget} onOpenChange={open => { if (!open) { setRestoreBackup(undefined); setRestoreTarget(''); } }} onSubmit={restore} /></>;
}

export function RemoteBackupsPage() {
  const [items, setItems] = useState<RemoteBackup[]>([]), [meta, setMeta] = useState<{ bucket?: string; prefix?: string; checked?: string }>({}), [query, setQuery] = usePreference<string>('deploy.backups.query', ''), [loading, setLoading] = useState(true), [error, setError] = useState('');
  const load = async () => { try { const value = await get<{ objects?: RemoteBackup[]; bucket?: string; prefix?: string; checked?: string }>('control/storage/backups'); setItems(value.objects || []); setMeta(value); setError(''); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setLoading(false); } };
  useEffect(() => { load(); }, []); const visible = items.filter(item => item.key.toLowerCase().includes(query.toLowerCase()));
  const columns: Column<RemoteBackup>[] = [{ id: 'object', label: 'Object', render: row => <div><code className="object-key">{meta.bucket}/{row.prefix || meta.prefix}/{row.key}</code>{row.legacy && <Badge tone="warning">Legacy location</Badge>}</div> }, { id: 'size', label: 'Size', render: row => formatBytes(row.size) }, { id: 'modified', label: 'Modified', render: row => formatDate(row.modifiedAt) }, { id: 'actions', label: 'Actions', render: row => <a className="button button-secondary button-sm" href={`/api/control/storage/backups/download?key=${encodeURIComponent(row.key)}${row.legacy ? '&legacy=1' : ''}`}><Download size={15} />Download</a> }];
  return <><PageHeader eyebrow="Off-site backups" title="Remote backups" description="Objects confirmed in your private S3-compatible bucket." actions={<Button onClick={load}><RefreshCw size={16} />Check storage</Button>} />{error && <ErrorBanner>{error}</ErrorBanner>}<Card><Toolbar><div><h2>{meta.bucket || 'Backup bucket'}</h2><Badge>{items.length} objects</Badge></div><SearchField value={query} onChange={event => setQuery(event.target.value)} placeholder="Filter backup objects…" /></Toolbar>{loading ? <Loading /> : <ResourceList rows={visible} columns={columns} rowKey={row => `${row.prefix}-${row.key}`} empty={<EmptyState icon={<CloudOff />} title="No remote backups found" description="Enable off-site copies on a database and run a backup, then check again." />} />}</Card><BackupRetentionCard /></>;
}
