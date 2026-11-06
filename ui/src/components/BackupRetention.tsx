import { type FormEvent, useEffect, useState } from 'react';
import { Eraser, ListChecks } from 'lucide-react';
import { get, patch, put, post } from '../lib/api';
import { formatBytes, formatDate } from '../lib/format';
import { useToast } from './ToastProvider';
import { Badge, Button, Card, ConfirmAction, EmptyState, ErrorBanner, Field, Loading, SectionHeader } from './ui';
import { ResourceList, type Column } from './ResourceList';

export interface RetentionPolicy { daily: number; weekly: number; monthly: number }
export interface RetentionSettings extends RetentionPolicy { orphan_grace_days: number; automatic: boolean }
interface RetentionEntry { key: string; size: number; created: string; location?: string; reason: string }
export interface DatabaseRetentionPlan {
  database: string; status: 'retained' | 'protected' | 'orphaned' | 'unknown'; policy?: RetentionPolicy; override?: boolean;
  removed?: string; delete_after?: string; note?: string; keep: RetentionEntry[]; delete: RetentionEntry[]; keep_bytes: number; delete_bytes: number;
}
export interface RetentionPlan {
  databases: DatabaseRetentionPlan[]; keep_objects: number; keep_bytes: number; delete_objects: number; delete_bytes: number; fingerprint: string;
  generated?: string; bucket?: string; prefix?: string; unavailable_servers?: string[]; ignored_objects?: number;
}
interface RetentionResponse { settings?: RetentionSettings; reviewed?: boolean; defaults?: RetentionSettings }

export const defaultRetention: RetentionSettings = { daily: 7, weekly: 4, monthly: 0, orphan_grace_days: 30, automatic: true };

function message(reason: unknown) { return reason instanceof Error ? reason.message : String(reason); }

export function policyLabel(policy?: RetentionPolicy) {
  const p = policy || defaultRetention;
  return `${p.daily} daily · ${p.weekly} weekly · ${p.monthly} monthly`;
}

function count(fields: FormData, name: string) { return Number(fields.get(name) || 0); }

/** Load the workspace retention; mocked or older panels fall back to the defaults. */
function useWorkspaceRetention() {
  const [state, setState] = useState<{ settings: RetentionSettings; reviewed: boolean; loaded: boolean }>({ settings: defaultRetention, reviewed: false, loaded: false });
  const load = async () => {
    try {
      const value = await get<RetentionResponse>('control/storage/retention');
      setState({ settings: value?.settings && typeof value.settings.daily === 'number' ? value.settings : defaultRetention, reviewed: Boolean(value?.reviewed), loaded: true });
    } catch {
      setState(current => ({ ...current, loaded: true }));
    }
  };
  useEffect(() => { void load(); }, []);
  return { ...state, reload: load };
}

function PolicyFields({ policy, disabled = false }: { policy: RetentionPolicy; disabled?: boolean }) {
  return <div className="form-grid three">
    <Field label="Daily backups" hint="Newest backup of each recent day."><input name="daily" type="number" min={0} max={365} required disabled={disabled} defaultValue={policy.daily} autoComplete="off" /></Field>
    <Field label="Weekly backups" hint="Newest backup of each recent ISO week."><input name="weekly" type="number" min={0} max={260} required disabled={disabled} defaultValue={policy.weekly} autoComplete="off" /></Field>
    <Field label="Monthly backups" hint="Newest backup of each recent month."><input name="monthly" type="number" min={0} max={120} required disabled={disabled} defaultValue={policy.monthly} autoComplete="off" /></Field>
  </div>;
}

const statusTone: Record<DatabaseRetentionPlan['status'], 'success' | 'warning' | 'neutral' | 'accent'> = { retained: 'success', protected: 'accent', orphaned: 'warning', unknown: 'neutral' };

