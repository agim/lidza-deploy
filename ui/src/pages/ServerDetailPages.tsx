import { createHref, goBack, indexHref, navigate, resourceHref, withSticky } from '../lib/router';
import { PortSummary } from '../components/AppPorts';
import { Input } from '../components/primitives/input';
import { Boxes, KeyRound, Plus, RefreshCw, Save, Server, TerminalSquare, Trash2, Unplug, UploadCloud } from 'lucide-react';
import { type FormEvent, useEffect, useState } from 'react';
import { del, get, post, put } from '../lib/api';
import { formatBytes } from '../lib/format';
import type { Application, ContainerInfo, ContainerInventory, Server as ServerType } from '../lib/types';
import { useWorkspace } from '../state/WorkspaceContext';
import { useToast } from '../components/ToastProvider';
import { ActionForm, ActionMenu, Button, Card, ConfirmAction, EmptyState, ErrorBanner, Field, Loading, MenuLabel, MenuLink, Notice, PageHeader, ProgressBar, SectionHeader, StatusBadge } from '../components/ui';
import { Cluster, Stack } from '../components/layout';
import { ServerResources, useServerHealth } from '../components/ServerResources';
import { HostUpdateBadges, ServerUpdates } from '../components/ServerUpdates';
import { ResourceList, type Column } from '../components/ResourceList';
import { ServerTuning } from '../components/ServerTuning';
import { ServerFirewall } from '../components/ServerFirewall';
import { ServerIngress } from '../components/ServerIngress';
import { ServerServiceControl } from '../components/ServerServiceControl';
import { ServerLogAccess } from '../components/ServerLogAccess';
import { RetainedResources } from '../components/RetainedResources';
import { SummaryList } from '../components/SummaryList';
import { DangerZone, DangerZoneRow, ResourceDetailPage, ResourceName } from '../components/resource';
import { ServerCleanup } from './ServerCleanupPage';
import { ServerConsole } from './ServerConsolePage';
import { ServerAgentUpgrade, ServerSSHAccess } from './ResourceDetailPages';
import { serverStatus } from './ServersPage';
import { appRuntime, appState, releaseLabel, runtimeLabel } from './ApplicationsPage';

/** The agent connection fields, shared by connecting a new server and editing an existing one. */
function ConnectionFields({ server, isNew }: { server?: ServerType; isNew: boolean }) {
  return <><div className="form-grid two"><Field label="Server ID" hint="Permanent lowercase identifier."><Input name="id" required readOnly={!isNew} defaultValue={server?.id} pattern="[a-z][a-z0-9-]{0,47}" placeholder="production-eu" autoComplete="off" spellCheck={false} /></Field><Field label="Display name"><Input name="name" required defaultValue={server?.name} placeholder="Production · Frankfurt" autoComplete="off" /></Field></div><Field label="Agent URL"><Input name="url" type="url" required defaultValue={server?.url} placeholder="https://agent.example.com" autoComplete="url" spellCheck={false} /></Field><Field label="Agent token" hint={isNew ? 'Use the token printed by the agent installer.' : 'Leave blank to keep the saved credential.'}><Input name="token" type="password" required={isNew} minLength={32} autoComplete="new-password" placeholder={isNew ? 'Paste agent token' : 'Saved credential'} /></Field></>;
}

function useSaveConnection(serverID: string, isNew: boolean) {
  const { refresh } = useWorkspace(); const notify = useToast(); const [error, setError] = useState(''); const [busy, setBusy] = useState(false);
  const submit = async (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); setBusy(true); try { const values = Object.fromEntries(new FormData(event.currentTarget)); const body = { id: values.id, name: values.name, url: values.url, token: values.token }; isNew ? await post('control/servers', body) : await put(`control/servers/${encodeURIComponent(serverID)}`, body); notify(isNew ? 'Server connected.' : 'Server connection updated.'); await refresh(); navigate(isNew ? resourceHref('servers', String(values.id)) : resourceHref('servers', serverID, 'agent')); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setBusy(false); } };
  return { submit, error, busy };
}

