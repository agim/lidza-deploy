import { PortSummary } from '../components/AppPorts';
import { Input } from '../components/primitives/input';
import { AlertTriangle, Boxes, Cpu, MemoryStick, Pause, Play, RefreshCw, Server, TerminalSquare } from 'lucide-react';
import { type FormEvent, useEffect, useRef, useState } from 'react';
import { del, get, post, put } from '../lib/api';
import { formatBytes } from '../lib/format';
import type { ContainerInfo, ContainerInventory, Deployment, Server as ServerType } from '../lib/types';
import { useWorkspace } from '../state/WorkspaceContext';
import { useToast } from '../components/ToastProvider';
import { ActionForm, Button, Card, ConfirmAction, LogOutputHeader, ErrorBanner, Field, Loading, Notice, PageHeader, ProgressBar, SectionHeader, StatusBadge } from '../components/ui';
import { ResourceList, type Column } from '../components/ResourceList';

export function ServerSettingsPage({ serverID }: { serverID: string }) {
  const { servers, apps, refresh } = useWorkspace(); const existing = servers.find(item => item.id === serverID); const isNew = serverID === 'new'; const [error, setError] = useState(''); const [busy, setBusy] = useState(false); const notify = useToast();
  const submit = async (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); setBusy(true); try { const values = Object.fromEntries(new FormData(event.currentTarget)); const body = { id: values.id, name: values.name, url: values.url, token: values.token }; isNew ? await post('control/servers', body) : await put(`control/servers/${encodeURIComponent(serverID)}`, body); notify(isNew ? 'Server connected.' : 'Server connection updated.'); await refresh(); location.hash = 'servers'; } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setBusy(false); } };
  return <ActionForm id="server-form" onSubmit={submit}><PageHeader eyebrow="Server management" title={isNew ? 'Connect server' : existing?.name || serverID} description={isNew ? 'Add an installed hosting agent to this workspace.' : existing?.url} back="#servers" actions={<div className="sticky-actions"><Button type="button" onClick={() => { location.hash = 'servers'; }}>Cancel</Button><Button type="submit" variant="primary" disabled={busy}>{busy ? 'Saving…' : 'Save connection'}</Button></div>} />{error && <ErrorBanner>{error}</ErrorBanner>}<Card className="form-card narrow-form"><SectionHeader title="Agent connection" description="The token is sent only when you save it and is never returned to the browser." /><div className="form-grid two"><Field label="Server ID" hint="Permanent lowercase identifier."><Input name="id" required readOnly={!isNew} defaultValue={existing?.id} pattern="[a-z][a-z0-9-]{0,47}" placeholder="production-eu" /></Field><Field label="Display name"><Input name="name" required defaultValue={existing?.name} placeholder="Production · Frankfurt" /></Field></div><Field label="Agent URL"><Input name="url" type="url" required defaultValue={existing?.url} placeholder="https://agent.example.com" /></Field><Field label="Agent token" hint={isNew ? 'Use the token printed by the agent installer.' : 'Leave blank to keep the saved credential.'}><Input name="token" type="password" required={isNew} minLength={32} autoComplete="new-password" placeholder={isNew ? 'Paste agent token' : 'Saved credential'} /></Field></Card>{!isNew && <Card className="danger-zone"><SectionHeader title="Disconnect server" description={apps.some(app => app.server_id === serverID) ? 'Move or remove hosted applications before disconnecting this server.' : 'This removes the control-panel connection, not the agent installation.'} /><ConfirmAction title={`Disconnect ${existing?.name || serverID}?`} description="The control panel will no longer manage this agent." confirmLabel="Disconnect server" destructive onConfirm={async () => { await del(`control/servers/${encodeURIComponent(serverID)}`); notify('Server disconnected.'); await refresh(); location.hash = 'servers'; }}><Button type="button" variant="danger" disabled={apps.some(app => app.server_id === serverID)}>Disconnect server</Button></ConfirmAction></Card>}</ActionForm>;
}

