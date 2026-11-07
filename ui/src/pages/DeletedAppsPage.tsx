import * as AlertDialog from '@radix-ui/react-alert-dialog';
import { type FormEvent, useCallback, useEffect, useId, useState } from 'react';
import { AlertTriangle, ArchiveRestore, LoaderCircle, RefreshCw, Trash2, Undo2 } from 'lucide-react';
import { del, get, post } from '../lib/api';
import { formatBytes } from '../lib/format';
import { type ArchiveRow, type ArchivesResponse, type RestoreResponse, formatDay, itemLabel } from '../lib/archive';
import { useToast } from '../components/ToastProvider';
import { ActionForm, Badge, Button, Card, EmptyState, ErrorBanner, Loading, PageHeader, SectionHeader } from '../components/ui';
import { ResourceList, type Column } from '../components/ResourceList';

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

/** DeletedAppsPage lists deleted applications that can still be restored. */
export function DeletedAppsPage() {
  const [data, setData] = useState<ArchivesResponse>();
  const [error, setError] = useState('');
  const [restoring, setRestoring] = useState<ArchiveRow>();
  const [purging, setPurging] = useState<ArchiveRow>();
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
  const columns: Column<ArchiveRow>[] = [
    { id: 'app', label: 'Application', render: row => <div className="retained-name"><strong>{row.app_id}</strong><small>{row.repository}{row.branch ? ` · ${row.branch}` : ''}</small><small>{row.server_name} · {row.kind === 'native' ? `native ${row.runtime || ''}`.trim() : row.kind}</small></div> },
    { id: 'deleted', label: 'Deleted', render: row => <div className="retained-name"><span title={new Date(row.deleted_at).toLocaleString()}>{formatDay(row.deleted_at)}</span>{row.deleted_by && <small>by {row.deleted_by}</small>}{row.reason && <small>“{row.reason}”</small>}</div> },
    { id: 'left', label: 'Restorable', render: row => <div className="retained-name">{phaseBadge(row)}<small>until {formatDay(row.expires_at)}</small>{row.error && <small className="field-error">{row.error}</small>}</div> },
    { id: 'kept', label: 'Kept', render: row => <div className="retained-name"><strong>{keptSize(row)}</strong>{row.kept.filter(item => item.kind !== 'settings').map(item => <small key={`${item.kind}/${item.name}`} title={item.name}>{itemLabel(item)}: {item.detail || item.name}{item.measured ? ` · ${formatBytes(item.size_bytes)}` : ''}</small>)}</div> },
    { id: 'removed', label: 'Deleted with it', render: row => <div className="retained-name">{row.deleted.map(item => <small key={`${item.kind}/${item.name}`}>{itemLabel(item)}{item.count ? ` (${item.count})` : ''}: {item.detail || item.name}</small>)}</div> },
    { id: 'actions', label: 'Actions', render: row => <div className="row-actions nowrap-actions"><Button size="sm" onClick={() => setRestoring(row)} disabled={row.phase === 'deleting' || row.phase === 'purging'}><Undo2 size={15} />Restore</Button><Button size="sm" variant="danger" onClick={() => setPurging(row)} disabled={row.phase === 'restoring' || row.phase === 'deleting'}><Trash2 size={15} />Delete now</Button></div> },
  ];
  return <>
    <PageHeader eyebrow="Workspace" title="Deleted apps" description={`A deleted application can be restored for ${data?.retention_days || 90} days: its settings, files and databases are kept with their logins disabled. After that the server deletes it permanently, databases included.`} actions={<Button size="sm" onClick={() => { void load(); }}><RefreshCw size={15} />Refresh</Button>} />
    {error && <ErrorBanner>{error}</ErrorBanner>}
    {data?.unavailable_servers.length ? <ErrorBanner>Not answering: {data.unavailable_servers.join(', ')}. Their deleted applications show the panel's last copy.</ErrorBanner> : null}
    <Card data-deleted-apps>
      {!data && !error ? <Loading label="Loading deleted applications…" /> : data && <ResourceList rows={data.archives} columns={columns} rowKey={row => `${row.server_id}/${row.id}`} empty={<EmptyState icon={<ArchiveRestore />} title="No deleted applications" description="Deleted applications appear here while they can be restored." />} />}
    </Card>
    {data && data.purged.length > 0 && <Card>
      <SectionHeader title="Permanently deleted" description="The latest applications the servers deleted for good, on expiry or on request." />
      <ul className="plain-list">{data.purged.map(p => <li key={`${p.server_id}/${p.archive_id}`}><strong>{p.app_id}</strong> on {p.server_name} · {formatDay(p.at)} · {p.trigger === 'expiry' ? 'expired' : `deleted${p.by ? ` by ${p.by}` : ''}`}</li>)}</ul>
    </Card>}
    <RestoreDialog row={restoring} onClose={() => setRestoring(undefined)} onDone={load} />
    <PurgeDialog row={purging} onClose={() => setPurging(undefined)} onDone={load} />
  </>;
}
