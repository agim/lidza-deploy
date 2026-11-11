import { usePreference } from '../state/PreferencesContext';
import { createHref, goBack, indexHref, navigate, resourceHref, withSticky } from '../lib/router';
import { DangerZone, DangerZoneRow, ResourceDetailPage, ResourceIndexPage, ResourceName, visibleTabs } from '../components/resource';
import { ResourceList, type Column } from '../components/ResourceList';
import { Cluster } from '../components/layout';
import { ComponentGallery } from '../components/ComponentGallery';
import { Input } from '../components/primitives/input';
import { Activity, BookOpen, CheckCircle2, ChevronDown, ClipboardList, Clock3, KeyRound, Plus, RefreshCw, Route as RouteIcon, Search, Server, Settings2, ShieldCheck, Smartphone, Trash2, UploadCloud, Users } from 'lucide-react';
import { type FormEvent, useEffect, useState } from 'react';
import { del, get, patch, post, put } from '../lib/api';
import { formatDate, roleLabels } from '../lib/format';
import { searchHelp } from '../lib/help';
import { useWorkspace } from '../state/WorkspaceContext';
import { useToast } from '../components/ToastProvider';
import { SummaryList } from '../components/SummaryList';
import { ActionForm, Badge, Button, Card, ConfirmAction, CopyButton, LogOutputHeader, EmptyState, ErrorBanner, Field, Loading, Notice, PageHeader, SearchField, SectionHeader, Toolbar } from '../components/ui';
import { type DefaultsResponse, type DefaultsRuntime, defaultsRuntimeLabels, defaultsRuntimes, maxRuntimeDefaults, runtimeProfile } from '../lib/defaults';

type DefaultsRow = { id: string; key: string; value: string };
const runtimeDefaultsCopy: Record<Exclude<DefaultsRuntime, 'go'>, { description: string; empty: string; placeholder: string }> = {
  rails: { description: 'Copied into new container Rails applications before their first deployment. Existing Rails applications, container or native, can review and apply them from Settings → Environment.', empty: 'No Ruby on Rails defaults. New Rails applications start with only their own variables.', placeholder: 'RAILS_ENV' },
  astro: { description: 'Copied into new container Astro sites before their first deployment. Existing Astro sites, container or native, can review and apply them from Settings → Environment.', empty: 'No Astro defaults. New Astro sites start with only their own variables.', placeholder: 'NODE_ENV' },
  node: { description: 'Node.js applications on hosts review and apply these from Settings → Environment.', empty: 'No Node.js defaults. Node.js applications keep only their own variables.', placeholder: 'NODE_ENV' },
};
const toRows = (values?: Record<string, string>): DefaultsRow[] => Object.entries(values || {}).sort(([a], [b]) => a.localeCompare(b)).map(([key, value]) => ({ id: crypto.randomUUID(), key, value }));

