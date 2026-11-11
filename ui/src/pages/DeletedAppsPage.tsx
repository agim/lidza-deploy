import * as AlertDialog from '@radix-ui/react-alert-dialog';
import { SummaryList } from '../components/SummaryList';
import { type FormEvent, useCallback, useEffect, useId, useState } from 'react';
import { AlertTriangle, ArchiveRestore, LoaderCircle, RefreshCw, Trash2, Undo2 } from 'lucide-react';
import { del, get, post } from '../lib/api';
import { formatBytes } from '../lib/format';
import { type ArchiveRow, type ArchivesResponse, type RestoreResponse, formatDay, itemLabel } from '../lib/archive';
import { useToast } from '../components/ToastProvider';
import { ActionForm, Badge, Button, Card, EmptyState, ErrorBanner, Loading, SectionHeader } from '../components/ui';
import { DangerZone, DangerZoneRow, ResourceDetailPage, ResourceIndexPage, ResourceName } from '../components/resource';
import { Cluster } from '../components/layout';
import { indexHref, resourceHref, withSticky } from '../lib/router';
import type { Column } from '../components/ResourceList';

const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);

function keptSize(row: ArchiveRow) {
  if (row.size_pending) return row.kept_bytes ? `${formatBytes(row.kept_bytes)}+ (measuring)` : 'Measuring…';
  return formatBytes(row.kept_bytes);
}

function phaseBadge(row: ArchiveRow) {
  if (row.unavailable) return <Badge tone="warning">Server unavailable</Badge>;
  if (row.phase === 'deleting') return <Badge tone="danger">Deletion unfinished</Badge>;
  if (row.phase === 'restoring') return <Badge tone="warning">Restoring</Badge>;
  if (row.phase === 'purging') return <Badge tone="danger">Deleting permanently</Badge>;
  return <Badge tone={row.days_left <= 7 ? 'warning' : 'neutral'}>{row.days_left === 1 ? '1 day left' : `${row.days_left} days left`}</Badge>;
}