function planNote(row: DatabaseRetentionPlan) {
  if (row.status === 'orphaned' && row.removed) return `Orphaned (removed ${formatDate(row.removed)}); ${row.delete.length ? 'grace period ended' : `kept until ${formatDate(row.delete_after)}`}`;
  if (row.status === 'retained') return `${policyLabel(row.policy)}${row.override ? ' (database override)' : ''}${row.note ? ` · ${row.note}` : ''}`;
  return row.note || '';
}

/** RetentionPlanView lists what a reviewed cleanup keeps and deletes. */
export function RetentionPlanView({ plan, onApply }: { plan: RetentionPlan; onApply: () => Promise<void> }) {
  const columns: Column<DatabaseRetentionPlan>[] = [
    { id: 'database', label: 'Database', render: row => <div><strong>{row.database}</strong> <Badge tone={statusTone[row.status]}>{row.status}</Badge><small className="field-hint">{planNote(row)}</small></div> },
    { id: 'kept', label: 'Kept', render: row => `${row.keep.length} · ${formatBytes(row.keep_bytes)}` },
    { id: 'delete', label: 'To delete', render: row => row.delete.length ? <details className="retention-deletions"><summary>{row.delete.length} · {formatBytes(row.delete_bytes)}</summary><ul>{row.delete.map(entry => <li key={`${entry.location || ''}:${entry.key}`}><code className="object-key">{entry.location ? `[${entry.location}] ` : ''}{entry.key}</code> {formatBytes(entry.size)} · {formatDate(entry.created)}</li>)}</ul></details> : 'Nothing' },
  ];
  const unavailable = plan.unavailable_servers || [];
  return <div className="retention-plan">
    <div className="database-policy">
      <span>To delete <strong>{plan.delete_objects} objects · {formatBytes(plan.delete_bytes)} reclaimed</strong></span>
      <span>Kept <strong>{plan.keep_objects} objects · {formatBytes(plan.keep_bytes)}</strong></span>
    </div>
    {unavailable.length > 0 && <div className="move-warnings"><strong>Some servers did not answer: {unavailable.join(', ')}.</strong> Folders no answering server owns are kept until every server can be checked.</div>}
    <ResourceList rows={plan.databases} columns={columns} rowKey={row => row.database} empty={<EmptyState icon={<ListChecks />} title="No database backups found" description="The bucket holds no database backup objects." />} />
    <ConfirmAction title={`Delete ${plan.delete_objects} off-site backups?`} description={<>This permanently deletes <strong>{plan.delete_objects} objects ({formatBytes(plan.delete_bytes)})</strong> exactly as reviewed. Pre-deployment copies and the newest backup of every database stay. If anything changed since this review, nothing is deleted and you review again.</>} confirmLabel="Apply cleanup" destructive onConfirm={onApply}>
      <Button variant="danger" disabled={!plan.delete_objects}><Eraser size={16} />Apply cleanup</Button>
    </ConfirmAction>
  </div>;
}

