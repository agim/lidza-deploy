import { ComponentGallery } from '../components/ComponentGallery';
import { Input } from '../components/primitives/input';
import { Activity, BookOpen, CheckCircle2, ChevronDown, ClipboardList, Clock3, KeyRound, Plus, RefreshCw, Route as RouteIcon, Search, Server, Settings2, ShieldCheck, Smartphone, Trash2, UploadCloud, Users } from 'lucide-react';
import { type FormEvent, useEffect, useState } from 'react';
import { del, get, patch, post, put } from '../lib/api';
import { formatDate } from '../lib/format';
import { useWorkspace } from '../state/WorkspaceContext';
import { useToast } from '../components/ToastProvider';
import { ActionForm, Badge, Button, Card, CopyButton, LogOutputHeader, EmptyState, ErrorBanner, Field, Loading, Notice, PageHeader, SearchField, SectionHeader } from '../components/ui';

export function DefaultsPage() {
  const [values, setValues] = useState<Record<string, string>>({}), [error, setError] = useState(''), [busy, setBusy] = useState(false); const notify = useToast();
  useEffect(() => { get<{ values?: Record<string, string> }>('control/application-defaults').then(value => setValues(value.values || {})).catch(reason => setError(reason instanceof Error ? reason.message : String(reason))); }, []);
  const save = async () => { setBusy(true); try { await put('control/application-defaults', { values }); notify('Application defaults saved. They apply only on an app’s first deployment.'); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setBusy(false); } };
  return <><PageHeader eyebrow="Workspace defaults" title="Application defaults" description="Shared variables copied once, before each new application’s first deployment." actions={<Button variant="primary" onClick={save} disabled={busy}>Save defaults</Button>} />{error && <ErrorBanner>{error}</ErrorBanner>}<Card className="form-card"><SectionHeader title="Environment variables" description="App-specific values take precedence. Existing applications never change when these defaults change." actions={<Button onClick={() => { let name = 'VARIABLE'; let index = 2; while (Object.hasOwn(values, name)) name = `VARIABLE_${index++}`; setValues(current => ({ ...current, [name]: '' })); }}><Plus size={16} />Add variable</Button>} /><div className="key-value-list">{Object.entries(values).map(([key, value]) => <div key={key}><Input aria-label="Variable name" value={key} onChange={event => { const next = { ...values }; delete next[key]; next[event.target.value] = value; setValues(next); }} /><Input aria-label={`${key} value`} type="password" value={value} onChange={event => setValues(current => ({ ...current, [key]: event.target.value }))} autoComplete="new-password" /><Button variant="ghost" size="icon" aria-label={`Remove ${key}`} onClick={() => { const next = { ...values }; delete next[key]; setValues(next); }}><Trash2 size={17} /></Button></div>)}{!Object.keys(values).length && <p className="muted-copy">No workspace defaults. Add only variables that every new application should receive.</p>}</div></Card></>;
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
      <Card><SectionHeader title="Hosting agents" description="The durable queue waits for deployments, backups, restores and scheduled commands to finish, then updates one host at a time." /><div className="simple-list">{(data.servers || []).map(server => { const target = targetByServer.get(server.server_id); const state = target?.state || server.state || (server.current === latest ? 'current' : 'idle'); const message = target?.message || server.message || server.error; return <div key={server.server_id}><div><strong>{server.name || server.server_id}</strong><small>{server.current || 'unknown'} · {state}{message ? ` · ${message}` : ''}</small></div><Badge tone={state === 'succeeded' || state === 'current' ? 'success' : state === 'failed' || Boolean(server.error) ? 'danger' : state === 'waiting' || state === 'updating' ? 'warning' : 'neutral'}>{state}</Badge></div>; })}</div><label className="check-row"><Input type="checkbox" checked={Boolean(settings.automatic)} disabled={policyBusy || inProgress} onChange={event => void setAutomatic(event.target.checked)} /><span><strong>Automatically install stable releases</strong><small>Uses the same durable queue and never interrupts deployments, backups or restores.</small></span></label></Card>
    </div>}
  </>;
}

