import * as AlertDialog from '@radix-ui/react-alert-dialog';
import { type FormEvent, useEffect, useId, useRef, useState } from 'react';
import { AlertTriangle, Archive, LoaderCircle, RefreshCw, Trash2 } from 'lucide-react';
import { del, get } from '../lib/api';
import { formatBytes, relativeTime } from '../lib/format';
import { useToast } from './ToastProvider';
import { ActionForm, Badge, Button, Card, EmptyState, ErrorBanner, Loading, SectionHeader } from './ui';
import { ResourceList, type Column } from './ResourceList';

export interface RetainedBackup { id: string; created: string; size?: number; offsite?: boolean; kind?: string }
export interface RetainedDatabase { id: string; volume?: string; engine?: string; size_bytes?: number; last_local_backup?: RetainedBackup; last_offsite_backup?: RetainedBackup; covered?: boolean; attached_to?: string[] }
export interface RetainedComponent { kind: string; name: string; size_bytes?: number; measured?: boolean; host?: boolean }
export interface RetainedResource {
  id: string; app_id: string; server_id: string; server_name?: string; kind?: 'application' | 'database'; created?: string; writes_stopped?: string; data_since?: string;
  components?: RetainedComponent[]; databases?: RetainedDatabase[]; size_bytes?: number; size_pending?: boolean;
  attached_to?: string[]; final_backup_required?: boolean; removing?: boolean; error?: string;
  reason?: 'moved' | 'removed'; runs_on?: string; runs_on_name?: string; moved_at?: string;
  /** The move the data came from, when known; copy marks a never-switched move's partial copy, removable without a backup. */
  origin?: { move_id: string; source_server?: string; switched: boolean }; copy?: boolean;
}

/** Above this size the removal needs the application name typed. */
export const TYPED_REMOVAL_BYTES = 1024 ** 3;

const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);
const engineLabels: Record<string, string> = { postgresql: 'PostgreSQL', clickhouse: 'ClickHouse' };
const componentLabels: Record<string, string> = { database: 'PostgreSQL', cache: 'Cache', clickhouse: 'ClickHouse', files: 'Files', volume: 'Volume', redis: 'Redis' };

const known = (value?: string) => Boolean(value && !value.startsWith('0001-'));
export function formatDay(value: string) {
  return new Date(value).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
}

/** retainedSince is when the data was left here: the move's end, the record, or the volume's creation. */
export function retainedSince(item: RetainedResource): string {
  if (item.reason === 'moved' && known(item.moved_at)) return formatDay(item.moved_at!);
  if (known(item.created)) return formatDay(item.created!);
  if (known(item.data_since)) return formatDay(item.data_since!);
  return 'Date unknown';
}

function sinceDate(item: RetainedResource) {
  const since = retainedSince(item);
  return since === 'Date unknown' ? '' : ` on ${since}`;
}

/** retainedWhy explains why a resource is still on its server. */
export function retainedWhy(item: RetainedResource) {
  const where = item.runs_on_name || item.runs_on;
  if (item.copy) return `Partial copy from a move that never switched${where ? `; the app still runs on ${where}` : ''}`;
  if (item.reason === 'moved') return `Copy left by the move${where ? ` to ${where}` : ''}${sinceDate(item)}${where ? ' (the app runs there now)' : ''}`;
  if (item.kind === 'database') return `Database left when ${item.app_id} was removed${sinceDate(item)}; no application uses it`;
  return `Left when ${item.app_id} was removed${sinceDate(item)}`;
}

/** parts lists a resource's databases (engine and size), then its other volumes and host services. */
export function retainedParts(item: RetainedResource) {
  const parts: Array<{ key: string; label: string; size?: number }> = [];
  const databaseVolumes = new Set((item.databases || []).map(db => db.volume).filter(Boolean));
  for (const db of item.databases || []) parts.push({ key: `db/${db.id}`, label: `${engineLabels[db.engine || 'postgresql'] || db.engine} ${db.id}`, size: db.size_bytes || undefined });
  for (const c of item.components || []) {
    if (databaseVolumes.has(c.name) || c.host && c.kind === 'database' && (item.databases || []).length) continue;
    parts.push({ key: `c/${c.kind}/${c.name}`, label: `${componentLabels[c.kind] || c.kind} ${c.name}${c.host ? ' (on the host)' : ''}`, size: c.measured ? c.size_bytes : undefined });
  }
  return parts;
}

/** lastBackup is the newest backup of the resource's databases, with where copies are. */
export function lastBackup(item: RetainedResource): { text: string; title?: string; missing?: boolean } {
  const dbs = item.databases || [];
  if (!dbs.length) return { text: 'No database' };
  let newest: RetainedBackup | undefined;
  let offsite = false, local = false;
  for (const db of dbs) {
    for (const b of [db.last_offsite_backup, db.last_local_backup]) if (b && (!newest || +new Date(b.created) > +new Date(newest.created))) newest = b;
    offsite ||= Boolean(db.last_offsite_backup);
    local ||= Boolean(db.last_local_backup);
  }
  if (!newest) return { text: 'No backup', missing: true, title: 'No backup of this data exists. Removing it takes a final backup first.' };
  const where = [offsite && 'off-site', local && 'local'].filter(Boolean).join(' + ');
  return { text: `${relativeTime(newest.created)} · ${where}`, title: new Date(newest.created).toLocaleString() };
}