/**
 * ServerDetailPage: one server. Its properties are tabs: overview, the
 * applications and processes on it, host management (ingress, updates,
 * tuning, firewall, cleanup, retained data), operator access (console, SSH)
 * and the agent (release, connection, Danger zone).
 */
export function ServerDetailPage({ serverID, tab }: { serverID: string; tab: string }) {
  const { servers, apps, loading } = useWorkspace();
  const server = servers.find(item => item.id === serverID);
  const metrics = useServerHealth(server ? serverID : '');
  const health = metrics.health.find(row => row.server_id === serverID);
  const hosted = apps.filter(app => app.server_id === serverID);
  const link = (next: string) => withSticky(resourceHref('servers', serverID, next));
  return <ResourceDetailPage resource="servers" id={serverID} tab={tab} title={server?.name || serverID}
    status={server && <StatusBadge status={serverStatus(server, health, metrics.unavailable, metrics.busy)} />}
    facts={server ? [<code>{server.url}</code>, <a href={link('applications')}>{hosted.length} application{hosted.length === 1 ? '' : 's'}</a>, health?.cpus ? `${health.cpus} CPUs` : undefined] : undefined}
    actions={server ? <><ActionMenu label="Actions"><MenuLabel>Operate</MenuLabel><MenuLink icon={<TerminalSquare size={16} />} href={link('console')}>Server console</MenuLink><MenuLink icon={<KeyRound size={16} />} href={link('ssh')}>SSH access</MenuLink><MenuLink icon={<Trash2 size={16} />} href={link('cleanup')}>Disk cleanup</MenuLink><MenuLink icon={<UploadCloud size={16} />} href={link('agent')}>Upgrade agent</MenuLink></ActionMenu><Button variant="primary" onClick={() => { navigate(createHref('applications', { server: serverID })); }}><Plus size={16} />Add application</Button></> : undefined}
    missing={!loading && !server ? <EmptyState icon={<Server />} title="Server not found" description={`No server called ${serverID} is connected to this control panel.`} action={<Button variant="primary" onClick={() => { navigate(createHref('servers')); }}>Connect server</Button>} /> : undefined}>
    {server && <>
      {tab === 'overview' && <ServerOverview server={server} />}
      {tab === 'applications' && <ServerApplications serverID={serverID} />}
      {tab === 'processes' && <ServerProcesses serverID={serverID} />}
      {tab === 'ingress' && <ServerIngress serverID={serverID} />}
      {tab === 'updates' && <ServerUpdates serverID={serverID} />}
      {tab === 'tuning' && <ServerTuning serverID={serverID} />}
      {tab === 'firewall' && <ServerFirewall serverID={serverID} />}
      {tab === 'cleanup' && <ServerCleanup serverID={serverID} />}
      {tab === 'retained' && <RetainedResources serverID={serverID} />}
      {tab === 'console' && <ServerConsole serverID={serverID} />}
      {tab === 'ssh' && <ServerSSHAccess serverID={serverID} />}
      {tab === 'agent' && <ServerAgent server={server} />}
    </>}
  </ResourceDetailPage>;
}

/** ConnectServerPage: the create form of a server (/servers/new). */
export function ConnectServerPage() {
  const { submit, error, busy } = useSaveConnection('new', true);
  return <ActionForm id="server-form" className="page" data-template="create" data-resource="servers" onSubmit={submit}><PageHeader title="Connect server" description="Add an installed hosting agent to this control panel." actions={<div className="sticky-actions"><Button type="button" onClick={() => { goBack(indexHref('servers')); }}>Cancel</Button><Button type="submit" variant="primary" disabled={busy}>{busy ? 'Saving…' : 'Save connection'}</Button></div>} />{error && <ErrorBanner>{error}</ErrorBanner>}<Card className="form-card narrow-form"><SectionHeader title="Agent connection" description="The token is sent only when you save it and is never returned to the browser." /><ConnectionFields isNew /></Card></ActionForm>;
}

