import { Activity, ArrowUpRight, Box, CheckCheck, ChevronRight, GitFork, LayoutGrid, List, Server, Database, ExternalLink, FileClock, GitBranch, HardDrive, MoreHorizontal, Play, Plus, RefreshCw, RotateCcw, SearchX, Settings, ShieldCheck, Square, Trash2 } from 'lucide-react';
import { useCallback, useMemo, useState } from 'react';
import { del, post, put } from '../lib/api';
import { relativeTime } from '../lib/format';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '../components/primitives/select';
import type { Application, Deployment } from '../lib/types';
import { useWorkspace } from '../state/WorkspaceContext';
import { useToast } from '../components/ToastProvider';
import { ActionMenu, Badge, Button, Card, ConfirmAction, EmptyState, ErrorBanner, MenuItem, MenuLabel, MenuSeparator, PageHeader, SearchField, StatusBadge, Toolbar } from '../components/ui';

function appState(app: Application, deployment?: { status?: string; kind?: string }) {
  if (app.retiring) return 'removal pending';
  if (app.stopped) return deployment?.kind === 'start' && ['queued', 'building'].includes(deployment.status || '') ? 'starting' : 'stopped';
  if (app.agent_error) return 'failed';
  if (['building', 'queued'].includes(deployment?.status || '')) return deployment?.status || 'pending';
  if (deployment?.status === 'failed') return 'failed';
  if(app.current?.stack?.bootstrap) return 'setup_required';
  return app.current ? 'live' : deployment?.status || 'not deployed';
}

