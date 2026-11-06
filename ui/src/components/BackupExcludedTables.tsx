import { useCallback, useEffect, useMemo, useState } from 'react';
import { get, patch } from '../lib/api';
import { formatBytes } from '../lib/format';
import { useToast } from './ToastProvider';
import { Button, Card, ErrorBanner, Loading, SearchField, SectionHeader } from './ui';

/**
 * Tables whose data a database's backups skip. The rows of disposable
 * tables (logs, caches, event streams) can stay out of backups; their schema
 * is kept, so a restore recreates them empty. The operator chooses from the
 * database's table list; the agent refuses tables that do not exist and a
 * list that would skip every table.
 */
export interface DatabaseTable { name: string; rows: number; bytes: number; excludable: boolean; excluded: boolean }
export interface DatabaseTables { tables: DatabaseTable[]; total: number; truncated?: boolean; excluded: string[] }

const rowText = (rows: number) => rows >= 1e6 ? `about ${(rows / 1e6).toFixed(1)}M rows` : rows >= 1e4 ? `about ${Math.round(rows / 1000)}k rows` : `about ${rows.toLocaleString()} rows`;

/** Estimated rows and size; an unknown row estimate is left out. */
export function tableSummary(table?: Pick<DatabaseTable, 'rows' | 'bytes'>) {
  if (!table) return 'not in the table list';
  const parts = [table.rows >= 0 ? rowText(table.rows) : '', table.bytes >= 0 ? formatBytes(table.bytes) : ''].filter(Boolean);
  return parts.length ? parts.join(', ') : 'not measured';
}

/** "data of 2 tables skipped" for a backup that left table data out. */
export function skippedTablesLabel(tables?: string[]) {
  const n = tables?.length || 0;
  return n ? `data of ${n} ${n === 1 ? 'table' : 'tables'} skipped` : '';
}

export function BackupExcludedTablesCard({ serverID, databaseID, excluded, disabled, onSaved }: {
  serverID: string; databaseID: string; excluded: string[]; disabled?: boolean; onSaved?: () => Promise<void> | void;
}) {
  const [listing, setListing] = useState<DatabaseTables>();
  const [error, setError] = useState('');
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState<string[]>(excluded);
  const [query, setQuery] = useState('');
  const [saving, setSaving] = useState(false);
  const notify = useToast();
  const base = `control/servers/${encodeURIComponent(serverID)}/databases/${encodeURIComponent(databaseID)}`;
  const load = useCallback(async () => {
    try { setListing(await get<DatabaseTables>(`${base}/tables`)); setError(''); }
    catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); }
  }, [base]);
  useEffect(() => { void load(); }, [load]);
  const byName = useMemo(() => new Map((listing?.tables || []).map(table => [table.name, table])), [listing]);
  const total = listing?.total ?? 0;
  const skipsEverything = listing !== undefined && draft.length > 0 && draft.length >= total;
  const visible = useMemo(() => {
    const needle = query.trim().toLowerCase();
    return (listing?.tables || []).filter(table => table.excludable && (!needle || table.name.includes(needle)));
  }, [listing, query]);
  const skippedBytes = excluded.reduce((sum, name) => sum + Math.max(byName.get(name)?.bytes ?? 0, 0), 0);

  const toggle = (name: string, on: boolean) => setDraft(current => on ? [...current.filter(item => item !== name), name] : current.filter(item => item !== name));
  const save = async () => {
    setSaving(true);
    try {
      await patch(`${base}/backups/excluded-tables`, { tables: [...draft].sort() });
      notify(draft.length ? `Backups of ${databaseID} skip the data of ${draft.length} ${draft.length === 1 ? 'table' : 'tables'}.` : `Backups of ${databaseID} contain every table again.`);
      setEditing(false);
      await onSaved?.();
      await load();
    } catch (reason) {
      notify(reason instanceof Error ? reason.message : String(reason), true);
    } finally {
      setSaving(false);
    }
  };

  return <Card className="form-card excluded-tables-card" aria-label={`Backups skip data of these tables for ${databaseID}`}>
    <SectionHeader title="Backups skip data of these tables" description="Keep the rows of disposable tables (logs, caches, event streams) out of this database's backups. Their schema stays in every backup." actions={!editing && <Button type="button" size="sm" disabled={disabled || !listing} onClick={() => { setDraft(excluded); setQuery(''); setEditing(true); }}>Choose tables</Button>} />
    {error && <ErrorBanner>{error}</ErrorBanner>}
    {excluded.length > 0 && <p className="excluded-tables-warning" role="note">Restores from these backups will not contain this data: the tables are recreated empty.</p>}
    {!editing && (excluded.length === 0
      ? <p className="muted-copy">Backups contain the data of every table.</p>
      : <>
        <ul className="excluded-tables-list">{excluded.map(name => <li key={name}><code>{name}</code><span className="muted-copy">{listing ? tableSummary(byName.get(name)) : ''}</span></li>)}</ul>
        {listing && skippedBytes > 0 && <p className="muted-copy">About {formatBytes(skippedBytes)} stays out of each backup.</p>}
      </>)}
    {editing && (!listing ? <Loading label="Loading tables…" /> : <div className="excluded-tables-editor">
      <SearchField value={query} onChange={event => setQuery(event.target.value)} placeholder="Filter tables…" aria-label="Filter tables" />
      {listing.truncated && <p className="muted-copy">Showing the {listing.tables.length} largest of {listing.total} tables.</p>}
      <ul className="excluded-tables-options">{visible.map(table => <li key={table.name}><label className="check-row"><input type="checkbox" checked={draft.includes(table.name)} onChange={event => toggle(table.name, event.target.checked)} aria-label={`Skip the data of ${table.name}`} /><span><strong>{table.name}</strong><small>{tableSummary(table)}</small></span></label></li>)}</ul>
      {visible.length === 0 && <p className="muted-copy">No table matches.</p>}
      {draft.length > 0 && <p className="excluded-tables-warning" role="note">Restores will not contain the data of {draft.length} {draft.length === 1 ? 'table' : 'tables'}; the tables are recreated empty.</p>}
      {skipsEverything && <ErrorBanner>Backups must keep the data of at least one table.</ErrorBanner>}
      <div className="row-actions">
        <Button type="button" variant="primary" size="sm" disabled={saving || skipsEverything} onClick={() => { void save(); }}>{saving ? 'Saving…' : 'Save skipped tables'}</Button>
        <Button type="button" size="sm" disabled={saving} onClick={() => setEditing(false)}>Cancel</Button>
      </div>
    </div>)}
  </Card>;
}