function ServerConnectionCard({ serverID, server }: { serverID: string; server?: ServerType }) {
  const { submit, error, busy } = useSaveConnection(serverID, false);
  return <Card className="form-card" data-server-connection={serverID}><SectionHeader title="Agent connection" description="The token is sent only when you save it and is never returned to the browser." />{error && <ErrorBanner>{error}</ErrorBanner>}<ActionForm id="server-form" onSubmit={submit}><ConnectionFields server={server} isNew={false} /><Cluster><Button type="submit" variant="primary" disabled={busy}><Save size={16} />{busy ? 'Saving…' : 'Save connection'}</Button></Cluster></ActionForm></Card>;
}

/** Overview tab: live resources and what runs on the server. */
function ServerOverview({ server }: { server: ServerType }) {
  const { apps } = useWorkspace();
  const metrics = useServerHealth(server.id);
  const hosted = apps.filter(app => app.server_id === server.id);
  return <Stack gap="page" data-server-overview={server.id}>
    <Card className="server-health-card" data-server-health={server.id}><ServerResources health={metrics.health.find(row => row.server_id === server.id)} loading={metrics.busy} unavailable={metrics.error || (metrics.unavailable.includes(server.name) || metrics.unavailable.includes(server.id) ? 'Hosting agent unavailable.' : undefined)} /></Card>
    <Card><SectionHeader title="On this server" actions={<HostUpdateBadges serverID={server.id} />} /><SummaryList items={[{ label: 'Applications', value: <a href={withSticky(resourceHref('servers', server.id, 'applications'))}>{hosted.length ? hosted.map(app => app.id).join(', ') : 'None yet'}</a> }, { label: 'Agent', value: <code>{server.url}</code> }]} /></Card>
  </Stack>;
}

/** Applications tab: the applications this server runs. */
function ServerApplications({ serverID }: { serverID: string }) {
  const { apps, latestDeployment } = useWorkspace();
  const rows = apps.filter(app => app.server_id === serverID);
  const columns: Column<Application>[] = [
    { id: 'name', label: 'Name', render: app => <ResourceName href={withSticky(resourceHref('applications', app.id))} name={app.id} detail={app.domain} /> },
    { id: 'status', label: 'Status', render: app => <StatusBadge status={appState(app, latestDeployment(app.id))} /> },
    { id: 'runtime', label: 'Runtime', render: app => runtimeLabel(appRuntime(app)) },
    { id: 'release', label: 'Release', render: app => <code>{releaseLabel(app)}</code> },
  ];
  return <Card><SectionHeader title="Applications" description="Each keeps its own hostname, runtime and data." actions={<Button size="sm" variant="primary" onClick={() => { navigate(createHref('applications', { server: serverID })); }}><Plus size={15} />Add application</Button>} /><ResourceList rows={rows} columns={columns} rowKey={app => app.id} empty={<EmptyState icon={<Server />} title="No applications yet" description="Add an application to deploy it on this server." />} /></Card>;
}

/** Agent tab: the agent release, its connection, service control of adopted applications, system log access and the Danger zone. */
function ServerAgent({ server }: { server: ServerType }) {
  const { apps, refresh } = useWorkspace(); const notify = useToast();
  const hosted = apps.filter(app => app.server_id === server.id);
  return <Stack gap="page">
    <ServerAgentUpgrade serverID={server.id} />
    <ServerConnectionCard serverID={server.id} server={server} />
    <ServerServiceControl serverID={server.id} />
    <ServerLogAccess serverID={server.id} />
    <DangerZone id={server.id} description="Disconnecting affects only this control panel.">
      <DangerZoneRow title="Disconnect this server" description={hosted.length ? 'Move or remove hosted applications before disconnecting this server.' : 'This removes the control-panel connection, not the agent installation.'} action={<ConfirmAction title={`Disconnect ${server.name || server.id}?`} description="The control panel will no longer manage this agent." confirmLabel="Disconnect server" destructive onConfirm={async () => { await del(`control/servers/${encodeURIComponent(server.id)}`); notify('Server disconnected.'); await refresh(); navigate(indexHref('servers')); }}><Button type="button" variant="danger" disabled={hosted.length > 0}><Unplug size={16} />Disconnect server</Button></ConfirmAction>} />
    </DangerZone>
  </Stack>;
}