/** removalSummary says exactly what removing a retained resource deletes and keeps. */
export function removalSummary(item: RetainedResource) {
  const deleted = retainedParts(item).map(part => `${part.label}${part.size ? ` (${formatBytes(part.size)})` : ''}`);
  const backups = item.reason === 'moved' || item.copy
    ? `Its backups are not deleted: they belong to ${item.app_id}${item.runs_on_name || item.runs_on ? ` on ${item.runs_on_name || item.runs_on}` : ''}.`
    : 'Its backup records and off-site copies are not deleted; they follow the off-site retention policy.';
  return { deleted, backups };
}

function RemoveDialog({ item, onClose, onRemoved }: { item?: RetainedResource; onClose: () => void; onRemoved: () => Promise<void> }) {
  const inputID = useId();
  const notify = useToast();
  const [confirm, setConfirm] = useState('');
  const [finalBackup, setFinalBackup] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => { setConfirm(''); setError(''); setBusy(false); setFinalBackup(Boolean(item?.final_backup_required && !item?.copy)); }, [item]);
  const databases = item?.databases || [];
  const required = Boolean(item?.final_backup_required && !item?.copy);
  const typed = Boolean(item && (item.size_pending || (item.size_bytes || 0) > TYPED_REMOVAL_BYTES));
  const ready = !typed || confirm.trim() === item?.app_id;
  const summary = item ? removalSummary(item) : { deleted: [], backups: '' };
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!item || !ready || busy) return;
    setBusy(true); setError('');
    try {
      await del(`control/servers/${encodeURIComponent(item.server_id)}/retained-resources/${encodeURIComponent(item.id)}`, { confirm: item.id, final_backup: finalBackup || required });
      notify(finalBackup || required ? 'Taking a final backup, then removing the retained data.' : 'Removing the retained data.');
      onClose();
      await onRemoved();
    } catch (reason) { setError(message(reason)); } finally { setBusy(false); }
  };
  return <AlertDialog.Root open={Boolean(item)} onOpenChange={open => { if (!open && !busy) onClose(); }}><AlertDialog.Portal><AlertDialog.Overlay className="dialog-overlay" /><AlertDialog.Content className="dialog">
    <ActionForm className="dialog-form" onSubmit={submit}>
      <AlertDialog.Title>Remove the data of {item?.app_id} from {item?.server_name || item?.server_id}?</AlertDialog.Title>
      <AlertDialog.Description>Deletes permanently{item?.size_bytes ? ` (${formatBytes(item.size_bytes)} in total)` : ''}{item?.components?.some(c => !c.host) ? ', with the containers that serve it' : ''}:</AlertDialog.Description>
      <ul className="removal-list" data-removal-list>{summary.deleted.length ? summary.deleted.map(line => <li key={line}>{line}</li>) : <li>Its stored credentials only</li>}</ul>
      <p className="muted-copy">{summary.backups}</p>
      {databases.length > 0 && !item?.copy && <label className="check-row"><input type="checkbox" checked={finalBackup || required} disabled={required || busy} onChange={event => setFinalBackup(event.target.checked)} /><span><strong>Take a final backup first</strong><small>{required ? 'Required: no verified backup of this data was taken after its application stopped writing, so one is taken before anything is deleted.' : 'A backup covering the final state exists; take another only if you want a fresh one (uploaded off-site when configured).'}</small></span></label>}
      {typed && <label className="field" htmlFor={inputID}><span className="field-label">Type <code>{item?.app_id}</code> to confirm</span><input id={inputID} autoComplete="off" spellCheck={false} value={confirm} onChange={event => setConfirm(event.target.value)} disabled={busy} required /></label>}
      {error && <div className="dialog-error" role="alert"><AlertTriangle size={17} /><span>{error}</span></div>}
      <div className="dialog-actions"><AlertDialog.Cancel asChild><Button type="button" disabled={busy}>Cancel</Button></AlertDialog.Cancel><Button type="submit" variant="danger" disabled={busy || !ready}>{busy && <LoaderCircle className="spin" size={16} />}Remove data</Button></div>
    </ActionForm>
  </AlertDialog.Content></AlertDialog.Portal></AlertDialog.Root>;
}

/** group orders rows by server name, then application. */
function groups(items: RetainedResource[]) {
  const byServer = new Map<string, RetainedResource[]>();
  for (const item of items) {
    const list = byServer.get(item.server_id) || [];
    list.push(item);
    byServer.set(item.server_id, list);
  }
  return [...byServer.entries()].map(([server, rows]) => ({ server, name: rows[0].server_name || server, rows: rows.sort((a, b) => a.app_id.localeCompare(b.app_id) || a.id.localeCompare(b.id)) })).sort((a, b) => a.name.localeCompare(b.name));
}

