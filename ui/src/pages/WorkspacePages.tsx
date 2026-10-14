import { Activity, BookOpen, CheckCircle2, ClipboardList, KeyRound, Plus, RefreshCw, Search, Settings2, ShieldCheck, Smartphone, Trash2, UploadCloud, Users } from 'lucide-react';
import { type FormEvent, useEffect, useState } from 'react';
import { del, get, patch, post, put } from '../lib/api';
import { formatDate } from '../lib/format';
import { useWorkspace } from '../state/WorkspaceContext';
import { useToast } from '../components/ToastProvider';
import { Badge, Button, Card, EmptyState, ErrorBanner, Field, Loading, PageHeader, SearchField, SectionHeader } from '../components/ui';

export function DefaultsPage() {
  const [values, setValues] = useState<Record<string, string>>({}), [error, setError] = useState(''), [busy, setBusy] = useState(false); const notify = useToast();
  useEffect(() => { get<{ values?: Record<string, string> }>('control/application-defaults').then(value => setValues(value.values || {})).catch(reason => setError(reason instanceof Error ? reason.message : String(reason))); }, []);
  const save = async () => { setBusy(true); try { await put('control/application-defaults', { values }); notify('Application defaults saved. They apply only on an app’s first deployment.'); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setBusy(false); } };
  return <><PageHeader eyebrow="Workspace defaults" title="Application defaults" description="Shared variables copied once, before each new application’s first deployment." actions={<Button variant="primary" onClick={save} disabled={busy}>Save defaults</Button>} />{error && <ErrorBanner>{error}</ErrorBanner>}<Card className="form-card"><SectionHeader title="Environment variables" description="App-specific values take precedence. Existing applications never change when these defaults change." actions={<Button onClick={() => { let name = 'VARIABLE'; let index = 2; while (Object.hasOwn(values, name)) name = `VARIABLE_${index++}`; setValues(current => ({ ...current, [name]: '' })); }}><Plus size={16} />Add variable</Button>} /><div className="key-value-list">{Object.entries(values).map(([key, value]) => <div key={key}><input aria-label="Variable name" value={key} onChange={event => { const next = { ...values }; delete next[key]; next[event.target.value] = value; setValues(next); }} /><input aria-label={`${key} value`} type="password" value={value} onChange={event => setValues(current => ({ ...current, [key]: event.target.value }))} autoComplete="new-password" /><Button variant="ghost" size="icon" aria-label={`Remove ${key}`} onClick={() => { const next = { ...values }; delete next[key]; setValues(next); }}><Trash2 size={17} /></Button></div>)}{!Object.keys(values).length && <p className="muted-copy">No workspace defaults. Add only variables that every new application should receive.</p>}</div></Card></>;
}

type UpdateTarget = { server_id: string; name?: string; state: string; message?: string };
type UpdateSettings = { automatic?: boolean; latest?: string; checked?: string; check_error?: string; version?: string; targets?: UpdateTarget[] };
type UpdateServer = { server_id: string; name?: string; current?: string; state?: string; message?: string; supported?: boolean; error?: string };
type UpdatesData = { control_version?: string; latest?: string; settings?: UpdateSettings; servers?: UpdateServer[] };

