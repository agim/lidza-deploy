import { AlertTriangle, Boxes, Cpu, KeyRound, MemoryStick, Pause, Play, RefreshCw, Server, TerminalSquare } from 'lucide-react';
import { type FormEvent, useEffect, useRef, useState } from 'react';
import { del, get, post, put } from '../lib/api';
import { formatBytes } from '../lib/format';
import type { ContainerInfo, Deployment, Server as ServerType } from '../lib/types';
import { useWorkspace } from '../state/WorkspaceContext';
import { useToast } from '../components/ToastProvider';
import { Button, Card, ConfirmAction, CopyButton, ErrorBanner, Field, Loading, PageHeader, ProgressBar, SectionHeader, StatusBadge } from '../components/ui';
import { ResourceList, type Column } from '../components/ResourceList';

export function ServerSettingsPage({ serverID }: { serverID: string }) {
  const { servers, apps, refresh } = useWorkspace(); const existing = servers.find(item => item.id === serverID); const isNew = serverID === 'new'; const [error, setError] = useState(''); const [busy, setBusy] = useState(false); const notify = useToast();
  const submit = async (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); setBusy(true); try { const values = Object.fromEntries(new FormData(event.currentTarget)); const body = { id: values.id, name: values.name, url: values.url, token: values.token }; isNew ? await post('control/servers', body) : await put(`control/servers/${encodeURIComponent(serverID)}`, body); notify(isNew ? 'Server connected.' : 'Server connection updated.'); await refresh(); location.hash = 'servers'; } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setBusy(false); } };
  return <form id="server-form" onSubmit={submit}><PageHeader eyebrow="Server management" title={isNew ? 'Connect server' : existing?.name || serverID} description={isNew ? 'Add an installed hosting agent to this workspace.' : existing?.url} back="#servers" actions={<div className="sticky-actions"><Button type="button" onClick={() => { location.hash = 'servers'; }}>Cancel</Button><Button type="submit" variant="primary" disabled={busy}>{busy ? 'Saving…' : 'Save connection'}</Button></div>} />{error && <ErrorBanner>{error}</ErrorBanner>}<Card className="form-card narrow-form"><SectionHeader title="Agent connection" description="The token is sent only when you save it and is never returned to the browser." /><div className="form-grid two"><Field label="Server ID" hint="Permanent lowercase identifier."><input name="id" required readOnly={!isNew} defaultValue={existing?.id} pattern="[a-z][a-z0-9-]{0,47}" placeholder="production-eu" /></Field><Field label="Display name"><input name="name" required defaultValue={existing?.name} placeholder="Production · Frankfurt" /></Field></div><Field label="Agent URL"><input name="url" type="url" required defaultValue={existing?.url} placeholder="https://agent.example.com" /></Field><Field label="Agent token" hint={isNew ? 'Use the token printed by the agent installer.' : 'Leave blank to keep the saved credential.'}><input name="token" type="password" required={isNew} minLength={32} autoComplete="new-password" placeholder={isNew ? 'Paste agent token' : 'Saved credential'} /></Field></Card>{!isNew && <Card className="danger-zone"><SectionHeader title="Disconnect server" description={apps.some(app => app.server_id === serverID) ? 'Move or remove hosted applications before disconnecting this server.' : 'This removes the control-panel connection, not the agent installation.'} /><ConfirmAction title={`Disconnect ${existing?.name || serverID}?`} description="The control panel will no longer manage this agent." confirmLabel="Disconnect server" destructive onConfirm={async () => { await del(`control/servers/${encodeURIComponent(serverID)}`); notify('Server disconnected.'); await refresh(); location.hash = 'servers'; }}><Button type="button" variant="danger" disabled={apps.some(app => app.server_id === serverID)}>Disconnect server</Button></ConfirmAction></Card>}</form>;
}