export function TeamPage({ audit = false }: { audit?: boolean }) {
  const [data, setData] = useState<any>(), [error, setError] = useState(''), [query, setQuery] = useState(''); const notify = useToast();
  const load = () => get<any>(`control/${audit ? 'audit' : 'team'}`).then(setData).catch(reason => setError(reason instanceof Error ? reason.message : String(reason)));
  useEffect(() => { void load(); }, [audit]); if (!data && !error) return <Loading overlay label={`Loading ${audit ? 'audit log' : 'team'}…`} />;
  const rows: any[] = Array.isArray(data) ? data : data?.members || data?.items || data?.entries || [];
  const visible = rows.filter(row => JSON.stringify(row).toLowerCase().includes(query.toLowerCase()));
  return <><PageHeader eyebrow="Workspace" title={audit ? 'Audit log' : 'Team'} description={audit ? 'A durable record of sensitive and administrative actions.' : 'Give people the least access needed to operate this workspace.'} actions={audit ? <CopyButton value={visible.map(row => [formatDate(row.created || row.created_at), row.actor, row.action || row.subject, row.target || row.detail].filter(Boolean).join(' · ')).join('\n')} label="Copy audit log" /> : undefined} />{error && <ErrorBanner>{error}</ErrorBanner>}<Card><SectionHeader title={audit ? 'Activity' : 'Fleet team'} description={audit ? 'Newest events first.' : 'Admins manage infrastructure; deployers release existing apps; viewers inspect status.'} actions={<SearchField value={query} onChange={event => setQuery(event.target.value)} placeholder={`Search ${audit ? 'activity' : 'members'}…`} />} /><div className="simple-list">{visible.map((row, index) => <div key={row.id || row.subject || index}><div><strong>{row.name || row.email || row.subject || row.action || 'Event'}</strong><small>{audit ? `${formatDate(row.created || row.created_at)} · ${row.actor || ''}` : `${row.role || 'viewer'}${row.owner ? ' · installation owner' : ''}`}</small>{audit && <p>{row.target || row.detail || ''}</p>}</div>{!audit && !row.owner && <Button size="sm" variant="danger" onClick={async () => { await del(`control/team/${encodeURIComponent(row.subject || row.email)}`); notify('Member removed.'); load(); }}>Remove</Button>}</div>)}{!visible.length && <EmptyState icon={audit ? <ClipboardList /> : <Users />} title={audit ? 'No activity found' : 'No team members found'} description={query ? 'Try another search.' : audit ? 'Administrative actions will appear here.' : 'The installation owner is always protected.'} />}</div></Card>{!audit && <AddMember onSaved={() => { notify('Team member saved.'); load(); }} onError={setError} />}</>;
}

function AddMember({ onSaved, onError }: { onSaved: () => void; onError: (value: string) => void }) {
  const submit = async (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); const form = event.currentTarget; try { await post('control/team', Object.fromEntries(new FormData(form))); form.reset(); onSaved(); } catch (reason) { onError(reason instanceof Error ? reason.message : String(reason)); } };
  return <Card className="form-card"><SectionHeader title="Add member or change role" /><ActionForm onSubmit={submit}><div className="form-grid three"><Field label="Email"><Input name="email" type="email" required /></Field><Field label="Name"><Input name="name" /></Field><Field label="Role"><select name="role"><option value="viewer">Viewer</option><option value="deployer">Deployer</option><option value="admin">Admin</option></select></Field></div><Button variant="primary">Save member</Button></ActionForm></Card>;
}

type ActivityItem = {
  id?: string;
  source?: string;
  message?: string;
  error?: string;
  detail?: string;
  stack?: string;
  route?: string;
  path?: string;
  method?: string;
  requestId?: string;
  request_id?: string;
  fingerprint?: string;
  createdAt?: string;
  created_at?: string;
  created?: string;
  last_seen?: string;
  timestamp?: string;
  title?: string;
  name?: string;
  kind?: string;
  status?: string | number;
  level?: string;
  count?: number;
  app_id?: string;
  container?: string;
  release?: string;
  commit?: string;
  security?: { category?: string; status?: number; outcome?: string };
};

