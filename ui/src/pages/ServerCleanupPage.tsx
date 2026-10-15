import { useEffect, useState } from 'react';
import { Clock, Database, HardDrive, RefreshCw, Save, ShieldCheck, Trash2 } from 'lucide-react';
import { get, patch, post } from '../lib/api';
import { formatDate } from '../lib/format';
import { useWorkspace } from '../state/WorkspaceContext';
import { useToast } from '../components/ToastProvider';
import { ActionForm, Badge, Button, Card, ConfirmAction, CopyButton, ErrorBanner, Field, Loading, Notice, PageHeader, SectionHeader } from '../components/ui';
import { Input } from '../components/primitives/input';
import { Switch } from '../components/primitives/switch';

interface Policy { enabled: boolean; interval_hours: number; build_cache: boolean; logs: boolean; older_than_days: number }
interface Status { policy: Policy; running: boolean; next_run?: string; next_backup_cleanup?: string; last_run?: string; messages?: string[]; error?: string }
const date = (value?: string) => value && !value.startsWith('0001-') ? formatDate(value) : 'On the next agent maintenance check';

export function ServerCleanupPage({ serverID }: { serverID: string }) {
 const { servers } = useWorkspace();
 const notify = useToast();
 const [status, setStatus] = useState<Status>();
 const [policy, setPolicy] = useState<Policy>();
 const [error, setError] = useState('');
 const [selected, setSelected] = useState({ backups: true, build_cache: true, logs: true });
 useEffect(() => {
  let active = true, timer: ReturnType<typeof setTimeout>;
  setStatus(undefined);setPolicy(undefined);setError('');
  const load = async (initial = false) => {
   try { const value = await get<Status>(`control/servers/${encodeURIComponent(serverID)}/maintenance`); if (!active) return; setStatus(value); if (initial) setPolicy(value.policy); setError(''); timer = setTimeout(() => { void load(); }, value.running ? 2000 : 15000); }
   catch (reason) { if (active) { setError(reason instanceof Error ? reason.message : String(reason));timer = setTimeout(() => { void load(initial); }, 15000); } }
  };
  void load(true);return () => { active = false;clearTimeout(timer); };
 }, [serverID]);
 const refresh = async () => { try { setStatus(await get<Status>(`control/servers/${encodeURIComponent(serverID)}/maintenance`));setError(''); } catch (reason) { setError(String(reason)); } };
 useEffect(() => { if (!status?.running) return;const timer = setInterval(() => { void refresh(); }, 2000);return () => clearInterval(timer); }, [status?.running, serverID]);
 const save = async () => { if (!policy) return;try { const value = await patch<Status>(`control/servers/${encodeURIComponent(serverID)}/maintenance`, policy);setStatus(value);setPolicy(value.policy);setError('');notify('Server cleanup schedule saved.'); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } };
 const run = async () => { const value = await post<Status>(`control/servers/${encodeURIComponent(serverID)}/maintenance/run`, selected);setStatus(value);notify('Cleanup started. Results appear here when it finishes.'); };
 const change = (value: Partial<Policy>) => setPolicy(current => current ? { ...current, ...value } : current);
 const title = `${servers.find(server => server.id === serverID)?.name || serverID} cleanup`;
 return <><PageHeader eyebrow="Server maintenance" title={title} description="Reclaim disk space with bounded cleanup and automatic retention." back="#servers" actions={<Button onClick={refresh}><RefreshCw size={16} />Refresh</Button>} />{error && <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner>}{!status || !policy ? !error && <Loading label="Loading cleanup policy…" /> : <div className="settings-stack">
  <Notice><ShieldCheck size={18} /><span>Running containers, images, volumes, databases, application files and Valkey data are preserved. Active Docker log files are never truncated. S3 objects are unchanged.</span></Notice>
  <Card className="form-card"><SectionHeader title="Clean up now" description={`Build cache and archived system journals must be at least ${status.policy.older_than_days} days old. The job waits for active deployments and database operations.`} actions={<Badge tone={status.running ? 'warning' : 'neutral'}>{status.running ? 'Cleanup running' : 'Ready'}</Badge>} />
   <div className="maintenance-options">{([{ key: 'backups', title: 'Older local backups', description: 'Keep only the newest usable dump per database, including retained databases.', icon: Database }, { key: 'build_cache', title: 'Unused Docker build cache', description: 'Remove old unused build layers. Application caches and stored data are untouched.', icon: HardDrive }, { key: 'logs', title: 'Archived system journal logs', description: 'Rotate and vacuum journald archives using the selected age threshold.', icon: Trash2 }] as const).map(item => <label className="check-row" key={item.key}><Input type="checkbox" checked={selected[item.key]} disabled={status.running} onChange={event => setSelected(current => ({ ...current, [item.key]: event.target.checked }))} /><item.icon size={18} /><span><strong>{item.title}</strong><small>{item.description}</small></span></label>)}</div>
   <div className="maintenance-actions"><ConfirmAction title="Clean selected server data?" description={`Remove the selected obsolete local data from ${servers.find(server => server.id === serverID)?.name || serverID}. The newest usable backup for every database and all S3 copies remain available.`} confirmLabel="Start cleanup" destructive onConfirm={run}><Button variant="danger" disabled={status.running || !Object.values(selected).some(Boolean)}><Trash2 size={16} />Clean selected now</Button></ConfirmAction></div>
  </Card>
  <Card className="form-card"><SectionHeader title="Automatic cleanup" description="This schedule runs on the hosting agent, even while the control panel is offline." /><ActionForm onSubmit={save}>
   <label className="check-row switch-row"><Switch aria-label="Automatically clean caches and logs" checked={policy.enabled} onCheckedChange={enabled => change({ enabled })} /><span><strong>Automatically clean caches and logs</strong><small>Disabling this does not disable hourly latest-only backup retention.</small></span></label>
   <div className="form-grid two"><Field label="Run every"><select aria-label="Run every" value={policy.interval_hours} onChange={event => change({ interval_hours: Number(event.target.value) })}>{[[1, 'Hour'], [6, 'Six hours'], [24, 'Day'], [168, 'Week']].map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></Field><Field label="Minimum age (days)" hint="Applies to unused build cache and archived journals."><Input aria-label="Minimum age (days)" type="number" min={1} max={365} required value={policy.older_than_days} onChange={event => change({ older_than_days: Number(event.target.value) })} /></Field></div>
   <label className="check-row"><Input type="checkbox" checked={policy.build_cache} onChange={event => change({ build_cache: event.target.checked })} /><span>Clean unused Docker build cache</span></label><label className="check-row"><Input type="checkbox" checked={policy.logs} onChange={event => change({ logs: event.target.checked })} /><span>Clean archived system journals</span></label>
   <div className="maintenance-actions"><Button type="submit" variant="primary"><Save size={16} />Save cleanup schedule</Button></div>
  </ActionForm><p className="muted-copy maintenance-next"><Clock size={15} />Next cache/log cleanup: {policy.enabled ? date(status.next_run) : 'Disabled'}</p></Card>
  <Card><SectionHeader title="Hourly local backup retention" description="Always enabled: one newest usable local backup per database. S3 history follows the bucket lifecycle policy." /><p className="muted-copy">Next backup cleanup: {date(status.next_backup_cleanup)}</p></Card>
  <Card><SectionHeader title="Last cleanup" description={status.last_run && !status.last_run.startsWith('0001-') ? date(status.last_run) : 'No cleanup has completed yet.'} actions={<CopyButton value={[...(status.messages || []), status.error || ''].filter(Boolean).join('\n')} label="Copy cleanup report" />} />{status.error && <ErrorBanner>{status.error}</ErrorBanner>}{status.running && <Loading label="Cleanup is running on the server…" />}<pre className="maintenance-report">{status.messages?.join('\n\n') || 'Cleanup results will appear here.'}</pre></Card>
 </div>}</>;
}