export function ContainersPage({ serverID }: { serverID: string }) {
  const { servers } = useWorkspace(); const server = servers.find(item => item.id === serverID); const [containers, setContainers] = useState<ContainerInfo[]>([]); const [loading, setLoading] = useState(true); const [error, setError] = useState(''); const notify = useToast();
  const load = async () => { try { const value = await get<ContainerInfo[] | { containers?: ContainerInfo[] }>(`control/servers/${encodeURIComponent(serverID)}/containers`); setContainers(Array.isArray(value) ? value : value.containers || []); setError(''); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setLoading(false); } };
  useEffect(() => { load(); const timer = setInterval(load, 10_000); return () => clearInterval(timer); }, [serverID]);
  const restart = async (name: string) => { try { await post(`control/servers/${encodeURIComponent(serverID)}/containers/restart`, { name }); notify(`${name} restart requested.`); await load(); } catch (reason) { notify(reason instanceof Error ? reason.message : String(reason), true); } };
  const columns: Column<ContainerInfo>[] = [
    { id: 'container', label: 'Container', render: row => <div className="container-name"><Boxes size={17} /><div><strong>{row.name}</strong><small>{row.app_id ? `Application · ${row.app_id}` : row.service ? `Service · ${row.service}` : row.image}</small></div></div> },
    { id: 'status', label: 'Status', render: row => <StatusBadge status={row.health || row.state || row.status || 'unknown'} /> },
    { id: 'cpu', label: 'CPU', render: row => <div className="usage-cell"><span>{Number(row.cpu_percent || 0).toFixed(1)}%</span><ProgressBar value={row.cpu_percent || 0} /></div> },
    { id: 'memory', label: 'Memory', render: row => <div className="usage-cell"><span>{formatBytes(row.memory_bytes)} / {formatBytes(row.memory_limit_bytes)}</span><ProgressBar value={row.memory_percent || (row.memory_limit_bytes ? (row.memory_bytes || 0) / row.memory_limit_bytes * 100 : 0)} tone={(row.memory_percent || 0) > 90 ? 'danger' : 'accent'} /></div> },
    { id: 'actions', label: 'Actions', render: row => <ConfirmAction title={`Restart ${row.name}?`} description="Docker will stop and start this container. The application or managed service may be briefly unavailable." confirmLabel="Restart container" onConfirm={() => restart(row.name)}><Button size="sm"><RefreshCw size={15} />Restart</Button></ConfirmAction> },
  ];
  return <><PageHeader eyebrow="Server containers" title={server?.name || serverID} description="Live process usage sampled by the hosting agent. Rows stay in a stable order while values update." back="#servers" actions={<Button onClick={load}><RefreshCw size={16} />Refresh</Button>} />{error && <ErrorBanner>{error}</ErrorBanner>}<Card><SectionHeader title="Containers & resource usage" description="Restart only containers that are unhealthy or stuck. Persistent volumes are not removed." />{loading ? <Loading label="Loading containers…" /> : <ResourceList rows={containers} columns={columns} rowKey={row => row.id || row.name} />}</Card></>;
}