type ActivityResponse = {
  errors?: ActivityItem[];
  items?: ActivityItem[];
  probes?: ActivityItem[];
  status?: string;
  truncated?: boolean;
  last_received?: string;
  collection_error?: string;
};

type ActivityGroup = { key: string; items: ActivityItem[]; latest: ActivityItem; first: ActivityItem; count: number };

function activityTime(item: ActivityItem) {
  return item.createdAt || item.created_at || item.created || item.last_seen || item.timestamp;
}

function activityTimeValue(item: ActivityItem) {
  const value = activityTime(item);
  if (!value) return 0;
  const time = new Date(value).valueOf();
  return Number.isNaN(time) ? 0 : time;
}

function activityGroups(items: ActivityItem[]) {
  const sorted = [...items].sort((a, b) => activityTimeValue(b) - activityTimeValue(a));
  const groups = new Map<string, ActivityItem[]>();
  sorted.forEach((item, index) => {
    const key = item.fingerprint || item.id || `activity-${index}`;
    groups.set(key, [...(groups.get(key) || []), item]);
  });
  return [...groups.entries()].map(([key, rows]): ActivityGroup => ({
    key,
    items: rows,
    latest: rows[0],
    first: rows[rows.length - 1],
    count: Math.max(rows.length, Number(rows[0].count) || 0),
  }));
}

function activityTitle(item: ActivityItem, security: boolean) {
  if (security && item.security?.category) return item.security.category.replaceAll('-', ' ');
  return item.title || item.name || item.message || item.error || item.path || item.kind || (security ? 'Security event' : 'Application error');
}

function ActivityGroupCard({ group, security }: { group: ActivityGroup; security: boolean }) {
  const item = group.latest;
  const occurredAt = activityTime(item);
  const route = item.route || item.path;
  const status = item.security?.status || item.status;
  const message = item.message || item.error || item.detail;
  const title = activityTitle(item, security);
  return <article className="activity-card">
    <div className="activity-card-summary">
      <div className="activity-card-heading">
        <strong>{title}</strong>
        <Badge tone={security && item.security?.outcome === 'review-response' ? 'warning' : 'neutral'}>{group.count} {group.count === 1 ? 'occurrence' : 'occurrences'}</Badge>
      </div>
      <div className="activity-meta">
        <span><Clock3 size={14} aria-hidden="true" /><span><b>Last seen</b> <time dateTime={occurredAt}>{formatDate(occurredAt)}</time></span></span>
        {(item.method || route) && <span><RouteIcon size={14} aria-hidden="true" /><span>{item.method && <b>{item.method}</b>} {route || 'Unknown route'}{status ? ` · HTTP ${status}` : ''}</span></span>}
        {(item.source || item.container) && <span><Server size={14} aria-hidden="true" /><span>{[item.source, item.container].filter(Boolean).join(' · ')}</span></span>}
      </div>
      {message && message !== title && <p>{message}</p>}
    </div>
    <details className="activity-details">
      <summary><span>View details</span><ChevronDown size={16} aria-hidden="true" /></summary>
      <div className="activity-details-body"><CopyButton value={JSON.stringify({ title, first_seen: activityTime(group.first), last_seen: occurredAt, occurrences: group.count, source: item.source, method: item.method, route, status, container: item.container, release: item.release, commit: item.commit, message, stack: item.stack, security: item.security, recent_occurrences: group.items.slice(0, 20).map(event => ({ time: activityTime(event), request_id: event.requestId || event.request_id })) }, null, 2)} label="Copy event details" />
        <dl className="activity-facts">
          <div><dt>First seen</dt><dd><time dateTime={activityTime(group.first)}>{formatDate(activityTime(group.first))}</time></dd></div>
          <div><dt>Last seen</dt><dd><time dateTime={occurredAt}>{formatDate(occurredAt)}</time></dd></div>
          <div><dt>Source</dt><dd>{item.source || 'Unknown'}</dd></div>
          <div><dt>Request</dt><dd>{[item.method, route].filter(Boolean).join(' ') || 'Not reported'}</dd></div>
          {security && <div><dt>Classification</dt><dd>{[item.security?.category, item.security?.outcome].filter(Boolean).join(' · ') || 'Not reported'}</dd></div>}
          {item.container && <div><dt>Container</dt><dd>{item.container}</dd></div>}
          {item.release && <div><dt>Release</dt><dd>{item.release}</dd></div>}
          {item.commit && <div><dt>Commit</dt><dd>{item.commit}</dd></div>}
        </dl>
        {message && <section className="activity-detail-section"><LogOutputHeader title="Message" value={message} copyLabel="Copy message" /><pre>{message}</pre></section>}
        {item.stack && <section className="activity-detail-section"><LogOutputHeader title="Stack trace" value={item.stack} copyLabel="Copy stack trace" /><pre>{item.stack}</pre></section>}
        <section className="activity-detail-section">
          <h3>Recent occurrences</h3>
          <div className="activity-occurrences">
            {group.items.slice(0, 20).map((occurrence, index) => {
              const occurrenceRequestID = occurrence.requestId || occurrence.request_id;
              return <div className="activity-occurrence" key={occurrence.id || `${activityTime(occurrence)}-${index}`}>
                <time dateTime={activityTime(occurrence)}>{formatDate(activityTime(occurrence))}</time>
                {occurrenceRequestID ? <><code>{occurrenceRequestID}</code><CopyButton value={occurrenceRequestID} label="Copy ID" /></> : <span className="muted-copy">No request ID</span>}
              </div>;
            })}
          </div>
        </section>
      </div>
    </details>
  </article>;
}