/** DefaultsPage: shared variables per runtime, one tab each. */
export function DefaultsPage({ tab = 'go' }: { tab?: string }) {
  const [values, setValues] = useState<Record<string, string>>({}), [error, setError] = useState(''), [busy, setBusy] = useState(false); const notify = useToast();
  const runtime = (defaultsRuntimes as readonly string[]).includes(tab) ? tab as DefaultsRuntime : 'go';
  const [rows, setRows] = useState<Record<Exclude<DefaultsRuntime, 'go'>, DefaultsRow[]>>({ rails: [], astro: [], node: [] });
  const [errors, setErrors] = useState<Partial<Record<DefaultsRuntime, string>>>({});
  const load = (response: DefaultsResponse) => { setValues(response.values || {}); setRows({ rails: toRows(runtimeProfile(response, 'rails').values), astro: toRows(runtimeProfile(response, 'astro').values), node: toRows(runtimeProfile(response, 'node').values) }); };
  useEffect(() => { get<DefaultsResponse>('control/application-defaults').then(load).catch(reason => setError(reason instanceof Error ? reason.message : String(reason))); }, []);
  const fail = (target: DefaultsRuntime, message: string) => setErrors(current => ({ ...current, [target]: message }));
  const save = async () => {
    setBusy(true); setErrors(current => ({ ...current, [runtime]: '' }));
    try {
      if (runtime === 'go') {
        await put('control/application-defaults', { values });
        notify('Application defaults saved. They apply only on an app’s first deployment.');
        return;
      }
      const next: Record<string, string> = {};
      for (const row of rows[runtime]) {
        const key = row.key.trim();
        if (!key && !row.value.trim()) continue;
        if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) throw new Error(`${key || 'Variable'} is not a valid environment variable name.`);
        if (Object.hasOwn(next, key)) throw new Error(`${key} appears more than once.`);
        next[key] = row.value;
      }
      if (Object.keys(next).length > maxRuntimeDefaults) throw new Error(`At most ${maxRuntimeDefaults} defaults per runtime.`);
      const saved = await put<DefaultsResponse>('control/application-defaults', { runtime, values: next });
      setRows(current => ({ ...current, [runtime]: toRows(runtimeProfile(saved, runtime).values) }));
      notify(`${defaultsRuntimeLabels[runtime]} defaults saved.`);
    } catch (reason) { fail(runtime, reason instanceof Error ? reason.message : String(reason)); } finally { setBusy(false); }
  };
  const updateRow = (target: Exclude<DefaultsRuntime, 'go'>, id: string, change: Partial<DefaultsRow>) => setRows(current => ({ ...current, [target]: current[target].map(row => row.id === id ? { ...row, ...change } : row) }));
  const goEditor = <Card className="form-card"><SectionHeader title="Environment variables" description="App-specific values take precedence. Existing applications never change when these defaults change." actions={<Button onClick={() => { let name = 'VARIABLE'; let index = 2; while (Object.hasOwn(values, name)) name = `VARIABLE_${index++}`; setValues(current => ({ ...current, [name]: '' })); }}><Plus size={16} />Add variable</Button>} />{errors.go && <ErrorBanner onDismiss={() => fail('go', '')}>{errors.go}</ErrorBanner>}<form className="key-value-list" aria-label="Go environment defaults" onSubmit={event => { event.preventDefault(); if (!busy) void save(); }}>{Object.entries(values).map(([key, value]) => <div key={key}><Input aria-label="Variable name" value={key} autoComplete="off" spellCheck={false} onChange={event => { const next = { ...values }; delete next[key]; next[event.target.value] = value; setValues(next); }} /><Input aria-label={`${key} value`} type="password" value={value} onChange={event => setValues(current => ({ ...current, [key]: event.target.value }))} autoComplete="new-password" /><Button type="button" variant="ghost" size="icon" aria-label={`Remove ${key}`} onClick={() => { const next = { ...values }; delete next[key]; setValues(next); }}><Trash2 size={17} /></Button></div>)}{!Object.keys(values).length && <p className="muted-copy">No application defaults. Add only variables that every new application should receive.</p>}</form></Card>;
  const runtimeEditor = (target: Exclude<DefaultsRuntime, 'go'>) => { const copy = runtimeDefaultsCopy[target]; const list = rows[target]; return <Card className="form-card"><SectionHeader title={`${defaultsRuntimeLabels[target]} environment`} description={copy.description} actions={<Button disabled={list.length >= maxRuntimeDefaults} onClick={() => setRows(current => ({ ...current, [target]: [...current[target], { id: crypto.randomUUID(), key: '', value: '' }] }))}><Plus size={16} />Add variable</Button>} />{errors[target] && <ErrorBanner onDismiss={() => fail(target, '')}>{errors[target]}</ErrorBanner>}<div className="key-value-list">{list.map(row => <div key={row.id}><Input aria-label="Variable name" value={row.key} placeholder={copy.placeholder} autoComplete="off" spellCheck={false} onChange={event => updateRow(target, row.id, { key: event.target.value })} /><Input aria-label={`${row.key || 'Variable'} value`} value={row.value} placeholder="Value" autoComplete="off" maxLength={2048} onChange={event => updateRow(target, row.id, { value: event.target.value })} /><Button variant="ghost" size="icon" aria-label={`Remove ${row.key || 'variable'}`} onClick={() => setRows(current => ({ ...current, [target]: current[target].filter(item => item.id !== row.id) }))}><Trash2 size={17} /></Button></div>)}{!list.length && <p className="muted-copy">{copy.empty}</p>}</div><div className="env-help"><KeyRound size={18} /><p>Only ordinary settings belong here; values are visible to administrators. Credentials, database and service URLs, and host variables such as PATH, PORT or LIDZA_* stay on each application. Empty values are dropped when saved.</p></div></Card>; };
  return <ResourceDetailPage resource="defaults" tab={tab} title="Application defaults" description="Shared variables for each runtime, copied before a new application’s first deployment and offered to existing applications for review." actions={<Button variant="primary" onClick={save} disabled={busy}>Save {defaultsRuntimeLabels[runtime]} defaults</Button>} notices={error ? <ErrorBanner>{error}</ErrorBanner> : undefined}>{runtime === 'go' ? goEditor : runtimeEditor(runtime)}</ResourceDetailPage>;
}

type UpdateTarget = { server_id: string; name?: string; state: string; message?: string };
type UpdateSettings = { automatic?: boolean; latest?: string; checked?: string; check_error?: string; version?: string; targets?: UpdateTarget[] };
type UpdateServer = { server_id: string; name?: string; current?: string; state?: string; message?: string; supported?: boolean; error?: string };
type UpdateChannel = { configured?: boolean; repository?: string; installation?: number; keys?: string[]; keys_pinned?: boolean; bundle_version?: string; key_id?: string };
type UpdatesData = { control_version?: string; latest?: string; settings?: UpdateSettings; servers?: UpdateServer[]; channel?: UpdateChannel; automatic_updates_supported?: boolean };