function RestoreDialog({ row, onClose, onDone }: { row?: ArchiveRow; onClose: () => void; onDone: () => Promise<void> }) {
  const idField = useId();
  const domainField = useId();
  const notify = useToast();
  const [newID, setNewID] = useState('');
  const [domain, setDomain] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [result, setResult] = useState<RestoreResponse>();
  useEffect(() => { setNewID(''); setDomain(''); setError(''); setBusy(false); setResult(undefined); }, [row]);
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!row || busy) return;
    if (result) { onClose(); return; }
    setBusy(true); setError('');
    try {
      const body: Record<string, string> = {};
      if (newID.trim()) body.new_id = newID.trim();
      if (domain.trim()) body.domain = domain.trim();
      const query = `?server=${encodeURIComponent(row.server_id)}`;
      const response = await post<RestoreResponse>(`control/archives/${encodeURIComponent(row.id)}/restore${query}`, body);
      setResult(response);
      notify(`${response.result.app_id} restored${response.deploy_error ? '; the deployment did not start' : ' and deploying'}.`);
      await onDone();
    } catch (reason) { setError(message(reason)); } finally { setBusy(false); }
  };
  const skipped = Object.entries(result?.result.skipped || {});
  return <AlertDialog.Root open={Boolean(row)} onOpenChange={open => { if (!open && !busy) onClose(); }}><AlertDialog.Portal><AlertDialog.Overlay className="dialog-overlay" /><AlertDialog.Content className="dialog dialog-wide">
    <ActionForm className="dialog-form" onSubmit={submit}>
      <AlertDialog.Title>{result ? `${result.result.app_id} restored` : `Restore ${row?.app_id}?`}</AlertDialog.Title>
      {!result && <AlertDialog.Description>Recreates the application from its saved settings on {row?.server_name}, enables its database logins, attaches its files, recreates an empty cache, publishes the hostnames no other application took meanwhile and deploys {row?.commit ? `its branch (last release ${row.commit.slice(0, 12)})` : 'its branch'}. Its backups were deleted with it and start again with the next scheduled backup.</AlertDialog.Description>}
      {!result && <p className="muted-copy">Hostnames: {row?.hostnames.join(', ') || 'none'}</p>}
      {!result && row && !row.id_available && <label className="field" htmlFor={idField}><span className="field-label">New application id</span><input id={idField} value={newID} onChange={event => setNewID(event.target.value)} required={row.kind !== 'native'} disabled={busy || row.kind === 'native'} placeholder={`${row.app_id}-restored`} /><small>Another application now uses the id {row.app_id}.{row.kind === 'native' ? ' A native application is restored under its own id: delete or rename the other one first.' : ''}</small></label>}
      {!result && <label className="field" htmlFor={domainField}><span className="field-label">New primary domain (only if its domain was taken)</span><input id={domainField} value={domain} onChange={event => setDomain(event.target.value)} disabled={busy} placeholder={row?.hostnames[0]} /></label>}
      {result && <div className="restore-result" data-restore-result>
        <p>Published: {result.result.hostnames.join(', ')}</p>
        {skipped.length > 0 && <div className="alert alert-warning" role="status"><AlertTriangle size={17} /><span>Not published, another application uses them: {skipped.map(([host, owner]) => `${host} (${owner})`).join(', ')}</span></div>}
        {result.webhook && <p className="muted-copy">{result.webhook}</p>}
        {result.deploy_error && <div className="dialog-error" role="alert"><AlertTriangle size={17} /><span>Deployment: {result.deploy_error}</span></div>}
      </div>}
      {error && <div className="dialog-error" role="alert"><AlertTriangle size={17} /><span>{error}</span></div>}
      <div className="dialog-actions">{!result && <AlertDialog.Cancel asChild><Button type="button" disabled={busy}>Cancel</Button></AlertDialog.Cancel>}<Button type="submit" variant="primary" disabled={busy}>{busy && <LoaderCircle className="spin" size={16} />}{result ? 'Done' : 'Restore application'}</Button></div>
    </ActionForm>
  </AlertDialog.Content></AlertDialog.Portal></AlertDialog.Root>;
}

function PurgeDialog({ row, onClose, onDone }: { row?: ArchiveRow; onClose: () => void; onDone: () => Promise<void> }) {
  const inputID = useId();
  const notify = useToast();
  const [confirm, setConfirm] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => { setConfirm(''); setError(''); setBusy(false); }, [row]);
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!row || busy || confirm.trim() !== row.app_id) return;
    setBusy(true); setError('');
    try {
      await del(`control/archives/${encodeURIComponent(row.id)}?server=${encodeURIComponent(row.server_id)}`, { confirm: row.app_id });
      notify(`${row.app_id} deleted permanently.`);
      onClose();
      await onDone();
    } catch (reason) { setError(message(reason)); } finally { setBusy(false); }
  };
  return <AlertDialog.Root open={Boolean(row)} onOpenChange={open => { if (!open && !busy) onClose(); }}><AlertDialog.Portal><AlertDialog.Overlay className="dialog-overlay" /><AlertDialog.Content className="dialog">
    <ActionForm className="dialog-form" onSubmit={submit}>
      <AlertDialog.Title>Delete {row?.app_id} permanently?</AlertDialog.Title>
      <AlertDialog.Description>Drops its databases and their roles, deletes its volumes and files ({row ? keptSize(row) : ''}) and the objects of its S3 application storage, and forgets its settings. This cannot be undone; there are no backups left to restore from.</AlertDialog.Description>
      <label className="field" htmlFor={inputID}><span className="field-label">Type <code>{row?.app_id}</code> to delete it permanently</span><input id={inputID} autoComplete="off" spellCheck={false} value={confirm} onChange={event => setConfirm(event.target.value)} disabled={busy} required /></label>
      {error && <div className="dialog-error" role="alert"><AlertTriangle size={17} /><span>{error}</span></div>}
      <div className="dialog-actions"><AlertDialog.Cancel asChild><Button type="button" disabled={busy}>Cancel</Button></AlertDialog.Cancel><Button type="submit" variant="danger" disabled={busy || confirm.trim() !== row?.app_id}>{busy && <LoaderCircle className="spin" size={16} />}Delete permanently</Button></div>
    </ActionForm>
  </AlertDialog.Content></AlertDialog.Portal></AlertDialog.Root>;
}