export function ActivityPage({ security = false, selectedID }: { security?: boolean; selectedID?: string }) {
  const { apps } = useWorkspace();
  const [appID, setAppID] = useState(selectedID || apps[0]?.id || '');
  const [response, setResponse] = useState<ActivityResponse>();
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  const [query, setQuery] = useState('');
  useEffect(() => { if (!appID && apps[0]?.id) setAppID(apps[0].id); }, [appID, apps]);
  const load = async (id = appID) => {
    if (!id) return;
    setLoading(true);
    try {
      const value = await get<ActivityResponse | ActivityItem[]>(`control/apps/${encodeURIComponent(id)}/${security ? 'security' : 'errors'}`);
      setResponse(Array.isArray(value) ? { items: value } : value);
      setError('');
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally { setLoading(false); }
  };
  useEffect(() => { if (appID) void load(); }, [appID, security]);
  const items = response?.items || response?.errors || response?.probes || [];
  const groups = activityGroups(items).filter(group => {
    const needle = query.trim().toLowerCase();
    if (!needle) return true;
    return group.items.some(item => [item.message, item.error, item.detail, item.stack, item.route, item.path, item.method, item.requestId, item.request_id, item.source, item.container, item.release, item.commit, item.security?.category, item.security?.outcome].some(value => String(value || '').toLowerCase().includes(needle)));
  });
  const collectorStatus = response?.status?.replaceAll('_', ' ');
  return <>
    <PageHeader eyebrow="Application diagnostics" title={security ? 'Security activity' : 'Errors'} description={security ? 'Suspicious and rejected traffic reported by hosted applications.' : 'Application failures grouped into a searchable operational view.'} actions={<Button onClick={() => load()} disabled={loading}><RefreshCw className={loading ? 'spin' : ''} size={16} />Refresh</Button>} />
    {error && <ErrorBanner>{error}</ErrorBanner>}
    {response?.collection_error && <Notice><strong>Collector warning:</strong>&nbsp; {response.collection_error}</Notice>}
    <Card>
      <SectionHeader title={security ? 'Security events' : 'Error groups'} description={`${groups.length} ${groups.length === 1 ? 'group' : 'groups'}${collectorStatus ? ` · collector ${collectorStatus}` : ''}${response?.last_received ? ` · last report ${formatDate(response.last_received)}` : ''}${response?.truncated ? ' · newest 500 occurrences shown' : ''}`} actions={<div className="activity-filters"><SearchField value={query} onChange={event => setQuery(event.target.value)} placeholder={`Search ${security ? 'security events' : 'errors'}…`} /><Field label="Application" className="toolbar-filter"><select value={appID} onChange={event => setAppID(event.target.value)}>{apps.map(app => <option key={app.id} value={app.id}>{app.id}</option>)}</select></Field></div>} />
      {loading ? <Loading /> : <div className="activity-list">{groups.map(group => <ActivityGroupCard key={group.key} group={group} security={security} />)}{!groups.length && <EmptyState icon={security ? <ShieldCheck /> : <Activity />} title={query ? 'No matching activity' : security ? 'No security events' : 'No application errors'} description={query ? 'Try a different message, route, request ID, release or container.' : 'Nothing requiring attention was reported for this application.'} />}</div>}
    </Card>
  </>;
}

export function HelpPage() {
  const guides = [{ title: 'First administrator', body: 'Set ADMIN_USERS to a JSON array of user IDs or email addresses before the first sign-in.' }, { title: 'Database migrations', body: 'DB_MIGRATE=true is included for new applications. Keep migrations backward compatible during zero-downtime activation.' }, { title: 'Environment variables', body: 'Use Setting for non-sensitive values and Credential for secrets. Saved credentials stay encrypted and masked.' }, { title: 'Backups and recovery', body: 'Keep local and private off-site copies. Regularly test restoration into a separate database.' }, { title: 'DNS and HTTPS', body: 'Point every A and AAAA record to the hosting server. The agent rechecks DNS and Caddy issues HTTPS when all records arrive.' }];
  return <><PageHeader eyebrow="Documentation" title="Help" description="Short operational guides attached to the workflows you use." /><div className="help-grid">{guides.map(guide => <Card key={guide.title}><BookOpen size={20} /><h2>{guide.title}</h2><p>{guide.body}</p></Card>)}</div><ComponentGallery /></>;
}

export function TwoFactorPage() {
  const [status, setStatus] = useState<any>(), [setup, setSetup] = useState<any>(), [codes, setCodes] = useState<string[]>([]), [error, setError] = useState(''); const notify = useToast();
  const load = () => get<any>('v1/auth/two-factor').then(setStatus).catch(reason => setError(reason instanceof Error ? reason.message : String(reason))); useEffect(() => { void load(); }, []);
  if (!status && !error) return <Loading overlay label="Loading two-factor settings…" />;
  const start = async () => { try { setSetup(await post('v1/auth/two-factor/setup')); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } };
  const enable = async (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); try { const result = await post<{ codes: string[] }>('v1/auth/two-factor/enable', { code: new FormData(event.currentTarget).get('code') }); setCodes(result.codes || []); setSetup(undefined); await load(); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } };
  return <><PageHeader eyebrow="Account security" title="Two-factor authentication" description="Protect credential reveal and just-in-time root console access." back="#applications" />{error && <ErrorBanner>{error}</ErrorBanner>}{codes.length ? <Card className="form-card narrow-form"><SectionHeader title="Save these recovery codes now" description="Each code works once and will not be shown again." /><pre className="recovery-codes">{codes.join('\n')}</pre></Card> : status?.enabled ? <Card className="form-card narrow-form"><SectionHeader title="Authenticator enabled" description="Your account requires a code at sign-in and before sensitive operations." actions={<Badge tone="success"><CheckCircle2 size={13} />Protected</Badge>} /><p><strong>{status.backupCodesLeft || 0}</strong> recovery codes remain.</p></Card> : <Card className="form-card narrow-form"><SectionHeader title="Protect your account" description="Scan a QR code with any TOTP authenticator. You will not need to type the long setup key." />{!setup ? <Button variant="primary" onClick={start}><Smartphone size={16} />Set up authenticator</Button> : <div className="factor-setup">{setup.qrCodeDataUrl && <img src={setup.qrCodeDataUrl} alt="QR code for Līdza Deploy two-factor setup" />}<div><p>Open your authenticator app and scan this QR code.</p><a className="button button-secondary button-md" href={setup.otpauthUrl}>Open authenticator app</a><details><summary>Can’t scan the QR code?</summary><code>{setup.secret}</code></details><ActionForm onSubmit={enable}><Field label="Six-digit code"><Input name="code" inputMode="numeric" autoComplete="one-time-code" required /></Field><Button variant="primary">Verify & enable</Button></ActionForm></div></div>}</Card>}</>;
}