/** UpdatesPage: releases of the control panel and agents (Overview) and the private update channel. */
export function UpdatesPage({ tab = 'overview' }: { tab?: string }) {
  const [data, setData] = useState<UpdatesData>();
  const [error, setError] = useState('');
  const [checking, setChecking] = useState(false);
  const [installing, setInstalling] = useState(false);
  const [policyBusy, setPolicyBusy] = useState(false);
  const [repository, setRepository] = useState('');
  const [channelBusy, setChannelBusy] = useState(false);
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

  const saveChannel = async (value: string) => {
    setChannelBusy(true); setError('');
    try {
      const result = await post<{ settings: UpdateSettings; channel: UpdateChannel }>('control/updates/channel', { repository: value });
      setData(current => current ? { ...current, settings: result.settings, channel: result.channel } : current);
      notify(value ? `Signed update channel bound to ${value}.` : 'Signed update channel cleared.');
      await refresh(true);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally { setChannelBusy(false); }
  };

  // The bound repository is the field's value (not its placeholder) until the operator edits it.
  const boundRepository = data?.channel?.repository || '';
  useEffect(() => { if (boundRepository) setRepository(current => current || boundRepository); }, [boundRepository]);

  const settings = data?.settings || {};
  const channel = data?.channel || {};
  const privateChannel = Boolean(channel.configured);
  const privateBuild = Boolean(channel.keys_pinned);
  const automaticSupported = Boolean(data?.automatic_updates_supported);
  const latest = settings.latest || data?.latest || '';
  const latestLabel = privateChannel ? 'Latest signed release' : privateBuild ? 'Signed channel' : 'Latest stable';
  const latestValue = latest || (privateChannel ? 'none published' : privateBuild ? 'not configured' : 'unknown');
  const targets = settings.targets || [];
  // A failed target ends the batch (the panel refuses to continue past it), so a new install may be queued.
  const failedTarget = targets.some(target => target.state === 'failed');
  const inProgress = !failedTarget && targets.some(target => target.state === 'waiting' || target.state === 'updating');
  const panelCurrent = Boolean(latest && data?.control_version === latest);
  const allCurrent = panelCurrent && (data?.servers || []).every(server => server.current === latest);
  const targetByServer = new Map(targets.map(target => [target.server_id, target]));

  return <ResourceDetailPage resource="updates" tab={tab} tabs={visibleTabs('updates', [!privateBuild && 'channel'])} title="Updates" description="Keep the control panel and hosting agents current without interrupting running apps." facts={data ? [`Installed ${data.control_version || 'unknown'}`, `${latestLabel} ${latestValue}`] : undefined} actions={<div className="sticky-actions"><Button onClick={check} disabled={checking || installing}><RefreshCw className={checking ? 'spin' : ''} size={16} />{checking ? 'Checking…' : 'Check now'}</Button><Button variant="primary" onClick={install} disabled={!latest || allCurrent || inProgress || installing || checking}><UploadCloud size={16} />{installing ? 'Queuing…' : inProgress ? 'Update queued' : 'Install update'}</Button></div>}
    notices={error ? <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner> : undefined}>
    {!data ? <Loading /> : <div className="settings-stack">
      {tab === 'overview' && <Card><SectionHeader title="Control panel" description={`Installed ${data.control_version || 'unknown'} · ${latestLabel} ${latestValue}`} actions={<Badge tone={panelCurrent ? 'success' : latest ? 'warning' : 'neutral'}>{panelCurrent ? 'Current' : latest ? 'Update available' : 'No release'}</Badge>} />{inProgress && <p className="muted-copy">The control panel host is updated last, after every remote host passes its health checks.</p>}</Card>}
      {tab === 'channel' && privateBuild && <Card><SectionHeader title="Signed update channel" description="Signed bundles are published as releases on this distribution's own repository and verified against the signing keys compiled into every binary before anything is installed." actions={<Badge tone={privateChannel ? 'success' : 'warning'}>{privateChannel ? 'Configured' : 'Not configured'}</Badge>} />
        <div className="simple-list">
          <div><div><strong>Repository</strong><small>{channel.repository || 'none'}{channel.installation ? ` · GitHub App installation ${channel.installation}` : ''}</small></div></div>
          <div><div><strong>Trusted signing keys</strong><small>{(channel.keys || []).join(' · ') || 'none pinned in this build'}</small></div></div>
          {channel.bundle_version && <div><div><strong>Verified release</strong><small>{channel.bundle_version}{channel.key_id ? ` · signed by ${channel.key_id}` : ''}</small></div></div>}
          {settings.check_error && <div><div><strong>Last check</strong><small>{settings.check_error}</small></div></div>}
        </div>
        <ActionForm onSubmit={event => { event.preventDefault(); void saveChannel(repository.trim()); }}><div className="form-grid two"><Field label="Release repository" hint="owner/name, readable by the connected GitHub App"><Input value={repository} onChange={event => setRepository(event.target.value)} placeholder="owner/deploy-private" disabled={channelBusy || inProgress} /></Field></div><div className="sticky-actions"><Button variant="primary" disabled={channelBusy || inProgress || !repository.trim() || repository.trim() === boundRepository}>{channelBusy ? 'Saving…' : 'Bind channel'}</Button>{privateChannel && <Button variant="danger" type="button" disabled={channelBusy || inProgress} onClick={() => void saveChannel('')}>Clear channel</Button>}</div></ActionForm>
      </Card>}
      {tab === 'overview' && <Card><SectionHeader title="Hosting agents" description="The durable queue waits for deployments, backups, restores and scheduled commands to finish, then updates one host at a time." /><div className="simple-list">{(data.servers || []).map(server => { const target = targetByServer.get(server.server_id); const state = target?.state || server.state || (server.current === latest ? 'current' : 'idle'); const message = target?.message || server.message || server.error; return <div key={server.server_id}><div><strong>{server.name || server.server_id}</strong><small>{server.current || 'unknown'} · {state}{message ? ` · ${message}` : ''}</small></div><Badge tone={state === 'succeeded' || state === 'current' ? 'success' : state === 'failed' || Boolean(server.error) ? 'danger' : state === 'waiting' || state === 'updating' ? 'warning' : 'neutral'}>{state}</Badge></div>; })}</div><label className="check-row"><Input type="checkbox" checked={Boolean(settings.automatic)} disabled={policyBusy || inProgress || !automaticSupported} onChange={event => void setAutomatic(event.target.checked)} /><span><strong>{privateBuild ? 'Automatically install verified private releases' : 'Automatically install stable releases'}</strong><small>{automaticSupported ? 'Uses the same durable queue and never interrupts deployments, backups or restores.' : 'Bind the private update channel first.'}</small></span></label></Card>}
    </div>}
  </ResourceDetailPage>;
}

type Member = { id?: string; subject?: string; email?: string; name?: string; role?: string; owner?: boolean };
type AuditEntry = { id?: string; action?: string; subject?: string; actor?: string; target?: string; detail?: string; created?: string; created_at?: string };
const memberID = (row: Member) => row.subject || row.email || row.id || '';
const roleCopy = 'Admins manage infrastructure; deployers release existing apps; viewers inspect status.';

/** useList loads a list endpoint of the control API. */
function useList<T>(path: string, pick: (data: any) => T[]) {
  const [rows, setRows] = useState<T[]>(), [error, setError] = useState('');
  const load = () => get<any>(path).then(value => { setRows(Array.isArray(value) ? value : pick(value)); setError(''); }).catch(reason => setError(reason instanceof Error ? reason.message : String(reason)));
  useEffect(() => { void load(); }, [path]);
  return { rows, error, load };
}

/** TeamPage: the index of people with access to this workspace. */
export function TeamPage() {
  const { rows, error } = useList<Member>('control/team', data => data?.members || data?.items || []);
  const [query, setQuery] = usePreference<string>('deploy.team.query', '');
  const visible = (rows || []).filter(row => JSON.stringify(row).toLowerCase().includes(query.toLowerCase()));
  const columns: Column<Member>[] = [
    { id: 'name', label: 'Member', render: row => <ResourceName href={withSticky(resourceHref('team', memberID(row)))} name={row.name || row.email || row.subject || 'Member'} detail={row.owner ? 'Installation owner' : undefined} /> },
    { id: 'role', label: 'Role', render: row => <Badge tone={row.role === 'admin' ? 'accent' : 'neutral'}>{roleLabels[row.role || 'viewer'] || row.role}</Badge> },
    { id: 'email', label: 'Email', render: row => row.email || '—' },
  ];
  return <ResourceIndexPage resource="team" title="Team" description={`Give people the least access needed to operate this control panel. ${roleCopy}`} count={rows?.length} loading={!rows && !error}
    primaryAction={<Button variant="primary" onClick={() => { navigate(createHref('team')); }}><Plus size={16} />Add member</Button>}
    notices={error ? <ErrorBanner>{error}</ErrorBanner> : undefined}
    filters={<SearchField value={query} onChange={event => setQuery(event.target.value)} placeholder="Search members…" aria-label="Search members" />}
    rows={visible} columns={columns} rowKey={row => memberID(row)}
    empty={<EmptyState icon={<Users />} title={query ? 'No matching members' : 'No team members yet'} description={query ? 'Try another name or email.' : 'Add someone to share this control panel. The installation owner is always protected.'} action={query ? undefined : <Button variant="primary" onClick={() => { navigate(createHref('team')); }}>Add member</Button>} />} />;
}

function MemberFields({ member }: { member?: Member }) {
  return <div className="form-grid three"><Field label="Email"><Input name="email" type="email" required autoComplete="email" defaultValue={member?.email} readOnly={Boolean(member)} /></Field><Field label="Name"><Input name="name" autoComplete="name" defaultValue={member?.name} /></Field><Field label="Role" hint={roleCopy}><select name="role" defaultValue={member?.role || 'viewer'}><option value="viewer">Viewer</option><option value="deployer">Deployer</option><option value="admin">Admin</option></select></Field></div>;
}

/** NewMemberPage: the create form of a team member (/team/new). */
export function NewMemberPage() {
  const [error, setError] = useState(''); const notify = useToast();
  const submit = async (event: FormEvent<HTMLFormElement>) => { const values = Object.fromEntries(new FormData(event.currentTarget)); try { await post('control/team', values); notify('Team member saved.'); navigate(indexHref('team')); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } };
  return <ActionForm id="member-form" className="page" data-template="create" data-resource="team" onSubmit={submit}><PageHeader title="Add member" description="They sign in with this email address and get the role you choose." actions={<div className="sticky-actions"><Button type="button" onClick={() => { goBack(indexHref('team')); }}>Cancel</Button><Button type="submit" variant="primary">Save member</Button></div>} />{error && <ErrorBanner>{error}</ErrorBanner>}<Card className="form-card"><SectionHeader title="Member" /><MemberFields /></Card></ActionForm>;
}

/** TeamMemberPage: one member; Overview and Settings (role, Danger zone). */
export function TeamMemberPage({ id, tab }: { id: string; tab: string }) {
  const { rows, error, load } = useList<Member>('control/team', data => data?.members || data?.items || []);
  const member = rows?.find(row => memberID(row) === id);
  const notify = useToast();
  const save = async (event: FormEvent<HTMLFormElement>) => { await post('control/team', Object.fromEntries(new FormData(event.currentTarget))); notify('Team member saved.'); await load(); };
  return <ResourceDetailPage resource="team" id={id} tab={tab} title={member?.name || member?.email || id}
    status={member && <Badge tone={member.role === 'admin' ? 'accent' : 'neutral'}>{roleLabels[member.role || 'viewer'] || member.role}</Badge>}
    facts={member ? [member.email, member.owner && 'Installation owner'] : undefined}
    notices={error ? <ErrorBanner>{error}</ErrorBanner> : undefined}
    missing={!rows && !error ? <Loading label="Loading member…" /> : rows && !member ? <EmptyState icon={<Users />} title="Not a team member" description="This person was removed or never added." /> : undefined}>
    {member && tab === 'overview' && <Card><SectionHeader title="Access" /><SummaryList items={[{ label: 'Name', value: member.name || '—' }, { label: 'Email', value: member.email || '—' }, { label: 'Role', value: roleLabels[member.role || 'viewer'] || member.role }, { label: 'What they can do', value: member.role === 'admin' ? 'Manage infrastructure, settings and people.' : member.role === 'deployer' ? 'Release existing applications.' : 'Inspect status.' }, member.owner && { label: 'Owner', value: 'The installation owner cannot be removed.' }]} /></Card>}
    {member && tab === 'settings' && <div className="settings-stack">
      <Card className="form-card"><SectionHeader title="Role" /><ActionForm onSubmit={save}><MemberFields member={member} /><Cluster><Button type="submit" variant="primary">Save member</Button></Cluster></ActionForm></Card>
      {!member.owner && <DangerZone id={id} description="Removing a member ends their access at once."><DangerZoneRow title="Remove from the team" description="They can no longer sign in to this control panel." action={<ConfirmAction title={`Remove ${member.name || member.email}?`} description="Their access ends at once. Add them again to restore it." confirmLabel="Remove member" destructive onConfirm={async () => { await del(`control/team/${encodeURIComponent(member.subject || member.email || '')}`); notify('Member removed.'); navigate(indexHref('team')); }}><Button variant="danger"><Trash2 size={16} />Remove member</Button></ConfirmAction>} /></DangerZone>}
    </div>}
  </ResourceDetailPage>;
}

/** AuditPage: the index of sensitive and administrative actions. An entry has no detail page. */
export function AuditPage() {
  const { rows, error } = useList<AuditEntry>('control/audit', data => data?.entries || data?.items || []);
  const [query, setQuery] = usePreference<string>('deploy.audit.query', '');
  const visible = (rows || []).filter(row => JSON.stringify(row).toLowerCase().includes(query.toLowerCase()));
  const columns: Column<AuditEntry>[] = [
    { id: 'event', label: 'Event', render: row => <ResourceName name={row.action || row.subject || 'Event'} /> },
    { id: 'actor', label: 'Actor', render: row => row.actor || '—' },
    { id: 'target', label: 'Target', render: row => row.target || row.detail || '—' },
    { id: 'time', label: 'Time', render: row => formatDate(row.created || row.created_at) },
  ];
  return <ResourceIndexPage resource="audit" title="Audit log" description="A durable record of sensitive and administrative actions, newest first." count={rows?.length} loading={!rows && !error}
    actions={<CopyButton value={visible.map(row => [formatDate(row.created || row.created_at), row.actor, row.action || row.subject, row.target || row.detail].filter(Boolean).join(' · ')).join('\n')} label="Copy audit log" />}
    notices={error ? <ErrorBanner>{error}</ErrorBanner> : undefined}
    filters={<SearchField value={query} onChange={event => setQuery(event.target.value)} placeholder="Search activity…" aria-label="Search activity" />}
    rows={visible} columns={columns} rowKey={row => row.id || `${row.created}-${row.action}`}
    empty={<EmptyState icon={<ClipboardList />} title="No activity found" description={query ? 'Try another search.' : 'Administrative actions will appear here.'} />} />;
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

function groupFacts(group: ActivityGroup) {
  const item = group.latest;
  const route = item.route || item.path;
  const status = item.security?.status || item.status;
  const message = item.message || item.error || item.detail;
  return { item, route, status, message, occurredAt: activityTime(item) };
}

function groupDetails(group: ActivityGroup, security: boolean) {
  const { item, route, status, message, occurredAt } = groupFacts(group);
  return JSON.stringify({ title: activityTitle(item, security), first_seen: activityTime(group.first), last_seen: occurredAt, occurrences: group.count, source: item.source, method: item.method, route, status, container: item.container, release: item.release, commit: item.commit, message, stack: item.stack, security: item.security, recent_occurrences: group.items.slice(0, 20).map(event => ({ time: activityTime(event), request_id: event.requestId || event.request_id })) }, null, 2);
}

/** useActivity loads an application's error or security groups. */
function useActivity(appID: string, security: boolean) {
  const [response, setResponse] = useState<ActivityResponse>();
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  const load = async () => {
    if (!appID) return;
    setLoading(true);
    try { const value = await get<ActivityResponse | ActivityItem[]>(`control/apps/${encodeURIComponent(appID)}/${security ? 'security' : 'errors'}`); setResponse(Array.isArray(value) ? { items: value } : value); setError(''); }
    catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); }
    finally { setLoading(false); }
  };
  useEffect(() => { setResponse(undefined); void load(); }, [appID, security]);
  const groups = activityGroups(response?.items || response?.errors || response?.probes || []);
  return { response, groups, error, loading, load };
}