export function ContainersPage({ serverID }: { serverID: string }) {
  const { servers, apps } = useWorkspace(); const server = servers.find(item => item.id === serverID); const [containers, setContainers] = useState<ContainerInfo[]>([]); const [checkedAt, setCheckedAt] = useState(''); const [warning, setWarning] = useState(''); const [loading, setLoading] = useState(true); const [error, setError] = useState(''); const notify = useToast();
  const load = async () => { try { const value = await get<ContainerInfo[] | ContainerInventory>(`control/servers/${encodeURIComponent(serverID)}/containers`); setContainers(Array.isArray(value) ? value : value.containers || []); setCheckedAt(Array.isArray(value) ? '' : value.checked_at || ''); setWarning(Array.isArray(value) ? '' : value.warning || ''); setError(''); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setLoading(false); } };
  useEffect(() => { load(); const timer = setInterval(load, 10_000); return () => clearInterval(timer); }, [serverID]);
  const restart = async (name: string) => { await post(`control/servers/${encodeURIComponent(serverID)}/containers/restart`, { name }); notify(`${name} restart requested.`); await load(); };
  const percent = (value?: string | number) => { const parsed = typeof value === 'number' ? value : Number.parseFloat(value || ''); return Number.isFinite(parsed) ? parsed : 0; };
  const columns: Column<ContainerInfo>[] = [
    { id: 'container', label: 'Container', render: row => <div className="container-name"><Boxes size={17} /><div><strong>{row.name}</strong><small>{row.app_id ? `${row.app_id}${row.role ? ` · ${row.role}` : ''}` : row.service ? `Service · ${row.service}` : row.role || row.image}</small></div></div> },
    { id: 'status', label: 'Status', render: row => <div><StatusBadge status={row.health || row.state || row.status || 'unknown'} />{row.public_tls && <div className="service-certificate muted-copy"><span>Service TLS: {row.public_tls.status === 'ready' ? 'certificate ready' : row.public_tls.status === 'pending' ? 'waiting for certificate' : 'certificate needs attention'}</span><small>{row.public_tls.hostname}{row.public_tls.not_after ? ` · expires ${new Date(row.public_tls.not_after).toLocaleDateString()}` : ''}</small>{row.public_tls.warning && <small>{row.public_tls.warning}</small>}</div>}</div> },
    { id: 'ports', label: 'Forwarded ports', render: row => row.ports?.length ? <PortSummary ports={row.ports} /> : <span className="muted-copy">None reported</span> },
    { id: 'cpu', label: 'CPU', render: row => { const usage = percent(row.cpu ?? row.cpu_percent); return <div className="usage-cell"><span>{row.cpu || `${usage.toFixed(1)}%`}</span><ProgressBar value={usage} /></div>; } },
    { id: 'memory', label: 'Memory', render: row => { const usage = percent(row.memory_percent) || (row.memory_limit_bytes ? (row.memory_bytes || 0) / row.memory_limit_bytes * 100 : 0); return <div className="usage-cell"><span>{row.memory || `${formatBytes(row.memory_bytes)} / ${formatBytes(row.memory_limit_bytes)}`}</span><ProgressBar value={usage} tone={usage > 90 ? 'danger' : 'accent'} /></div>; } },
    { id: 'actions', label: 'Actions', render: row => row.restartable === false ? <span className="muted-copy">Protected</span> : <ConfirmAction title={`Restart ${row.name}?`} description={row.role?.startsWith("Stack ·") ? "This queues a coordinated reload of the entire stack, preserving its shared network and persistent data. The application will be briefly unavailable." : "Docker will stop and start this container. The application or managed service may be briefly unavailable."} confirmLabel="Restart container" onConfirm={() => restart(row.name)}><Button size="sm"><RefreshCw size={15} />Restart</Button></ConfirmAction> },
  ];
  const groups = [...containers.reduce((groups, row) => { const key = row.app_id || ''; const rows = groups.get(key) || []; rows.push(row); groups.set(key, rows); return groups; }, new Map<string, ContainerInfo[]>())].sort(([a], [b]) => a === '' ? 1 : b === '' ? -1 : a.localeCompare(b));
  return <><PageHeader eyebrow="Server containers" title={server?.name || serverID} description="Live process usage sampled by the hosting agent. Rows stay in a stable order while values update." back="#servers" actions={<Button onClick={load}><RefreshCw size={16} />Refresh</Button>} />{error && <ErrorBanner>{error}</ErrorBanner>}{warning && <Notice>{warning}</Notice>}<p className="muted-copy">Containers are grouped by application. Persistent volumes are not removed.{checkedAt ? ` Sampled ${new Date(checkedAt).toLocaleTimeString()}.` : ''}</p>{loading ? <Loading label="Loading containers…" /> : <div className="settings-stack">{groups.length ? groups.map(([appID, rows]) => <Card key={appID} data-app-containers={appID || '__unassigned'}><SectionHeader title={appID || 'Unassigned containers'} description={`${rows.length} container${rows.length === 1 ? '' : 's'}${apps.find(app => app.id === appID)?.domain ? ` · ${apps.find(app => app.id === appID)?.domain}` : ''}`} actions={appID && apps.some(app => app.id === appID) ? <a className="button button-secondary" href={`#page/settings/${encodeURIComponent(appID)}`}>App settings</a> : undefined} /><ResourceList rows={rows} columns={columns} rowKey={row => row.id || row.name} /></Card>) : <Card><p>No containers reported by this server.</p></Card>}</div>}</>;
}

export { ServerConsolePage } from './ServerConsolePage';

