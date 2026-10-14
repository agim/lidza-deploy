import { Activity, Database, ExternalLink, FileClock, GitBranch, HardDrive, MoreHorizontal, Play, Plus, RefreshCw, RotateCcw, SearchX, Settings, ShieldCheck, Square, Trash2 } from 'lucide-react';
import { useMemo, useState } from 'react';
import { del, post, put } from '../lib/api';
import { detailHref } from '../lib/router';
import type { Application } from '../lib/types';
import { useWorkspace } from '../state/WorkspaceContext';
import { useToast } from '../components/ToastProvider';
import { ActionMenu, Badge, Button, Card, ConfirmAction, EmptyState, ErrorBanner, MenuItem, MenuLabel, MenuSeparator, PageHeader, SearchField, StatusBadge, Toolbar } from '../components/ui';

function appState(app: Application, deployment?: { status?: string; kind?: string }) {
  if (app.retiring) return 'removal pending';
  if (app.stopped) return deployment?.kind === 'start' && ['queued', 'building'].includes(deployment.status || '') ? 'starting' : 'stopped';
  if (app.agent_error) return 'failed';
  if (['building', 'queued'].includes(deployment?.status || '')) return deployment?.status || 'pending';
  if (deployment?.status === 'failed') return 'failed';
  return app.current ? 'live' : deployment?.status || 'not deployed';
}

export function ApplicationsPage() {
  const { apps, servers, deployments, latestDeployment, error, refresh } = useWorkspace();
  const [query, setQuery] = useState('');
  const notify = useToast();
  const visible = useMemo(() => apps.filter(app => `${app.id} ${app.domain} ${app.repository}`.toLowerCase().includes(query.toLowerCase())), [apps, query]);
  const run = async (app: Application, action: string, method: 'POST' | 'DELETE' = 'POST') => {
    try { if (new URLSearchParams(location.search).get('demo') !== '1') method === 'DELETE' ? await del(`control/apps/${app.id}/${action}`) : await post(`control/apps/${app.id}/${action}`); notify(action === 'deploy' ? 'Deployment queued.' : `${action.replace('-', ' ')} requested.`); await refresh(); } catch (error) { notify(error instanceof Error ? error.message : String(error), true); }
  };
  return <>
    <PageHeader eyebrow="Applications" title="Applications" description="Deploy, inspect, and operate every app from one stable view." actions={<Button variant="primary" onClick={() => { location.hash = 'new-application'; }} disabled={!servers.length}><Plus size={17} />New application</Button>} />
    <div className="stats-grid"><Card><span>Applications</span><strong>{apps.length}</strong><small>Across connected servers</small></Card><Card><span>Healthy releases</span><strong>{deployments.filter(item => item.status === 'live').length}</strong><small>In retained history</small></Card><Card><span>Servers</span><strong>{servers.length}</strong><small>Independent agents</small></Card></div>
    {error && <ErrorBanner>{error}</ErrorBanner>}
    <Toolbar><div><h2>Your applications</h2><Badge>{apps.length}</Badge></div><SearchField value={query} onChange={event => setQuery(event.target.value)} placeholder="Search applications…" /></Toolbar>
    <div className="application-list">
      {visible.map(app => { const deployment = latestDeployment(app.id); const server = servers.find(item => item.id === app.server_id); return <Card className="application-card" key={app.id} data-app-card={app.id}>
        <div className="application-summary"><span className="resource-avatar">{app.id.slice(0, 2).toUpperCase()}</span><div className="application-identity"><div><h3>{app.id}</h3><StatusBadge status={appState(app, deployment)} /></div><a href={`https://${app.domain}`} target="_blank" rel="noreferrer">{app.domain}<ExternalLink size={13} /></a></div><div className="application-source"><span><GitBranch size={15} />{app.repository}</span><span>{app.branch}</span><code>{app.current?.commit?.slice(0, 7) || 'pending'}</code></div><div className="application-meta"><span>{server?.name || app.server_id}</span><span>{app.auto_deploy ? 'Auto-deploy on' : 'Manual deploys'}</span></div></div>
        <div className="application-actions"><Button variant="primary" size="sm" data-action="deploy" data-id={app.id} disabled={!!app.stopped || !!app.retiring} onClick={() => run(app, 'deploy')}><Play size={15} />Deploy</Button><Button size="sm" data-history={deployment?.id || ''} disabled={!deployment} onClick={() => deployment && (location.hash = `page/build-logs/${encodeURIComponent(deployment.id)}`)}><FileClock size={15} />Build logs</Button><Button size="sm" data-action="settings" data-id={app.id} onClick={() => { location.hash = `page/settings/${encodeURIComponent(app.id)}`; }}><Settings size={15} />Settings</Button>
          <ActionMenu label="More"><MenuLabel>Inspect</MenuLabel><MenuItem icon={<Activity size={16} />} onSelect={() => { location.hash = `page/runtime-logs/${encodeURIComponent(app.id)}`; }}>Runtime logs</MenuItem><MenuItem icon={<ShieldCheck size={16} />} onSelect={() => { location.hash = `security/${encodeURIComponent(app.id)}`; }}>Security activity</MenuItem><MenuSeparator /><MenuLabel>Resources</MenuLabel><MenuItem icon={<Database size={16} />} onSelect={() => { location.hash = `page/database/${encodeURIComponent(app.id)}`; }}>Database & backups</MenuItem><MenuItem icon={<HardDrive size={16} />} onSelect={() => { location.hash = `page/cache/${encodeURIComponent(app.id)}`; }}>Cache</MenuItem><MenuSeparator /><MenuLabel>Operations</MenuLabel><MenuItem icon={app.stopped ? <Play size={16} /> : <Square size={16} />} onSelect={async () => { try { if (new URLSearchParams(location.search).get('demo') !== '1') await put(`control/apps/${app.id}/state`, { stopped: !app.stopped }); notify(app.stopped ? 'Application start requested.' : 'Application stop requested.'); await refresh(); } catch (error) { notify(error instanceof Error ? error.message : String(error), true); } }}>{app.stopped ? 'Start application' : 'Stop application'}</MenuItem><MenuItem icon={<RefreshCw size={16} />} onSelect={() => run(app, 'reload')}>Reload</MenuItem><MenuItem icon={<RotateCcw size={16} />} onSelect={() => run(app, 'rollback')}>Rollback</MenuItem><MenuSeparator />
            <ConfirmAction title={`Remove ${app.id}?`} description="The application and its containers will be removed. Files, cache, databases, and backups remain retained separately." confirmLabel="Remove application" destructive onConfirm={async () => { if (new URLSearchParams(location.search).get('demo') !== '1') await del(`control/apps/${app.id}`, { confirm: app.id }); notify('Application removed. Retained data remains available.'); await refresh(); }}><div className="menu-confirm-trigger"><Trash2 size={16} />Remove application</div></ConfirmAction>
          </ActionMenu>
        </div>
      </Card>; })}
      {!visible.length && <EmptyState icon={<SearchX />} title={apps.length ? 'No matching applications' : servers.length ? 'Add your first application' : 'Connect your first server'} description={apps.length ? 'Try another name, domain, or repository.' : servers.length ? 'Choose a repository and hosting server to prepare your first release.' : 'A hosting agent is required before an application can be created.'} action={<Button variant="primary" onClick={() => { location.hash = servers.length ? 'new-application' : 'servers'; }}>{servers.length ? 'New application' : 'Connect server'}</Button>} />}
    </div>
  </>;
}