/** ActivityPage: the index of error (or security event) groups of one application. */
export function ActivityPage({ security = false, appID: requested }: { security?: boolean; appID?: string }) {
  const { apps } = useWorkspace();
  const collection = security ? 'security' : 'errors';
  const [savedAppID, setAppID] = usePreference<string>(security ? 'deploy.security.app' : 'deploy.errors.app', '');
  const appID = requested || (apps.some(app => app.id === savedAppID) ? savedAppID : '') || apps[0]?.id || '';
  const [query, setQuery] = usePreference<string>(security ? 'deploy.security.query' : 'deploy.errors.query', '');
  const { response, groups: all, error, loading, load } = useActivity(appID, security);
  const needle = query.trim().toLowerCase();
  const groups = all.filter(group => !needle || group.items.some(item => [item.message, item.error, item.detail, item.stack, item.route, item.path, item.method, item.requestId, item.request_id, item.source, item.container, item.release, item.commit, item.security?.category, item.security?.outcome].some(value => String(value || '').toLowerCase().includes(needle))));
  const collectorStatus = response?.status?.replaceAll('_', ' ');
  const columns: Column<ActivityGroup>[] = [
    { id: 'title', label: security ? 'Event' : 'Error', render: group => { const { item, message } = groupFacts(group); const title = activityTitle(item, security); return <ResourceName href={withSticky(resourceHref(collection, group.key, undefined, { app: appID }))} name={title} detail={message && message !== title ? message : undefined} />; } },
    { id: 'count', label: 'Occurrences', render: group => <Badge tone={security && group.latest.security?.outcome === 'review-response' ? 'warning' : 'neutral'}>{String(group.count)}</Badge> },
    { id: 'request', label: 'Request', render: group => { const { item, route, status } = groupFacts(group); return item.method || route ? <span>{item.method && <b>{item.method}</b>} {route || 'Unknown route'}{status ? ` · HTTP ${status}` : ''}</span> : '—'; } },
    { id: 'source', label: 'Source', render: group => [group.latest.source, group.latest.container].filter(Boolean).join(' · ') || '—' },
    { id: 'seen', label: 'Last seen', render: group => <time dateTime={activityTime(group.latest)}>{formatDate(activityTime(group.latest))}</time> },
  ];
  return <ResourceIndexPage resource={collection} title={security ? 'Security activity' : 'Errors'} description={`${security ? 'Suspicious and rejected traffic' : 'Application failures'} grouped by cause${collectorStatus ? `; collector ${collectorStatus}` : ''}${response?.last_received ? `, last report ${formatDate(response.last_received)}` : ''}${response?.truncated ? ' (newest 500 occurrences)' : ''}.`} count={all.length} loading={loading && !response}
    actions={<Button onClick={() => load()} disabled={loading}><RefreshCw className={loading ? 'spin' : ''} size={16} />Refresh</Button>}
    notices={<>{error && <ErrorBanner>{error}</ErrorBanner>}{response?.collection_error && <Notice><strong>Collector warning:</strong>&nbsp; {response.collection_error}</Notice>}</>}
    filters={<><SearchField value={query} onChange={event => setQuery(event.target.value)} placeholder={`Search ${security ? 'security events' : 'errors'}…`} aria-label={`Search ${security ? 'security events' : 'errors'}`} /><Field label="Application" className="toolbar-filter"><select aria-label="Application" value={appID} onChange={event => { setAppID(event.target.value); navigate(indexHref(collection, { app: event.target.value }), { replace: true }); }}>{apps.map(app => <option key={app.id} value={app.id}>{app.id}</option>)}</select></Field></>}
    rows={groups} columns={columns} rowKey={group => group.key}
    empty={<EmptyState icon={security ? <ShieldCheck /> : <Activity />} title={query ? 'No matching activity' : security ? 'No security events' : 'No application errors'} description={query ? 'Try a different message, route, request ID, release or container.' : 'Nothing requiring attention was reported for this application.'} />} />;
}