/** useArchives loads the deleted applications and polls while one is measured, restored or purged. */
function useArchives() {
  const [data, setData] = useState<ArchivesResponse>();
  const [error, setError] = useState('');
  const load = useCallback(async () => {
    try { setData(await get<ArchivesResponse>('control/archives')); setError(''); } catch (reason) { setError(message(reason)); }
  }, []);
  useEffect(() => { void load(); }, [load]);
  const pending = Boolean(data?.archives.some(row => row.size_pending || row.phase === 'restoring' || row.phase === 'purging'));
  useEffect(() => {
    if (!pending) return;
    const timer = setInterval(() => { void load(); }, 5000);
    return () => clearInterval(timer);
  }, [pending, load]);
  return { data, error, load };
}

function kindLabel(row: ArchiveRow) { return row.kind === 'native' ? `native ${row.runtime || ''}`.trim() : row.kind; }

/** DeletedAppsPage: the index of deleted applications that can still be restored. */
export function DeletedAppsPage() {
  const { data, error, load } = useArchives();
  const columns: Column<ArchiveRow>[] = [
    { id: 'app', label: 'Application', render: row => <ResourceName href={withSticky(resourceHref('deleted-apps', row.id))} name={row.app_id} detail={`${row.repository || ''}${row.branch ? ` · ${row.branch}` : ''}`} /> },
    { id: 'left', label: 'Status', render: row => <div className="retained-name">{phaseBadge(row)}<small>until {formatDay(row.expires_at)}</small>{row.error && <small className="field-error">{row.error}</small>}</div> },
    { id: 'server', label: 'Server', render: row => <div className="retained-name"><span>{row.server_name}</span><small>{kindLabel(row)}</small></div> },
    { id: 'kept', label: 'Kept', render: row => keptSize(row) },
    { id: 'deleted', label: 'Deleted', render: row => <div className="retained-name"><span title={new Date(row.deleted_at).toLocaleString()}>{formatDay(row.deleted_at)}</span>{row.deleted_by && <small>by {row.deleted_by}</small>}</div> },
  ];
  return <ResourceIndexPage resource="deleted-apps" title="Deleted apps" description={`A deleted application can be restored for ${data?.retention_days || 90} days, with its settings, files and databases.`} count={data?.archives.length}
    actions={<Button onClick={() => { void load(); }}><RefreshCw size={15} />Refresh</Button>} loading={!data && !error}
    notices={<>{error && <ErrorBanner>{error}</ErrorBanner>}{data?.unavailable_servers.length ? <ErrorBanner>Not answering: {data.unavailable_servers.join(', ')}. Their deleted applications show the panel's last copy.</ErrorBanner> : null}</>}
    rows={data?.archives || []} columns={columns} rowKey={row => `${row.server_id}/${row.id}`}
    empty={<EmptyState icon={<ArchiveRestore />} title="No deleted applications" description="Deleted applications appear here while they can be restored." />}>
    {data && data.purged.length > 0 && <Card>
      <SectionHeader title="Permanently deleted" description="The latest applications the servers deleted for good, on expiry or on request." />
      <ul className="plain-list">{data.purged.map(p => <li key={`${p.server_id}/${p.archive_id}`}><strong>{p.app_id}</strong> on {p.server_name} · {formatDay(p.at)} · {p.trigger === 'expiry' ? 'expired' : `deleted${p.by ? ` by ${p.by}` : ''}`}</li>)}</ul>
    </Card>}
  </ResourceIndexPage>;
}

/** DeletedAppDetailPage: one deleted application; Overview and Settings (restore, Danger zone). */
export function DeletedAppDetailPage({ id, tab }: { id: string; tab: string }) {
  const { data, error, load } = useArchives();
  const row = data?.archives.find(item => item.id === id);
  const [restoring, setRestoring] = useState<ArchiveRow>();
  const [purging, setPurging] = useState<ArchiveRow>();
  const restoreBlocked = !row || row.phase === 'deleting' || row.phase === 'purging';
  const done = async () => { await load(); };
  return <ResourceDetailPage resource="deleted-apps" id={id} tab={tab} title={row?.app_id || id}
    status={row && phaseBadge(row)}
    facts={row ? [row.server_name, kindLabel(row), `deleted ${formatDay(row.deleted_at)}`] : undefined}
    actions={row ? <Button variant="primary" onClick={() => setRestoring(row)} disabled={restoreBlocked}><Undo2 size={15} />Restore</Button> : undefined}
    notices={error ? <ErrorBanner>{error}</ErrorBanner> : undefined}
    missing={!data && !error ? <Loading label="Loading deleted application…" /> : !row ? <EmptyState icon={<ArchiveRestore />} title="Not in deleted apps" description="It was restored or deleted permanently." action={<a className="button button-secondary button-md" href={withSticky(indexHref('deleted-apps'))}>Open deleted apps</a>} /> : undefined}>
    {row && tab === 'overview' && <div className="settings-stack">
      <Card><SectionHeader title="Summary" /><SummaryList items={[
        { label: 'Repository', value: `${row.repository || '—'}${row.branch ? ` · ${row.branch}` : ''}` },
        { label: 'Server', value: <a href={withSticky(resourceHref('servers', row.server_id))}>{row.server_name}</a> },
        { label: 'Hostnames', value: row.hostnames.join(', ') || 'None' },
        { label: 'Deleted', value: `${formatDay(row.deleted_at)}${row.deleted_by ? ` by ${row.deleted_by}` : ''}` },
        row.reason && { label: 'Reason', value: row.reason },
        { label: 'Restorable until', value: formatDay(row.expires_at) },
        { label: 'Kept', value: keptSize(row) },
        row.error && { label: 'Problem', value: row.error },
      ]} /></Card>
      <Card><SectionHeader title="Kept for restoring" /><ul className="plain-list">{row.kept.filter(item => item.kind !== 'settings').map(item => <li key={`${item.kind}/${item.name}`} title={item.name}>{itemLabel(item)}: {item.detail || item.name}{item.measured ? ` · ${formatBytes(item.size_bytes)}` : ''}</li>)}</ul></Card>
      <Card><SectionHeader title="Deleted with it" /><ul className="plain-list">{row.deleted.map(item => <li key={`${item.kind}/${item.name}`}>{itemLabel(item)}{item.count ? ` (${item.count})` : ''}: {item.detail || item.name}</li>)}</ul></Card>
    </div>}
    {row && tab === 'settings' && <div className="settings-stack">
      <Card className="form-card"><SectionHeader title="Restore" description="Brings the application back with its settings, files and databases, under its own or a new name." /><Cluster><Button variant="primary" onClick={() => setRestoring(row)} disabled={restoreBlocked}><Undo2 size={15} />Restore application</Button></Cluster></Card>
      <DangerZone id={row.id} description="Deleting now cannot be undone."><DangerZoneRow title="Delete permanently" description="The server deletes the kept files and databases now instead of on expiry." action={<Button variant="danger" onClick={() => setPurging(row)} disabled={row.phase === 'restoring' || row.phase === 'deleting'}><Trash2 size={15} />Delete now</Button>} /></DangerZone>
    </div>}
    <RestoreDialog row={restoring} onClose={() => setRestoring(undefined)} onDone={done} />
    <PurgeDialog row={purging} onClose={() => setPurging(undefined)} onDone={done} />
  </ResourceDetailPage>;
}