/**
 * RetainedResources lists what moves and older removals left on servers,
 * grouped by server, with a guarded removal. Without serverID it lists
 * every server's resources (optionally one application's).
 */
export function RetainedResources({ serverID, appID }: { serverID?: string; appID?: string }) {
  const [items, setItems] = useState<RetainedResource[]>();
  const [error, setError] = useState('');
  const [removing, setRemoving] = useState<RetainedResource>();
  const [unanswered, setUnanswered] = useState<string[]>([]);
  const load = async () => {
    try {
      let list: RetainedResource[];
      if (serverID) {
        list = await get<RetainedResource[]>(`control/servers/${encodeURIComponent(serverID)}/retained-resources`);
        setUnanswered([]);
      } else {
        const fleet = await get<{ retained_resources?: RetainedResource[]; retained_resources_unavailable_servers?: string[] }>('control/databases');
        list = fleet.retained_resources || [];
        setUnanswered(fleet.retained_resources_unavailable_servers || []);
      }
      setItems((list || []).filter(item => !appID || item.app_id === appID)); setError('');
    } catch (reason) { setError(message(reason)); }
  };
  const latest = useRef(load);
  latest.current = load;
  useEffect(() => { setItems(undefined); void latest.current(); }, [serverID, appID]);
  const busy = Boolean(items?.some(item => item.removing || item.size_pending));
  useEffect(() => {
    if (!busy) return;
    const timer = setInterval(() => { void latest.current(); }, 4000);
    return () => clearInterval(timer);
  }, [busy]);

  const columns: Column<RetainedResource>[] = [
    { id: 'resource', label: 'Application', render: row => <div className="retained-name"><strong>{row.app_id}</strong><small className="retained-id"><code>{row.id}</code></small><ul className="retained-parts">{retainedParts(row).map(part => <li key={part.key}>{part.label}{part.size ? <span> · {formatBytes(part.size)}</span> : null}</li>)}</ul></div> },
    { id: 'size', label: 'Total size', className: 'retained-size-col', render: row => row.size_pending ? <span className="muted-copy">Measuring…</span> : formatBytes(row.size_bytes) },
    { id: 'since', label: 'Retained since', render: row => retainedSince(row) },
    { id: 'why', label: 'Why', render: row => retainedWhy(row) },
    { id: 'backup', label: 'Last backup', render: row => { const backup = lastBackup(row); return <span className={backup.missing ? 'retained-backup-missing' : 'retained-backup'} title={backup.title}>{backup.text}</span>; } },
    { id: 'actions', label: 'Actions', className: 'retained-actions-col', render: row => row.removing ? <Badge tone="warning">Removing…</Badge> : row.attached_to?.length ? <span className="muted-copy">In use by {row.attached_to.join(', ')}</span> : <div className="retained-actions">{row.error && <small className="field-error">{row.error}</small>}<Button size="sm" variant="danger" className="retained-remove" onClick={() => setRemoving(row)}><Trash2 size={15} />Remove</Button></div> },
  ];
  if (appID && !serverID && items && !items.length && !error) return null;
  const header = <SectionHeader title="Retained resources" description="Data that moves and earlier removals left on servers, outside any deleted-app archive. Deleting an application now keeps its data under Deleted apps for 90 days instead. Removing an entry here frees its disk; it cannot be undone." actions={<Button size="sm" onClick={() => { void load(); }}><RefreshCw size={15} />Refresh</Button>} />;
  return <Card data-retained-resources={serverID || appID || 'all'}>
    {header}
    {error && <ErrorBanner>{error}</ErrorBanner>}
    {unanswered.length > 0 && <ErrorBanner>Retained resources of {unanswered.join(', ')} could not be read; they are missing below.</ErrorBanner>}
    {appID && !serverID && <p className="muted-copy retained-filter">Showing {appID} only.</p>}
    {!items && !error ? <Loading label="Loading retained resources…" /> : items && (items.length ? groups(items).map(group => <section key={group.server} className="retained-group" data-retained-server={group.server}>
      <h3 className="retained-server">{group.name}<span className="muted-copy"> · {group.rows.length} {group.rows.length === 1 ? 'entry' : 'entries'} · {formatBytes(group.rows.reduce((sum, row) => sum + (row.size_bytes || 0), 0))}</span></h3>
      <ResourceList rows={group.rows} columns={columns} rowKey={row => `${row.server_id}/${row.id}`} />
    </section>) : <EmptyState icon={<Archive />} title="Nothing retained" description="No move or earlier removal left data here." />)}
    <RemoveDialog item={removing} onClose={() => setRemoving(undefined)} onRemoved={load} />
  </Card>;
}