export function UpdatesPage() {
  const [data, setData] = useState<UpdatesData>();
  const [error, setError] = useState('');
  const [checking, setChecking] = useState(false);
  const [installing, setInstalling] = useState(false);
  const [policyBusy, setPolicyBusy] = useState(false);
  const notify = useToast();

  const refresh = async (background = false) => {
    if (!background) setChecking(true);
    try {
      const value = await get<UpdatesData>('control/updates');
      setData(value);
      setError('');
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      if (!background) setChecking(false);
    }
  };
  useEffect(() => {
    void refresh();
    const timer = window.setInterval(() => { if (!document.hidden) void refresh(true); }, 5_000);
    return () => clearInterval(timer);
  }, []);

  const check = async () => {
    setChecking(true); setError('');
    try {
      await post<UpdateSettings>('control/updates/check');
      await refresh(true);
      notify('Release check complete.');
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally { setChecking(false); }
  };
  const install = async () => {
    setInstalling(true); setError('');
    try {
      const settings = await post<UpdateSettings>('control/updates/install');
      setData(current => current ? { ...current, settings } : current);
      notify('Update queued. Busy servers will continue automatically when active operations finish.');
      window.setTimeout(() => void refresh(true), 1_000);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally { setInstalling(false); }
  };
  const setAutomatic = async (automatic: boolean) => {
    setPolicyBusy(true); setError('');
    const previous = Boolean(data?.settings?.automatic);
    setData(current => current ? { ...current, settings: { ...current.settings, automatic } } : current);
    try {
      const settings = await patch<UpdateSettings>('control/updates', { automatic });
      setData(current => current ? { ...current, settings } : current);
      notify(automatic ? 'Automatic stable updates enabled.' : 'Automatic stable updates disabled.');
    } catch (reason) {
      setData(current => current ? { ...current, settings: { ...current.settings, automatic: previous } } : current);
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally { setPolicyBusy(false); }
  };

  const settings = data?.settings || {};
  const latest = settings.latest || data?.latest || '';
  const targets = settings.targets || [];
  const inProgress = targets.some(target => target.state === 'waiting' || target.state === 'updating');
  const panelCurrent = Boolean(latest && data?.control_version === latest);
  const allCurrent = panelCurrent && (data?.servers || []).every(server => server.current === latest);
  const targetByServer = new Map(targets.map(target => [target.server_id, target]));

  return <>
    <PageHeader eyebrow="Release management" title="Updates" description="Keep the control panel and hosting agents current without interrupting running apps." actions={<div className="sticky-actions"><Button onClick={check} disabled={checking || installing}><RefreshCw className={checking ? 'spin' : ''} size={16} />{checking ? 'Checking…' : 'Check now'}</Button><Button variant="primary" onClick={install} disabled={!latest || allCurrent || inProgress || installing || checking}><UploadCloud size={16} />{installing ? 'Queuing…' : inProgress ? 'Update queued' : 'Install update'}</Button></div>} />
    {error && <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner>}
    {!data ? <Loading /> : <div className="settings-stack">
      <Card><SectionHeader title="Control panel" description={`Installed ${data.control_version || 'unknown'} · Latest stable ${latest || 'unknown'}`} actions={<Badge tone={panelCurrent ? 'success' : 'warning'}>{panelCurrent ? 'Current' : 'Update available'}</Badge>} />{inProgress && <p className="muted-copy">The control panel host is updated last, after every remote host passes its health checks.</p>}</Card>
      <Card><SectionHeader title="Hosting agents" description="The durable queue waits for deployments, backups, restores and scheduled commands to finish, then updates one host at a time." /><div className="simple-list">{(data.servers || []).map(server => { const target = targetByServer.get(server.server_id); const state = target?.state || server.state || (server.current === latest ? 'current' : 'idle'); const message = target?.message || server.message || server.error; return <div key={server.server_id}><div><strong>{server.name || server.server_id}</strong><small>{server.current || 'unknown'} · {state}{message ? ` · ${message}` : ''}</small></div><Badge tone={state === 'succeeded' || state === 'current' ? 'success' : state === 'failed' || Boolean(server.error) ? 'danger' : state === 'waiting' || state === 'updating' ? 'warning' : 'neutral'}>{state}</Badge></div>; })}</div><label className="check-row"><input type="checkbox" checked={Boolean(settings.automatic)} disabled={policyBusy || inProgress} onChange={event => void setAutomatic(event.target.checked)} /><span><strong>Automatically install stable releases</strong><small>Uses the same durable queue and never interrupts deployments, backups or restores.</small></span></label></Card>
    </div>}
  </>;
}

export function TeamPage({ audit = false }: { audit?: boolean }) {
  const [data, setData] = useState<any>(), [error, setError] = useState(''), [query, setQuery] = useState(''); const notify = useToast();
  const load = () => get<any>(`control/${audit ? 'audit' : 'team'}`).then(setData).catch(reason => setError(reason instanceof Error ? reason.message : String(reason)));
  useEffect(() => { void load(); }, [audit]); if (!data && !error) return <Loading overlay label={`Loading ${audit ? 'audit log' : 'team'}…`} />;
  const rows: any[] = Array.isArray(data) ? data : data?.members || data?.items || data?.entries || [];
  const visible = rows.filter(row => JSON.stringify(row).toLowerCase().includes(query.toLowerCase()));
  return <><PageHeader eyebrow="Workspace" title={audit ? 'Audit log' : 'Team'} description={audit ? 'A durable record of sensitive and administrative actions.' : 'Give people the least access needed to operate this workspace.'} />{error && <ErrorBanner>{error}</ErrorBanner>}<Card><SectionHeader title={audit ? 'Activity' : 'Fleet team'} description={audit ? 'Newest events first.' : 'Admins manage infrastructure; deployers release existing apps; viewers inspect status.'} actions={<SearchField value={query} onChange={event => setQuery(event.target.value)} placeholder={`Search ${audit ? 'activity' : 'members'}…`} />} /><div className="simple-list">{visible.map((row, index) => <div key={row.id || row.subject || index}><div><strong>{row.name || row.email || row.subject || row.action || 'Event'}</strong><small>{audit ? `${formatDate(row.created || row.created_at)} · ${row.actor || ''}` : `${row.role || 'viewer'}${row.owner ? ' · installation owner' : ''}`}</small>{audit && <p>{row.target || row.detail || ''}</p>}</div>{!audit && !row.owner && <Button size="sm" variant="danger" onClick={async () => { await del(`control/team/${encodeURIComponent(row.subject || row.email)}`); notify('Member removed.'); load(); }}>Remove</Button>}</div>)}{!visible.length && <EmptyState icon={audit ? <ClipboardList /> : <Users />} title={audit ? 'No activity found' : 'No team members found'} description={query ? 'Try another search.' : audit ? 'Administrative actions will appear here.' : 'The installation owner is always protected.'} />}</div></Card>{!audit && <AddMember onSaved={() => { notify('Team member saved.'); load(); }} onError={setError} />}</>;
}

function AddMember({ onSaved, onError }: { onSaved: () => void; onError: (value: string) => void }) {
  const submit = async (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); const form = event.currentTarget; try { await post('control/team', Object.fromEntries(new FormData(form))); form.reset(); onSaved(); } catch (reason) { onError(reason instanceof Error ? reason.message : String(reason)); } };
  return <Card className="form-card"><SectionHeader title="Add member or change role" /><form onSubmit={submit}><div className="form-grid three"><Field label="Email"><input name="email" type="email" required /></Field><Field label="Name"><input name="name" /></Field><Field label="Role"><select name="role"><option value="viewer">Viewer</option><option value="deployer">Deployer</option><option value="admin">Admin</option></select></Field></div><Button variant="primary">Save member</Button></form></Card>;
}

export function ActivityPage({ security = false, selectedID }: { security?: boolean; selectedID?: string }) {
  const { apps } = useWorkspace(); const [appID, setAppID] = useState(selectedID || apps[0]?.id || ''), [items, setItems] = useState<any[]>([]), [error, setError] = useState(''), [loading, setLoading] = useState(false);
  const load = async (id = appID) => { if (!id) return; setLoading(true); try { const value = await get<any>(`control/apps/${encodeURIComponent(id)}/${security ? 'security' : 'errors'}`); setItems(Array.isArray(value) ? value : value.items || value.errors || value.probes || []); setError(''); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setLoading(false); } };
  useEffect(() => { if (appID) load(); }, [appID, security]);
  return <><PageHeader eyebrow="Application diagnostics" title={security ? 'Security activity' : 'Errors'} description={security ? 'Suspicious and rejected traffic reported by hosted applications.' : 'Application failures grouped into a searchable operational view.'} actions={<Button onClick={() => load()}><RefreshCw size={16} />Refresh</Button>} />{error && <ErrorBanner>{error}</ErrorBanner>}<Card><SectionHeader title={security ? 'Security events' : 'Error groups'} actions={<Field label="Application" className="toolbar-filter"><select value={appID} onChange={event => setAppID(event.target.value)}>{apps.map(app => <option key={app.id}>{app.id}</option>)}</select></Field>} />{loading ? <Loading /> : <div className="simple-list">{items.map((item, index) => <div key={item.id || item.fingerprint || index}><div><strong>{item.title || item.name || item.path || item.kind || (security ? 'Security event' : 'Application error')}</strong><small>{formatDate(item.created || item.last_seen || item.timestamp)} · {item.status || item.level || ''}</small><p>{item.message || item.error || item.detail || ''}</p></div>{item.count && <Badge>{item.count}</Badge>}</div>)}{!items.length && <EmptyState icon={security ? <ShieldCheck /> : <Activity />} title={security ? 'No security events' : 'No application errors'} description="Nothing requiring attention was reported for this application." />}</div>}</Card></>;
}

export function HelpPage() {
  const guides = [{ title: 'First administrator', body: 'Set ADMIN_USERS to a JSON array of user IDs or email addresses before the first sign-in.' }, { title: 'Database migrations', body: 'DB_MIGRATE=true is included for new applications. Keep migrations backward compatible during zero-downtime activation.' }, { title: 'Environment variables', body: 'Use Setting for non-sensitive values and Credential for secrets. Saved credentials stay encrypted and masked.' }, { title: 'Backups and recovery', body: 'Keep local and private off-site copies. Regularly test restoration into a separate database.' }, { title: 'DNS and HTTPS', body: 'Point every A and AAAA record to the hosting server. The agent rechecks DNS and Caddy issues HTTPS when all records arrive.' }];
  return <><PageHeader eyebrow="Documentation" title="Help" description="Short operational guides attached to the workflows you use." /><div className="help-grid">{guides.map(guide => <Card key={guide.title}><BookOpen size={20} /><h2>{guide.title}</h2><p>{guide.body}</p></Card>)}</div></>;
}

export function TwoFactorPage() {
  const [status, setStatus] = useState<any>(), [setup, setSetup] = useState<any>(), [codes, setCodes] = useState<string[]>([]), [error, setError] = useState(''); const notify = useToast();
  const load = () => get<any>('v1/auth/two-factor').then(setStatus).catch(reason => setError(reason instanceof Error ? reason.message : String(reason))); useEffect(() => { void load(); }, []);
  if (!status && !error) return <Loading overlay label="Loading two-factor settings…" />;
  const start = async () => { try { setSetup(await post('v1/auth/two-factor/setup')); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } };
  const enable = async (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); try { const result = await post<{ codes: string[] }>('v1/auth/two-factor/enable', { code: new FormData(event.currentTarget).get('code') }); setCodes(result.codes || []); setSetup(undefined); await load(); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } };
  return <><PageHeader eyebrow="Account security" title="Two-factor authentication" description="Protect credential reveal and just-in-time root console access." back="#applications" />{error && <ErrorBanner>{error}</ErrorBanner>}{codes.length ? <Card className="form-card narrow-form"><SectionHeader title="Save these recovery codes now" description="Each code works once and will not be shown again." /><pre className="recovery-codes">{codes.join('\n')}</pre></Card> : status?.enabled ? <Card className="form-card narrow-form"><SectionHeader title="Authenticator enabled" description="Your account requires a code at sign-in and before sensitive operations." actions={<Badge tone="success"><CheckCircle2 size={13} />Protected</Badge>} /><p><strong>{status.backupCodesLeft || 0}</strong> recovery codes remain.</p></Card> : <Card className="form-card narrow-form"><SectionHeader title="Protect your account" description="Scan a QR code with any TOTP authenticator. You will not need to type the long setup key." />{!setup ? <Button variant="primary" onClick={start}><Smartphone size={16} />Set up authenticator</Button> : <div className="factor-setup">{setup.qrCodeDataUrl && <img src={setup.qrCodeDataUrl} alt="QR code for Līdza Deploy two-factor setup" />}<div><p>Open your authenticator app and scan this QR code.</p><a className="button button-secondary button-md" href={setup.otpauthUrl}>Open authenticator app</a><details><summary>Can’t scan the QR code?</summary><code>{setup.secret}</code></details><form onSubmit={enable}><Field label="Six-digit code"><input name="code" inputMode="numeric" autoComplete="one-time-code" required /></Field><Button variant="primary">Verify & enable</Button></form></div></div>}</Card>}</>;
}