/** ActivityDetailPage: one error or security group; Overview and Occurrences. */
export function ActivityDetailPage({ security = false, appID, id, tab }: { security?: boolean; appID: string; id: string; tab: string }) {
  const collection = security ? 'security' : 'errors';
  const { groups, error, response } = useActivity(appID, security);
  const group = groups.find(item => item.key === id);
  const facts = group && groupFacts(group);
  const title = group ? activityTitle(group.latest, security) : id;
  return <ResourceDetailPage resource={collection} id={id} query={{ app: appID }} tab={tab} title={title}
    status={group && <Badge tone={security && group.latest.security?.outcome === 'review-response' ? 'warning' : 'neutral'}>{group.count} {group.count === 1 ? 'occurrence' : 'occurrences'}</Badge>}
    facts={facts ? [<a href={withSticky(resourceHref('applications', appID))}>{appID}</a>, <>Last seen {formatDate(facts.occurredAt)}</>] : undefined}
    actions={group ? <CopyButton value={groupDetails(group, security)} label="Copy event details" /> : undefined}
    notices={error ? <ErrorBanner>{error}</ErrorBanner> : undefined}
    missing={!response && !error ? <Loading label="Loading activity…" /> : response && !group ? <EmptyState icon={security ? <ShieldCheck /> : <Activity />} title="No longer reported" description="This group is not among the latest reports of the application." action={<a className="button button-secondary button-md" href={withSticky(indexHref(collection, { app: appID }))}>Open {security ? 'security activity' : 'errors'}</a>} /> : undefined}>
    {group && facts && tab === 'overview' && <div className="settings-stack">
      <Card><SectionHeader title="Summary" /><SummaryList items={[
        { label: 'First seen', value: <time dateTime={activityTime(group.first)}>{formatDate(activityTime(group.first))}</time> },
        { label: 'Last seen', value: <time dateTime={facts.occurredAt}>{formatDate(facts.occurredAt)}</time> },
        { label: 'Source', value: facts.item.source || 'Unknown' },
        { label: 'Request', value: [facts.item.method, facts.route].filter(Boolean).join(' ') + (facts.status ? ` · HTTP ${facts.status}` : '') || 'Not reported' },
        security && { label: 'Classification', value: [facts.item.security?.category, facts.item.security?.outcome].filter(Boolean).join(' · ') || 'Not reported' },
        facts.item.container && { label: facts.item.container.endsWith('.service') ? 'Service unit' : 'Container', value: facts.item.container },
        facts.item.release && { label: 'Release', value: facts.item.release },
        facts.item.commit && { label: 'Commit', value: facts.item.commit },
      ]} /></Card>
      {facts.message && <Card className="log-card"><LogOutputHeader title="Message" value={facts.message} copyLabel="Copy message" /><pre>{facts.message}</pre></Card>}
      {facts.item.stack && <Card className="log-card"><LogOutputHeader title="Stack trace" value={facts.item.stack} copyLabel="Copy stack trace" /><pre>{facts.item.stack}</pre></Card>}
    </div>}
    {group && tab === 'occurrences' && <Card><SectionHeader title="Recent occurrences" description="The newest 20, with their request IDs." /><ResourceList rows={group.items.slice(0, 20)} rowKey={occurrence => `${occurrence.id || ''}-${activityTime(occurrence)}`} columns={[
      { id: 'time', label: 'Time', render: occurrence => <time dateTime={activityTime(occurrence)}>{formatDate(activityTime(occurrence))}</time> },
      { id: 'request', label: 'Request ID', render: occurrence => { const requestID = occurrence.requestId || occurrence.request_id; return requestID ? <span className="copy-inline"><code>{requestID}</code><CopyButton value={requestID} label="Copy ID" /></span> : <span className="muted-copy">No request ID</span>; } },
      { id: 'release', label: 'Release', render: occurrence => occurrence.release || '—' },
    ]} /></Card>}
  </ResourceDetailPage>;
}

