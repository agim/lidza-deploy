import * as AlertDialog from '@radix-ui/react-alert-dialog';
import { type FormEvent, useEffect, useId, useRef, useState } from 'react';
import { AlertTriangle, Archive, LoaderCircle, RefreshCw, Trash2 } from 'lucide-react';
import { del, get } from '../lib/api';
import { formatBytes, formatDate } from '../lib/format';
import { useToast } from './ToastProvider';
import { ActionForm, Badge, Button, Card, EmptyState, ErrorBanner, Loading, SectionHeader } from './ui';
import { ResourceList, type Column } from './ResourceList';

export interface RetainedBackup { id: string; created: string; size?: number; offsite?: boolean }
export interface RetainedDatabase { id: string; volume?: string; last_local_backup?: RetainedBackup; last_offsite_backup?: RetainedBackup; covered?: boolean; attached_to?: string[] }
export interface RetainedComponent { kind: string; name: string; size_bytes?: number; measured?: boolean; host?: boolean }
export interface RetainedResource {
  id: string; app_id: string; server_id: string; kind?: 'application' | 'database'; created?: string; writes_stopped?: string;
  components?: RetainedComponent[]; databases?: RetainedDatabase[]; size_bytes?: number; size_pending?: boolean;
  attached_to?: string[]; final_backup_required?: boolean; removing?: boolean; error?: string;
  reason?: 'moved' | 'removed'; runs_on?: string; runs_on_name?: string;
  /** The move the data came from, when known; copy marks a never-switched move's partial copy, removable without a backup. */
  origin?: { move_id: string; source_server?: string; switched: boolean }; copy?: boolean;
}

const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);
const componentLabels: Record<string, string> = { database: 'PostgreSQL', cache: 'Cache', clickhouse: 'ClickHouse', files: 'Files', volume: 'Volume', redis: 'Redis' };

/** removalDescription says what removing a retained resource deletes: Docker volumes, data services on the host, or both. */
export function removalDescription(item?: RetainedResource) {
  const components = item?.components || [];
  const volumes = components.filter(c => !c.host).map(c => c.name);
  const host = components.filter(c => c.host).map(c => `${componentLabels[c.kind] || c.kind} ${c.name}`);
  const parts: string[] = [];
  if (volumes.length || !host.length) parts.push(`Stops and removes its containers, then deletes these volumes from ${item?.server_id}: ${volumes.join(', ') || 'none'}.`);
  if (host.length) parts.push(`Deletes these data services on ${item?.server_id}: ${host.join(', ')}, including the Redis data directory.`);
  parts.push('Backup records and off-site copies are kept and follow the retention policy.');
  return parts.join(' ');
}

/** retainedWhy explains why a resource is still on its server. */
export function retainedWhy(item: RetainedResource) {
  if (item.copy) return 'Copied by a move that never switched; no backup needed';
  if (item.reason === 'moved') return item.runs_on ? `Moved — now runs on ${item.runs_on_name || item.runs_on}` : 'Moved to another server';
  return item.kind === 'database' ? 'No application uses it' : 'Application deleted, data kept';
}

function retainedSince(item: RetainedResource) {
  return item.created && !item.created.startsWith('0001-') ? formatDate(item.created) : 'Before this agent recorded it';
}

function lastBackup(db: RetainedDatabase) {
  const parts: string[] = [];
  if (db.last_offsite_backup) parts.push(`off-site ${formatDate(db.last_offsite_backup.created)}`);
  if (db.last_local_backup) parts.push(`local ${formatDate(db.last_local_backup.created)}`);
  return parts.length ? parts.join(' · ') : 'no backup';
}