/** The confirmation text of restarting one process row. */
function restartDescription(row: ContainerInfo) {
  if (row.runtime === 'native') return 'systemd stops and starts this service. Requests it is serving may be interrupted; the application\'s other processes keep running.';
  return row.role?.startsWith('Stack ·') ? 'This queues a coordinated reload of the entire stack, preserving its shared network and persistent data. The application will be briefly unavailable.' : 'Docker will stop and start this container. The application or managed service may be briefly unavailable.';
}

/** ServerProcesses: the Processes tab, live containers and native services grouped by application. */
export function ServerProcesses({ serverID }: { serverID: string }) {
  const { apps } = useWorkspace(); const [containers, setContainers] = useState<ContainerInfo[]>([]); const [checkedAt, setCheckedAt] = useState(''); const [warning, setWarning] = useState(''); const [loading, setLoading] = useState(true); const [error, setError] = useState(''); const notify = useToast();
  const load = async () => { try { const value = await get<ContainerInfo[] | ContainerInventory>(`control/servers/${encodeURIComponent(serverID)}/containers`); setContainers(Array.isArray(value) ? value : value.containers || []); setCheckedAt(Array.isArray(value) ? '' : value.checked_at || ''); setWarning(Array.isArray(value) ? '' : value.warning || ''); setError(''); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setLoading(false); } };
  useEffect(() => { load(); const timer = setInterval(load, 10_000); return () => clearInterval(timer); }, [serverID]);
  // Native services restart through the application's service control,
  // which confirms the application by name; containers through Docker.
  const restart = async (row: ContainerInfo) => {
    if (row.runtime === 'native') {
      const app = apps.find(item => item.id === row.app_id);
      await post(`control/native-apps/${encodeURIComponent(row.app_id || '')}/services/${encodeURIComponent(row.unit || row.name)}/restart`, { confirm: app?.native?.name || row.app_id });
    } else await post(`control/servers/${encodeURIComponent(serverID)}/containers/restart`, { name: row.name });
    notify(`${row.name} restart requested.`); await load();
  };
  const percent = (value?: string | number) => { const parsed = typeof value === 'number' ? value : Number.parseFloat(value || ''); return Number.isFinite(parsed) ? parsed : 0; };
  const columns: Column<ContainerInfo>[] = [
    { id: 'process', label: 'Process', render: row => <div className="container-name">{row.runtime === 'native' ? <Server size={17} /> : <Boxes size={17} />}<div><strong>{row.name}</strong><small>{row.app_id ? `${row.app_id}${row.role ? ` · ${row.role}` : ''}` : row.service ? `Service · ${row.service}` : row.role || row.image}</small>{row.runtime === 'native' && <small>{[row.since && row.state === 'running' ? `Since ${new Date(row.since).toLocaleString()}` : '', typeof row.restart_count === 'number' ? `${row.restart_count} restart${row.restart_count === 1 ? '' : 's'}` : '', row.pids ? `${row.pids} tasks` : ''].filter(Boolean).join(' · ')}</small>}</div></div> },
    { id: 'status', label: 'Status', render: row => <div><StatusBadge status={row.health || row.state || row.status || 'unknown'} />{row.public_tls && <div className="service-certificate muted-copy"><span>Service TLS: {row.public_tls.status === 'ready' ? 'certificate ready' : row.public_tls.status === 'pending' ? 'waiting for certificate' : 'certificate needs attention'}</span><small>{row.public_tls.hostname}{row.public_tls.not_after ? ` · expires ${new Date(row.public_tls.not_after).toLocaleDateString()}` : ''}</small>{row.public_tls.warning && <small>{row.public_tls.warning}</small>}</div>}</div> },
    { id: 'ports', label: 'Forwarded ports', render: row => row.ports?.length ? <PortSummary ports={row.ports} /> : <span className="muted-copy">{row.runtime === 'native' ? 'Served through the web server' : 'None reported'}</span> },
    { id: 'cpu', label: 'CPU', render: row => { if (row.cpu === undefined && row.cpu_percent === undefined) return <span className="muted-copy">Not measured</span>; const usage = percent(row.cpu ?? row.cpu_percent); return <div className="usage-cell"><span>{row.cpu || `${usage.toFixed(1)}%`}</span><ProgressBar value={Math.min(usage, 100)} /></div>; } },
    { id: 'memory', label: 'Memory', render: row => { if (!row.memory && row.memory_bytes === undefined) return <span className="muted-copy">Not measured</span>; const usage = percent(row.memory_percent) || (row.memory_limit_bytes ? (row.memory_bytes || 0) / row.memory_limit_bytes * 100 : 0); return <div className="usage-cell"><span>{row.memory || `${formatBytes(row.memory_bytes)}${row.memory_limit_bytes ? ` / ${formatBytes(row.memory_limit_bytes)}` : ''}`}</span>{(row.memory_percent || row.memory_limit_bytes) && <ProgressBar value={usage} tone={usage > 90 ? 'danger' : 'accent'} />}</div>; } },
    { id: 'actions', label: 'Actions', render: row => row.restartable === false ? <span className="muted-copy">{row.runtime === 'native' ? 'Service control off' : 'Protected'}</span> : <ConfirmAction title={`Restart ${row.name}?`} description={restartDescription(row)} confirmLabel={row.runtime === 'native' ? 'Restart service' : 'Restart container'} onConfirm={() => restart(row)}><Button size="sm"><RefreshCw size={15} />Restart</Button></ConfirmAction> },
  ];
  const groups = [...containers.reduce((groups, row) => { const key = row.app_id || ''; const rows = groups.get(key) || []; rows.push(row); groups.set(key, rows); return groups; }, new Map<string, ContainerInfo[]>())].sort(([a], [b]) => a === '' ? 1 : b === '' ? -1 : a.localeCompare(b));
  const count = (rows: ContainerInfo[]) => { const services = rows.filter(row => row.runtime === 'native').length; const boxes = rows.length - services; return [boxes ? `${boxes} container${boxes === 1 ? '' : 's'}` : '', services ? `${services} service${services === 1 ? '' : 's'}` : ''].filter(Boolean).join(' · '); };
  return <div className="settings-stack"><Cluster><Button onClick={load}><RefreshCw size={16} />Refresh</Button></Cluster>{error && <ErrorBanner>{error}</ErrorBanner>}{warning && <Notice>{warning}</Notice>}<p className="muted-copy">Live usage sampled by the hosting agent: containers from Docker, native services from systemd, grouped by application. Rows keep their order while values update. Persistent volumes and application files are not removed.{checkedAt ? ` Sampled ${new Date(checkedAt).toLocaleTimeString()}.` : ''}</p>{loading ? <Loading label="Loading processes…" /> : <div className="settings-stack">{groups.length ? groups.map(([appID, rows]) => <Card key={appID} data-app-containers={appID || '__unassigned'}><SectionHeader title={appID || 'Unassigned containers'} description={`${count(rows)}${apps.find(app => app.id === appID)?.domain ? ` · ${apps.find(app => app.id === appID)?.domain}` : ''}`} actions={appID && apps.some(app => app.id === appID) ? <a className="button button-secondary button-sm" href={withSticky(resourceHref('applications', appID))}>Open application</a> : undefined} /><ResourceList rows={rows} columns={columns} rowKey={row => row.id || row.name} /></Card>) : <Card><p>No processes reported by this server.</p></Card>}</div>}</div>;
}