export function ServerConsolePage({ serverID }: { serverID: string }) {
  const { servers } = useWorkspace(); const server = servers.find(item => item.id === serverID); const [status, setStatus] = useState<{ two_factor_enabled: boolean; unlocked: boolean }>(); const [command, setCommand] = useState(''); const [output, setOutput] = useState('Locked.'); const [busy, setBusy] = useState(false); const [error, setError] = useState(''); const notify = useToast();
  useEffect(() => { get<typeof status>(`control/servers/${encodeURIComponent(serverID)}/console`).then(setStatus).catch(reason => setError(reason instanceof Error ? reason.message : String(reason))); }, [serverID]);
  const unlock = async (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); const form = event.currentTarget; const code = String(new FormData(form).get('code') || ''); try { const result = await post<{ expiresAt: string }>('v1/auth/step-up', { code, purpose: 'server.console', target: serverID }); setStatus({ two_factor_enabled: true, unlocked: true }); form.reset(); notify(`Root console unlocked until ${new Date(result.expiresAt).toLocaleTimeString()}.`); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } };
  const run = async (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); const submitted = command; setCommand(''); setBusy(true); setOutput('Running…'); try { const result = await post<{ output?: string; exit_code?: number; duration_seconds?: number }>(`control/servers/${encodeURIComponent(serverID)}/console`, { command: submitted }); setOutput(`${result.output || '(no output)'}\n\n[exit ${result.exit_code} · ${Number(result.duration_seconds || 0).toFixed(2)}s]`); } catch (reason) { setOutput(reason instanceof Error ? reason.message : String(reason)); } finally { setBusy(false); } };
  if (!status && !error) return <Loading overlay label="Checking console access…" />;
  return <><PageHeader eyebrow="Protected operation" title={`${server?.name || serverID} root console`} description="Just-in-time root access is scoped to this server and recorded in the audit log." back="#servers" />{error && <ErrorBanner>{error}</ErrorBanner>}<div className="console-grid"><Card className="form-card"><SectionHeader title="Security unlock" description={status?.two_factor_enabled ? 'Verify to unlock this server for 15 minutes. Verify again to extend access.' : 'Enable two-factor authentication before using the root console.'} />{status?.two_factor_enabled ? <form id="console-unlock" onSubmit={unlock}><Field label="Authenticator or recovery code"><input name="code" inputMode="numeric" autoComplete="one-time-code" required /></Field><Button type="submit"><KeyRound size={16} />{status.unlocked ? 'Extend access' : 'Unlock for 15 minutes'}</Button></form> : <Button onClick={() => { location.hash = 'page/two-factor/account'; }}>Set up two-factor authentication</Button>}</Card><Card className="form-card"><SectionHeader title="Run command" description="Maximum runtime: 2 minutes. Output is capped at 1 MiB." /><form id="console-command" onSubmit={run}><Field label="Command"><textarea rows={10} required spellCheck={false} value={command} onChange={event => setCommand(event.target.value)} placeholder={'docker ps\n\ndocker exec -i lidza-db-app sh -c \'exec psql …\''} /></Field><Button variant="primary" type="submit" disabled={!status?.unlocked || busy}><Play size={16} />{busy ? 'Running…' : 'Run command'}</Button></form><div className="console-output-heading"><h3>Output</h3><CopyButton value={output} label="Copy output" /></div><pre className="console-output">{output}</pre></Card></div></>;
}

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
          const value = await get<{ deployments?: Deployment[] } | Deployment[]>('control/deployments');
          const rows = Array.isArray(value) ? value : value.deployments || [];
          const current = rows.find(item => item.id === id);
          if (!active) return;
          if (current) {
            setDeployment(current);
            const latest = current.log || current.error;
            if (latest) setOutput(latest);
            setError('');
            setLoading(false);
            schedule(!['queued', 'building'].includes(current.status));
            return;
          }
          setError('');
        } else {
          const value = await get<{ logs?: string }>(`control/apps/${encodeURIComponent(id)}/logs`);
          if (!active) return;
          if (value.logs) setOutput(value.logs);
          setError('');
        }
      } catch (reason) {
        if (!active) return;
        setError(`Live refresh failed: ${reason instanceof Error ? reason.message : String(reason)}. Retrying…`);
      }
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

  const visibleOutput = output || (terminal ? 'No build output was recorded.' : kind === 'build-logs' ? 'Waiting for build output…' : 'Waiting for runtime output…');
  const stateLabel = kind === 'runtime-logs' ? `${paused ? 'Paused' : 'Live'} · runtime` : terminal ? `Finished · ${deployment?.status}` : paused ? `Paused · ${deployment?.status || 'waiting'}` : `Live · ${deployment?.status || 'waiting'}`;
  const status = kind === 'runtime-logs' ? loading ? 'connecting' : 'running' : deployment?.status || (loading ? 'connecting' : 'waiting');
  return <><PageHeader eyebrow={kind === 'build-logs' ? 'Deployment output' : 'Application output'} title={kind === 'build-logs' ? `Build ${id.slice(0, 12)}` : `${id} runtime logs`} description={terminal ? 'This deployment has finished. Its complete output remains available below.' : 'New output appears here automatically without replacing the page or moving your reading position.'} back={kind === 'build-logs' ? '#deployments' : '#applications'} actions={<div className="sticky-actions"><CopyButton value={visibleOutput} label="Copy logs" /><Button id="log-pause" onClick={() => setPaused(value => !value)} disabled={terminal}>{paused ? <Play size={15} /> : <Pause size={15} />}{paused ? 'Resume live output' : 'Pause live output'}</Button><Button size="sm" onClick={() => setRefreshKey(value => value + 1)}><RefreshCw size={15} />Refresh</Button></div>} />{error && <div id="deployment-refresh-error"><ErrorBanner>{error}</ErrorBanner></div>}<Card className="log-status-card"><div><StatusBadge status={status} /><strong id="deployment-state" aria-live="polite">{stateLabel}</strong>{deployment && <span>{Math.round(deployment.duration_seconds || 0)} seconds</span>}</div><label className="log-follow-control"><input id="log-follow" type="checkbox" checked={follow} onChange={event => setFollow(event.target.checked)} /><span>Follow latest output</span></label></Card><Card className="log-card">{loading && !output ? <Loading label={`Connecting to live ${kind === 'build-logs' ? 'build' : 'runtime'} output…`} /> : <pre id="deployment-output" ref={outputRef} onScroll={event => { const element = event.currentTarget; if (follow && element.scrollHeight - element.scrollTop - element.clientHeight > 28) setFollow(false); }}>{visibleOutput}</pre>}</Card></>;
}