function RemoveDialog({ item, onClose, onRemoved }: { item?: RetainedResource; onClose: () => void; onRemoved: () => Promise<void> }) {
  const inputID = useId();
  const notify = useToast();
  const [confirm, setConfirm] = useState('');
  const [finalBackup, setFinalBackup] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => { setConfirm(''); setError(''); setBusy(false); setFinalBackup(Boolean(item?.final_backup_required)); }, [item]);
  const databases = item?.databases || [];
  const required = Boolean(item?.final_backup_required);
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!item || confirm.trim() !== item.id || busy) return;
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
      <AlertDialog.Title>Remove retained data of {item?.app_id}?</AlertDialog.Title>
      <AlertDialog.Description>{removalDescription(item)}</AlertDialog.Description>
      {databases.length > 0 && <label className="check-row"><input type="checkbox" checked={finalBackup || required} disabled={required || busy} onChange={event => setFinalBackup(event.target.checked)} /><span><strong>Take a final backup first</strong><small>{required ? 'Required: no verified backup was taken after the application stopped writing.' : 'Uploaded off-site when off-site storage is configured.'}</small></span></label>}
      <label className="field" htmlFor={inputID}><span className="field-label">Type <code>{item?.id}</code> to confirm</span><input id={inputID} autoComplete="off" spellCheck={false} value={confirm} onChange={event => setConfirm(event.target.value)} disabled={busy} required /></label>
      {error && <div className="dialog-error" role="alert"><AlertTriangle size={17} /><span>{error}</span></div>}
      <div className="dialog-actions"><AlertDialog.Cancel asChild><Button type="button" disabled={busy}>Cancel</Button></AlertDialog.Cancel><Button type="submit" variant="danger" disabled={busy || confirm.trim() !== item?.id}>{busy && <LoaderCircle className="spin" size={16} />}Remove</Button></div>
    </ActionForm>
  </AlertDialog.Content></AlertDialog.Portal></AlertDialog.Root>;
}

/**
 * RetainedResources lists what removed applications and finished moves left
 * on a server, with a guarded removal. Without serverID it lists every
 * server's resources (optionally one application's).
 */
export function RetainedResources({ serverID, appID }: { serverID?: string; appID?: string }) {
  const [items, setItems] = useState<RetainedResource[]>();
  const [error, setError] = useState('');
  const [removing, setRemoving] = useState<RetainedResource>();
  const load = async () => {
    try {
      const list = serverID
        ? await get<RetainedResource[]>(`control/servers/${encodeURIComponent(serverID)}/retained-resources`)
        : (await get<{ retained_resources?: RetainedResource[] }>('control/databases')).retained_resources || [];
      setItems(list.filter(item => !appID || item.app_id === appID)); setError('');
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
    { id: 'resource', label: 'Resource', render: row => <div className="retained-name"><strong>{row.app_id}</strong><small><code>{row.id}</code>{!serverID && ` · ${row.server_id}`}</small><small>{(row.components || []).map(c => componentLabels[c.kind] || c.kind).join(', ') || (row.kind === 'database' ? 'PostgreSQL' : 'Credentials only')}</small></div> },
    { id: 'size', label: 'Size', render: row => row.size_pending ? <span className="muted-copy">Measuring…</span> : formatBytes(row.size_bytes) },
    { id: 'since', label: 'Retained since', render: row => retainedSince(row) },
    { id: 'why', label: 'Why', render: row => retainedWhy(row) },
    { id: 'backup', label: 'Last backup', render: row => row.databases?.length ? <div className="retained-backups">{row.databases.map(db => <small key={db.id}>{db.id}: {lastBackup(db)}{db.covered ? '' : ' (final backup needed)'}</small>)}</div> : <span className="muted-copy">No database</span> },
    { id: 'actions', label: 'Actions', render: row => row.removing ? <Badge tone="warning">Removing…</Badge> : row.attached_to?.length ? <span className="muted-copy">In use by {row.attached_to.join(', ')}</span> : <div className="retained-actions">{row.error && <small className="field-error">{row.error}</small>}<Button size="sm" variant="danger" onClick={() => setRemoving(row)}><Trash2 size={15} />Remove</Button></div> },
  ];
  if (appID && !serverID && items && !items.length && !error) return null;
  const header = <SectionHeader title="Retained resources" description="Data that removed applications and finished moves left on the server. Removing it frees the disk; backups stay." actions={<Button size="sm" onClick={() => { void load(); }}><RefreshCw size={15} />Refresh</Button>} />;
  return <Card data-retained-resources={serverID || appID || 'all'}>
    {header}
    {error && <ErrorBanner>{error}</ErrorBanner>}
    {!items && !error ? <Loading label="Loading retained resources…" /> : items && <ResourceList rows={items} columns={columns} rowKey={row => `${row.server_id}/${row.id}`} empty={<EmptyState icon={<Archive />} title="Nothing retained" description="No removed application or finished move left data here." />} />}
    <RemoveDialog item={removing} onClose={() => setRemoving(undefined)} onRemoved={load} />
  </Card>;
}