export function LogPage({ kind, id }: { kind: 'build-logs' | 'runtime-logs'; id: string }) {
  const { deployments } = useWorkspace();
  const initial = deployments.find(item => item.id === id);
  const [deployment, setDeployment] = useState<Deployment | undefined>(initial);
  const [output, setOutput] = useState(initial?.log || initial?.error || '');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(true);
  const [paused, setPaused] = useState(false);
  const [follow, setFollow] = useState(true);
  const [refreshKey, setRefreshKey] = useState(0);
  const [manualRefreshing, setManualRefreshing] = useState(false);
  const outputRef = useRef<HTMLPreElement>(null);
  const terminal = kind === 'build-logs' && !!deployment && !['queued', 'building'].includes(deployment.status);

  useEffect(() => {
    let active = true;
    let timer: number | undefined;
    const schedule = (finished = false) => {
      if (active && !paused && !finished) timer = window.setTimeout(load, 1_500);
    };
    const load = async () => {
      try {
        if (kind === 'build-logs') {
          const current = await get<Deployment>(`control/deployments/${encodeURIComponent(id)}`);
          if (!active) return;
          setDeployment(current);
          setOutput(current.log || current.error || '');
          setError(''); setLoading(false);
          schedule(!['queued','building'].includes(current.status));
          return;
        } else {
          const value = await get<{ logs?: string }>(`control/apps/${encodeURIComponent(id)}/logs`);
          if (!active) return;
          if (value.logs) setOutput(value.logs);
          setError('');
        }
      } catch (reason) {
        if (!active) return;
        setError(`Live refresh failed: ${reason instanceof Error ? reason.message : String(reason)}. Retrying…`);
      } finally { if (active) setManualRefreshing(false); }
      if (active) setLoading(false);
      schedule(false);
    };
    load();
    return () => { active = false; if (timer) clearTimeout(timer); };
  }, [kind, id, paused, refreshKey]);

  useEffect(() => {
    const element = outputRef.current;
    if (follow && element) element.scrollTop = element.scrollHeight;
  }, [output, follow]);

  const failure = kind === 'build-logs' ? deployment?.error : undefined;
  const visibleOutput = failure && output && !output.includes(failure) ? `${output}\n\nFailure: ${failure}` : output || (terminal ? 'No build output was recorded.' : kind === 'build-logs' ? 'Waiting for build output…' : 'Waiting for runtime output…');
  const stateLabel = kind === 'runtime-logs' ? `${paused ? 'Paused' : 'Live'} · runtime` : terminal ? `Finished · ${deployment?.status}` : paused ? `Paused · ${deployment?.status || 'waiting'}` : `Live · ${deployment?.status || 'waiting'}`;
  const status = kind === 'runtime-logs' ? loading ? 'connecting' : 'running' : deployment?.status || (loading ? 'connecting' : 'waiting');
  return <><PageHeader eyebrow={kind === 'build-logs' ? 'Deployment output' : 'Application output'} title={kind === 'build-logs' ? `Build ${id.slice(0, 12)}` : `${id} runtime logs`} description={terminal ? 'This deployment has finished. Its complete output remains available below.' : 'New output appears here automatically without replacing the page or moving your reading position.'} back={kind === 'build-logs' ? '#deployments' : '#applications'} actions={<div className="sticky-actions"><Button id="log-pause" onClick={() => setPaused(value => !value)} disabled={terminal}>{paused ? <Play size={15} /> : <Pause size={15} />}{paused ? 'Resume live output' : 'Pause live output'}</Button><Button size="sm" feedback pending={manualRefreshing} onClick={() => { setManualRefreshing(true); setRefreshKey(value => value + 1); }}><RefreshCw size={15} />Refresh</Button></div>} />{failure && <div id="deployment-failure"><ErrorBanner>{failure}</ErrorBanner></div>}{error && <div id="deployment-refresh-error"><ErrorBanner>{error}</ErrorBanner></div>}<Card className="log-status-card"><div><StatusBadge status={status} /><strong id="deployment-state" aria-live="polite">{stateLabel}</strong>{deployment && <span>{Math.round(deployment.duration_seconds || 0)} seconds</span>}</div><label className="log-follow-control"><Input id="log-follow" type="checkbox" checked={follow} onChange={event => setFollow(event.target.checked)} /><span>Follow latest output</span></label></Card><Card className="log-card"><LogOutputHeader title={kind === 'build-logs' ? 'Build log' : 'Runtime log'} value={visibleOutput} copyLabel={kind === 'build-logs' ? 'Copy build log' : 'Copy runtime log'} />{loading && !output ? <Loading label={`Connecting to live ${kind === 'build-logs' ? 'build' : 'runtime'} output…`} /> : <pre id="deployment-output" ref={outputRef} onScroll={event => { const element = event.currentTarget; if (follow && element.scrollHeight - element.scrollTop - element.clientHeight > 28) setFollow(false); }}>{visibleOutput}</pre>}</Card></>;
}