/** BackupRetentionCard edits the workspace retention and reviews a cleanup of the whole bucket. */
export function BackupRetentionCard() {
  const workspace = useWorkspaceRetention();
  const notify = useToast();
  const [saving, setSaving] = useState(false);
  const [plan, setPlan] = useState<RetentionPlan>();
  const [reviewing, setReviewing] = useState(false);
  const [error, setError] = useState('');
  const settings = workspace.settings;
  const save = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const fields = new FormData(event.currentTarget);
    const next: RetentionSettings = { daily: count(fields, 'daily'), weekly: count(fields, 'weekly'), monthly: count(fields, 'monthly'), orphan_grace_days: count(fields, 'orphan_grace_days'), automatic: fields.get('automatic') === 'on' };
    setSaving(true);
    try {
      const result = await put<{ unsynced_servers?: string[] }>('control/storage/retention', next);
      await workspace.reload();
      const unsynced = result?.unsynced_servers || [];
      notify(unsynced.length ? `Retention saved; ${unsynced.join(', ')} will receive it with the next backup.` : 'Backup retention saved.');
    } catch (reason) {
      notify(message(reason), true);
    } finally {
      setSaving(false);
    }
  };
  const review = async () => {
    setReviewing(true);
    try { setPlan(await get<RetentionPlan>('control/storage/retention/plan')); setError(''); } catch (reason) { setError(message(reason)); } finally { setReviewing(false); }
  };
  const apply = async () => {
    if (!plan) return;
    const result = await post<{ deleted: number; deleted_bytes: number; failed?: unknown[] }>('control/storage/retention/apply', { fingerprint: plan.fingerprint });
    notify(result.failed?.length ? `Deleted ${result.deleted} objects; ${result.failed.length} could not be deleted. Check the bucket's delete permission.` : `Deleted ${result.deleted} objects (${formatBytes(result.deleted_bytes)}).`, Boolean(result.failed?.length));
    await review();
  };
  return <Card className="form-card">
    <SectionHeader title="Off-site retention" description="How many backups of each database stay in the bucket. Local disk always keeps only the newest backup, and each database keeps one pre-deployment copy." />
    {!workspace.loaded ? <Loading /> : <form key={JSON.stringify(settings)} onSubmit={save}>
      {!workspace.reviewed && <div className="move-warnings"><strong>Retention has not been saved yet.</strong> Nothing is deleted until you save it; review the cleanup first to see what these defaults remove.</div>}
      <PolicyFields policy={settings} />
      <div className="form-grid two">
        <Field label="Removed database grace (days)" hint="Backups of a database no server has any more stay this long, then are deleted."><input name="orphan_grace_days" type="number" min={1} max={365} required defaultValue={settings.orphan_grace_days} autoComplete="off" /></Field>
      </div>
      <label className="check-row"><input type="checkbox" name="automatic" defaultChecked={settings.automatic} />Clean up automatically after each upload and every six hours</label>
      <div className="row-actions">
        <Button type="submit" variant="primary" disabled={saving}>Save retention</Button>
        <Button type="button" onClick={review} pending={reviewing}><ListChecks size={16} />Review cleanup</Button>
      </div>
    </form>}
    {error && <ErrorBanner>{error}</ErrorBanner>}
    {plan && <RetentionPlanView plan={plan} onApply={apply} />}
  </Card>;
}

/** DatabaseRetentionCard sets or clears one database's off-site retention override. */
export function DatabaseRetentionCard({ serverID, databaseID, override, disabled = false, onSaved }: { serverID: string; databaseID: string; override?: RetentionPolicy | null; disabled?: boolean; onSaved: () => Promise<void> }) {
  const workspace = useWorkspaceRetention();
  const notify = useToast();
  const [own, setOwn] = useState(Boolean(override));
  const [saving, setSaving] = useState(false);
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const fields = new FormData(event.currentTarget);
    const next = own ? { daily: count(fields, 'daily'), weekly: count(fields, 'weekly'), monthly: count(fields, 'monthly') } : null;
    setSaving(true);
    try {
      await patch(`control/servers/${encodeURIComponent(serverID)}/databases/${encodeURIComponent(databaseID)}/backups/retention`, next);
      await onSaved();
      notify(next ? 'Retention override saved.' : 'This database now follows the workspace retention.');
    } catch (reason) {
      notify(message(reason), true);
    } finally {
      setSaving(false);
    }
  };
  return <Card className="form-card">
    <SectionHeader title="Off-site retention" description="Which scheduled backups of this database stay in the bucket. The pre-deployment copy and the newest backup always stay." />
    <form onSubmit={submit}>
      <label className="check-row"><input type="checkbox" checked={!own} onChange={event => setOwn(!event.target.checked)} />Use the workspace default ({policyLabel(workspace.settings)})</label>
      {own && <PolicyFields policy={override || workspace.settings} />}
      <Button type="submit" variant="primary" disabled={saving || disabled}>Save retention</Button>
    </form>
  </Card>;
}