export function ApplicationsPage() {
  const { apps, servers, deployments, latestDeployment, error, refresh } = useWorkspace();
  const [submitted, setSubmitted] = useState<Record<string, Deployment>>({});
  const displayedDeployment = useCallback((id: string) => { const saved = latestDeployment(id); const queued = submitted[id]; return queued && (!saved || +new Date(queued.created) > +new Date(saved.created)) ? queued : saved; }, [latestDeployment, submitted]);
  const [query, setQuery] = useState('');
  const [actionErrors, setActionErrors] = useState<Record<string, string>>({});
  const [filter, setFilter] = useState('all');
  const [view, setView] = useState<'grid' | 'list'>('grid');
  const notify = useToast();
  const visible = useMemo(() => apps.filter(app => `${app.id} ${app.domain} ${app.repository}`.toLowerCase().includes(query.toLowerCase()) && (filter === 'all' || appState(app, displayedDeployment(app.id)) === filter)), [apps, query, filter, displayedDeployment]);
  const run = async (app: Application, action: string, method: 'POST' | 'DELETE' = 'POST') => {
    try {
      setActionErrors(current => ({ ...current, [app.id]: '' }));
      if (new URLSearchParams(location.search).get('demo') !== '1') {
        const response = method === 'DELETE' ? await del<Deployment>(`control/apps/${app.id}/${action}`) : await post<Deployment>(`control/apps/${app.id}/${action}`);
        if (response?.id && (action === 'deploy' || action === 'reload')) setSubmitted(current => ({ ...current, [app.id]: { ...response, app_id: app.id, server_id: app.server_id, status: response.status || 'queued', created: response.created || new Date().toISOString() } }));
      }
      notify(action === 'deploy' ? 'Deployment queued.' : `${action.replace('-', ' ')} requested.`); await refresh();
    } catch (error) { const message = error instanceof Error ? error.message : String(error); setActionErrors(current => ({ ...current, [app.id]: message })); notify(message, true); }
  };
  const live = apps.filter(app => appState(app, displayedDeployment(app.id)) === 'live').length;
  return <>
    <PageHeader eyebrow="Your workspace, at a glance" title="Applications" description="From your next idea to your latest release. All in one place." actions={<Button variant="primary" onClick={() => { location.hash = 'new-application'; }} disabled={!servers.length}><Plus size={16} />New application</Button>} />
    <div className="stats-grid stat-grid"><Card><div className="stat-label">Applications<LayoutGrid /></div><div className="stat-value">{apps.length}<span>across your workspace</span></div><div className="stat-bottom"><span className="mini-dot" />{live} live applications</div></Card><Card><div className="stat-label">Healthy releases<CheckCheck /></div><div className="stat-value">{deployments.filter(item => item.status === 'live').length}<span>in retained history</span></div><div className="stat-bottom">{deployments.length ? `Latest release · ${relativeTime([...deployments].sort((a, b) => +new Date(b.created) - +new Date(a.created))[0].created)}` : 'Your first release starts here'}</div></Card><Card><div className="stat-label">Connected servers<Server /></div><div className="stat-value">{servers.length}<span>independent agents</span></div><div className="stat-bottom"><span className="mini-dot" />Configured hosting agents</div></Card></div>
    {error && <ErrorBanner>{error}</ErrorBanner>}
    <div className="list-heading"><h2>Your applications <Badge>{String(apps.length).padStart(2, '0')}</Badge></h2></div>
    <Toolbar><SearchField value={query} onChange={event => setQuery(event.target.value)} placeholder="Search applications…" /><Select value={filter} onValueChange={setFilter}><SelectTrigger className="status-select" aria-label="Filter by application status"><SelectValue /></SelectTrigger><SelectContent>{[['all', 'All statuses'], ['live', 'Live'], ['setup_required', 'Setup required'], ['building', 'Building'], ['queued', 'Queued'], ['failed', 'Failed'], ['stopped', 'Stopped'], ['not deployed', 'Not deployed']].map(([value, label]) => <SelectItem key={value} value={value}>{label}</SelectItem>)}</SelectContent></Select><div className="view-toggle" role="group" aria-label="Application view"><Button size="icon" variant="ghost" aria-label="Grid view" aria-pressed={view === 'grid'} onClick={() => setView('grid')}><LayoutGrid size={16} /></Button><Button size="icon" variant="ghost" aria-label="List view" aria-pressed={view === 'list'} onClick={() => setView('list')}><List size={16} /></Button></div></Toolbar>
    <div className={`application-grid application-view-${view}`}>
      {visible.map(app => { const deployment = displayedDeployment(app.id); const server = servers.find(item => item.id === app.server_id); return <Card className="application-card" key={app.id} data-app-card={app.id}>
        <div className="application-card-content"><div className="app-card-heading"><span className="app-avatar"><Box size={23} strokeWidth={1.5} /></span><StatusBadge status={appState(app, deployment)} /></div><a className="app-name" href={`#page/settings/${encodeURIComponent(app.id)}`}>{app.id}<ChevronRight size={14} /></a><a className="app-domain" href={`https://${app.domain}`} target="_blank" rel="noreferrer">{app.domain}<ArrowUpRight size={12} /></a><div className="app-source"><GitFork size={14} /><span>{app.repository}</span></div><div className="app-details"><span><GitBranch size={13} />{app.branch}<code>{app.current?.commit && app.current.commit !== 'pending' ? app.current.commit.slice(0, 7) : app.current ? 'Commit unavailable' : 'No release deployed'}</code></span><span><Server size={13} />{server?.name || app.server_id}</span></div><div className="auto-deploy"><span className={`mini-dot ${!app.auto_deploy ? 'neutral-dot' : ''}`} />Auto-deploy {app.auto_deploy ? 'enabled' : 'disabled'}</div></div>
        {(actionErrors[app.id] || deployment?.error) && <div className="app-deployment-error" role="alert">{actionErrors[app.id] ? `Request failed: ${actionErrors[app.id]}` : `Last deployment failed: ${deployment?.error}`}</div>}
        <div className="application-actions app-card-footer">{app.current?.stack?.bootstrap && <a className="button button-secondary" href={`https://${app.domain}${app.current.stack.manifest?.bootstrap_readiness?.setup_path || "/setup"}`} target="_blank" rel="noreferrer">Open setup<ArrowUpRight size={13}/></a>}<Button size="sm" data-action="deploy" data-id={app.id} disabled={!!app.stopped || !!app.retiring} onClick={() => run(app, 'deploy')}><ArrowUpRight size={14} />Deploy</Button><Button variant="ghost" size="sm" data-history={deployment?.id || ''} disabled={!deployment} onClick={() => deployment && (location.hash = `page/build-logs/${encodeURIComponent(deployment.id)}`)}><FileClock size={14} />Build logs</Button><Button variant="ghost" size="icon" className="app-settings-action" aria-label={`Settings for ${app.id}`} data-action="settings" data-id={app.id} onClick={() => { location.hash = `page/settings/${encodeURIComponent(app.id)}`; }}><Settings size={16} /></Button>
          <ActionMenu label="More" compact><MenuLabel>Inspect</MenuLabel><MenuItem icon={<Activity size={16} />} onSelect={() => { location.hash = `page/runtime-logs/${encodeURIComponent(app.id)}`; }}>Runtime logs</MenuItem><MenuItem icon={<ShieldCheck size={16} />} onSelect={() => { location.hash = `security/${encodeURIComponent(app.id)}`; }}>Security activity</MenuItem><MenuSeparator /><MenuLabel>Resources</MenuLabel><MenuItem icon={<Database size={16} />} onSelect={() => { location.hash = `page/database/${encodeURIComponent(app.id)}`; }}>Database & backups</MenuItem><MenuItem icon={<HardDrive size={16} />} onSelect={() => { location.hash = `page/cache/${encodeURIComponent(app.id)}`; }}>Cache</MenuItem><MenuSeparator /><MenuLabel>Operations</MenuLabel><MenuItem icon={app.stopped ? <Play size={16} /> : <Square size={16} />} onSelect={async () => { try { if (new URLSearchParams(location.search).get('demo') !== '1') await put(`control/apps/${app.id}/state`, { stopped: !app.stopped }); notify(app.stopped ? 'Application start requested.' : 'Application stop requested.'); await refresh(); } catch (error) { notify(error instanceof Error ? error.message : String(error), true); } }}>{app.stopped ? 'Start application' : 'Stop application'}</MenuItem><MenuItem icon={<RefreshCw size={16} />} onSelect={() => run(app, 'reload')}>Reload</MenuItem><MenuItem icon={<RotateCcw size={16} />} onSelect={() => run(app, 'rollback')}>Rollback</MenuItem><MenuSeparator />
            <ConfirmAction title={`Remove ${app.id}?`} description="The application and its containers will be removed. Files, cache, databases, and backups remain retained separately." confirmLabel="Remove application" destructive onConfirm={async () => { if (new URLSearchParams(location.search).get('demo') !== '1') await del(`control/apps/${app.id}`, { confirm: app.id }); notify('Application removed. Retained data remains available.'); await refresh(); }}><div className="menu-confirm-trigger"><Trash2 size={16} />Remove application</div></ConfirmAction>
          </ActionMenu>
        </div>
      </Card>; })}
      {!query && filter === 'all' && servers.length > 0 && <button className="new-app-card" onClick={() => { location.hash = 'new-application'; }}><span className="new-app-icon"><Plus size={20} /></span><strong>Your next application</strong><small>Give your next idea a place to live.</small></button>}
    </div>
    {!visible.length && <EmptyState icon={<SearchX />} title={apps.length ? 'No matching applications' : servers.length ? 'Add your first application' : 'Connect your first server'} description={apps.length ? 'Try another name, domain, repository, or status.' : servers.length ? 'Choose a repository and hosting server to prepare your first release.' : 'A hosting agent is required before an application can be created.'} action={<Button variant="primary" onClick={() => { location.hash = servers.length ? 'new-application' : 'servers'; }}>{servers.length ? 'New application' : 'Connect server'}</Button>} />}
    <Card className="workspace-note"><ShieldCheck size={20} /><div><strong>Your applications run on your infrastructure.</strong><p>Līdza brings releases, configuration, and visibility into one workspace.</p></div><Badge>Self-hosted</Badge></Card>
  </>;
}