/** HelpPage: guides and the component gallery. */
export function HelpPage({ tab = 'guides' }: { tab?: string }) {
  const [query, setQuery] = useState('');
  const articles = searchHelp(query);
  return <ResourceDetailPage resource="help" tab={tab} title="Help" description="Short operational guides for the workflows you use, and the components the console is built from.">
    {tab === 'guides' && <div className="settings-stack"><Toolbar><SearchField value={query} onChange={event => setQuery(event.target.value)} placeholder="Search help…" aria-label="Search help" /></Toolbar><div className="help-grid">{articles.map(article => <Card key={article.id} id={`help-${article.id}`} data-help-article={article.id}><BookOpen size={20} /><h2>{article.title}</h2>{article.body.map(paragraph => <p key={paragraph}>{paragraph}</p>)}</Card>)}</div>
    {!articles.length && <EmptyState icon={<BookOpen />} title="No matching guide" description="Try another word, such as redirect, delete, move or backup." />}</div>}
    {tab === 'components' && <ComponentGallery />}
  </ResourceDetailPage>;
}

/** AccountPage: the signed-in operator's account; two-factor authentication. */
export function AccountPage({ tab = 'two-factor' }: { tab?: string }) {
  const [status, setStatus] = useState<any>(), [setup, setSetup] = useState<any>(), [codes, setCodes] = useState<string[]>([]), [error, setError] = useState(''); const notify = useToast();
  const load = () => get<any>('v1/auth/two-factor').then(setStatus).catch(reason => setError(reason instanceof Error ? reason.message : String(reason))); useEffect(() => { void load(); }, []);

  const start = async () => { try { setSetup(await post('v1/auth/two-factor/setup')); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } };
  const enable = async (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); try { const result = await post<{ codes: string[] }>('v1/auth/two-factor/enable', { code: new FormData(event.currentTarget).get('code') }); setCodes(result.codes || []); setSetup(undefined); await load(); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } };
  return <ResourceDetailPage resource="account" tab={tab} title="Account" description="Your sign-in security. Two-factor authentication protects credential reveal and just-in-time root console access." status={status && (status.enabled ? <Badge tone="success">Two-factor on</Badge> : <Badge tone="warning">Two-factor off</Badge>)} notices={error ? <ErrorBanner>{error}</ErrorBanner> : undefined}>{!status && !error ? <Loading label="Loading two-factor settings…" /> : codes.length ? <Card className="form-card narrow-form"><SectionHeader title="Save these recovery codes now" description="Each code works once and will not be shown again." /><pre className="recovery-codes">{codes.join('\n')}</pre></Card> : status?.enabled ? <Card className="form-card narrow-form"><SectionHeader title="Authenticator enabled" description="Your account requires a code at sign-in and before sensitive operations." actions={<Badge tone="success"><CheckCircle2 size={13} />Protected</Badge>} /><p><strong>{status.backupCodesLeft || 0}</strong> recovery codes remain.</p></Card> : <Card className="form-card narrow-form"><SectionHeader title="Protect your account" description="Scan a QR code with any TOTP authenticator. You will not need to type the long setup key." />{!setup ? <Button variant="primary" onClick={start}><Smartphone size={16} />Set up authenticator</Button> : <div className="factor-setup">{setup.qrCodeDataUrl && <img src={setup.qrCodeDataUrl} alt="QR code for Līdza Deploy two-factor setup" />}<div><p>Open your authenticator app and scan this QR code.</p><a className="button button-secondary button-md" href={setup.otpauthUrl}>Open authenticator app</a><details><summary>Can’t scan the QR code?</summary><code>{setup.secret}</code></details><ActionForm onSubmit={enable}><Field label="Six-digit code"><Input name="code" inputMode="numeric" autoComplete="one-time-code" required /></Field><Button variant="primary">Verify & enable</Button></ActionForm></div></div>}</Card>}</ResourceDetailPage>;
}
