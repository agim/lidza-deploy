import { usePreference } from '../state/PreferencesContext';
import { useUrlPreference } from '../state/useUrlPreference';
import { detailHref, href, navigate, setSearchParams, useSearchParam, withSticky } from '../lib/router';
import { ApplicationAutoDeploy } from '../components/ApplicationAutoDeploy';
import { ObservedApplicationCard } from '../components/ObservedApplications';
import { MigrationPlan } from '../components/MigrationPlan';
import { LiveProgress } from '../components/LiveProgress';
import { waitingLabel } from '../components/BuildProgress';
import { moveActive, moveHolds, moveLockReason } from '../lib/move';
import { Activity, AlertTriangle, ArrowUpRight, Box, CheckCheck, ChevronRight, Cpu, GitFork, LayoutGrid, List, Server, Database, ExternalLink, FileClock, GitBranch, HardDrive, MoreHorizontal, Play, Plus, RefreshCw, RotateCcw, SearchX, Settings, ShieldCheck, Square, Trash2 } from 'lucide-react';
import { useCallback, useMemo, useState } from 'react';
import { del, post, put } from '../lib/api';
import { relativeTime } from '../lib/format';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '../components/primitives/select';
import type { Application, Deployment } from '../lib/types';
import { useWorkspace } from '../state/WorkspaceContext';
import { useToast } from '../components/ToastProvider';
import { ActionMenu, Badge, Button, Card, ConfirmAction, EmptyState, ErrorBanner, MenuItem, MenuLabel, MenuSeparator, PageHeader, SearchField, StatusBadge, Toolbar } from '../components/ui';

function appState(app: Application, deployment?: { status?: string; kind?: string }) {
  if (app.observed) return 'observed';
  if (app.native && !app.management_capabilities?.deploy) return 'native';
  if (app.retiring) return 'removal pending';
  if (app.stopped) return deployment?.kind === 'start' && ['queued', 'building'].includes(deployment.status || '') ? 'starting' : 'stopped';
  if (app.agent_error) return 'failed';
  if (['building', 'queued'].includes(deployment?.status || '')) return deployment?.status || 'pending';
  if (deployment?.status === 'failed') return 'failed';
  if(app.current?.stack?.bootstrap) return 'setup_required';
  return app.current ? 'live' : deployment?.status || 'not deployed';
}

export function ApplicationsPage() {
  const { apps, servers, deployments, latestDeployment, error, refresh, status } = useWorkspace();
  const [submitted, setSubmitted] = useState<Record<string, Deployment>>({});
  const displayedDeployment = useCallback((id: string) => { const saved = latestDeployment(id); const queued = submitted[id]; return queued && (!saved || +new Date(queued.created) > +new Date(saved.created)) ? queued : saved; }, [latestDeployment, submitted]);
  const [query, setQuery] = useUrlPreference('deploy.apps.query', 'q', '');
  const [actionErrors, setActionErrors] = useState<Record<string, string>>({});
  const [filter, setFilter] = useUrlPreference('deploy.apps.status', 'status', 'all');
  const [runtimeFilter, setRuntimeFilter] = useUrlPreference('deploy.apps.runtime', 'runtime', 'all');
  const [serverFilter, setServerFilter] = useUrlPreference('deploy.apps.server', 'server', 'all');
  const runtimes = useMemo(() => [...new Set(apps.map(appRuntime))].sort(), [apps]);
  const [view, setView] = usePreference<'grid' | 'list'>('deploy.apps.view', 'grid');
  const moving = useSearchParam('move');
  const notify = useToast();
  const visible = useMemo(() => apps.filter(app => `${app.id} ${app.domain} ${app.repository}`.toLowerCase().includes(query.toLowerCase()) && (filter === 'all' || appState(app, displayedDeployment(app.id)) === filter) && (runtimeFilter === 'all' || appRuntime(app) === runtimeFilter) && (serverFilter === 'all' || app.server_id === serverFilter)), [apps, query, filter, runtimeFilter, serverFilter, displayedDeployment]);
  const filtered = !!query || filter !== 'all' || runtimeFilter !== 'all' || serverFilter !== 'all';
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
  const inventory = status?.native_inventory?.filter(item => item.state !== 'unsupported') || [];
  return <>
    <PageHeader eyebrow="Your workspace, at a glance" title="Applications" description="From your next idea to your latest release. All in one place." actions={<Button variant="primary" onClick={() => { navigate(href('new-application')); }} disabled={!servers.length}><Plus size={16} />New application</Button>} />
    <div className="stats-grid stat-grid"><Card><div className="stat-label">Applications<LayoutGrid /></div><div className="stat-value">{apps.length}<span>across your workspace</span></div><div className="stat-bottom"><span className="mini-dot" />{live} live applications</div></Card><Card><div className="stat-label">Healthy releases<CheckCheck /></div><div className="stat-value">{deployments.filter(item => item.status === 'live').length}<span>in retained history</span></div><div className="stat-bottom">{deployments.length ? `Latest release · ${relativeTime([...deployments].sort((a, b) => +new Date(b.created) - +new Date(a.created))[0].created)}` : 'Your first release starts here'}</div></Card><Card><div className="stat-label">Connected servers<Server /></div><div className="stat-value">{servers.length}<span>independent agents</span></div><div className="stat-bottom"><span className="mini-dot" />Configured hosting agents</div></Card></div>
    {error && <ErrorBanner>{error}</ErrorBanner>}
    {inventory.length > 0 && <div className="notice-stack">{inventory.map(item => <div key={item.server_id} className={`alert notice-row ${item.state === 'error' ? 'alert-warning' : 'alert-info'}`} role="status">{item.state === 'error' ? <AlertTriangle size={17} aria-hidden="true" /> : item.state === 'synced' ? <CheckCheck size={17} aria-hidden="true" /> : <RefreshCw size={17} className="spin" aria-hidden="true" />}<span>{servers.find(server => server.id === item.server_id)?.name || item.server_id}: {item.state === 'synced' ? `Application inventory synced automatically · ${item.apps} apps · ${relativeTime(item.checked)}` : item.state === 'error' ? item.message : 'Discovering applications automatically…'}</span></div>)}</div>}
    <div className="list-heading"><h2>Your applications <Badge>{String(apps.length).padStart(2, '0')}</Badge></h2></div>
    <Toolbar><SearchField value={query} onChange={event => setQuery(event.target.value)} placeholder="Search applications…" /><Select value={filter} onValueChange={setFilter}><SelectTrigger className="status-select" aria-label="Filter by application status"><SelectValue /></SelectTrigger><SelectContent>{[['all', 'All statuses'], ['observed', 'Observed'], ['native', 'Native services'], ['live', 'Live'], ['setup_required', 'Setup required'], ['building', 'Building'], ['queued', 'Queued'], ['failed', 'Failed'], ['stopped', 'Stopped'], ['not deployed', 'Not deployed']].map(([value, label]) => <SelectItem key={value} value={value}>{label}</SelectItem>)}</SelectContent></Select>{runtimes.length > 1 && <Select value={runtimes.includes(runtimeFilter) ? runtimeFilter : 'all'} onValueChange={setRuntimeFilter}><SelectTrigger className="status-select" aria-label="Filter by runtime"><SelectValue /></SelectTrigger><SelectContent><SelectItem value="all">All runtimes</SelectItem>{runtimes.map(runtime => <SelectItem key={runtime} value={runtime}>{runtimeLabel(runtime)}</SelectItem>)}</SelectContent></Select>}{servers.length > 1 && <Select value={servers.some(server => server.id === serverFilter) ? serverFilter : 'all'} onValueChange={setServerFilter}><SelectTrigger className="status-select" aria-label="Filter by server"><SelectValue /></SelectTrigger><SelectContent><SelectItem value="all">All servers</SelectItem>{servers.map(server => <SelectItem key={server.id} value={server.id}>{server.name}</SelectItem>)}</SelectContent></Select>}{filtered && <Button variant="ghost" onClick={() => { setQuery(''); setFilter('all'); setRuntimeFilter('all'); setServerFilter('all'); }}>Clear filters</Button>}<div className="view-toggle" role="group" aria-label="Application view"><Button size="icon" variant="ghost" aria-label="Grid view" aria-pressed={view === 'grid'} onClick={() => setView('grid')}><LayoutGrid size={16} /></Button><Button size="icon" variant="ghost" aria-label="List view" aria-pressed={view === 'list'} onClick={() => setView('list')}><List size={16} /></Button></div></Toolbar>
    <div className={`application-grid application-view-${view}`}>
      {visible.map(app => { const deployment = displayedDeployment(app.id); const server = servers.find(item => item.id === app.server_id); const held = moveHolds(app.move); const activeMove = moveActive(app.move); const lockReason = held ? moveLockReason(app.id, servers.find(item => item.id === app.move?.target_server)?.name || app.move!.target_server) : ''; if (app.observed) return <ObservedApplicationCard key={app.id} app={app} />; return <Card className="application-card" key={app.id} data-app-card={app.id}>
        <div className="application-card-content"><div className="app-card-heading"><span className="app-avatar" title={app.runtime === "rails" ? "Ruby on Rails" : app.runtime === "astro" ? "Astro" : "Līdza (Go)"}><Box size={23} strokeWidth={1.5} /></span><StatusBadge status={appState(app, deployment)} /></div><a className="app-name" href={withSticky(detailHref('settings', app.id))}>{app.id}<ChevronRight size={14} /></a><a className="app-domain" href={`https://${app.domain}`} target="_blank" rel="noreferrer">{app.domain}<ArrowUpRight size={12} /></a><div className="app-source"><GitFork size={14} /><span>{app.repository}</span><Badge>{app.runtime === "rails" ? "Rails" : app.runtime==='astro'?'Astro':"Go"}</Badge></div><div className="app-details"><span><GitBranch size={13} />{app.branch}<code>{app.current?.commit && app.current.commit !== 'pending' ? app.current.commit.slice(0, 7) : app.current ? 'Commit unavailable' : app.native?'Existing native release':'No release deployed'}</code></span><span><Server size={13} />{server?.name || app.server_id}</span></div><div className="auto-deploy"><span className={`mini-dot ${!app.auto_deploy ? 'neutral-dot' : ''}`} />Auto-deploy {app.auto_deploy ? 'enabled' : 'disabled'}</div>{deployment && ['queued', 'building'].includes(deployment.status) && <div className="app-card-progress"><LiveProgress compact label={waitingLabel(deployment.progress) || (deployment.status === 'queued' ? 'Queued' : deploymentLabel(deployment.kind))} startedAt={deployment.progress?.started_at || deployment.started || deployment.created} /></div>}</div>
        {(actionErrors[app.id] || deployment?.error) && <div className="app-deployment-error" role="alert">{actionErrors[app.id] ? `Request failed: ${actionErrors[app.id]}` : `Last deployment failed: ${deployment?.error}`}</div>}
        <div className="application-actions app-card-footer">{app.current?.stack?.bootstrap && <a className="button button-secondary" href={`https://${app.domain}${app.current.stack.manifest?.bootstrap_readiness?.setup_path || "/setup"}`} target="_blank" rel="noreferrer">Open setup<ArrowUpRight size={13}/></a>}<Button size="sm" data-action="deploy" data-id={app.id} disabled={!!app.native&&!app.management_capabilities?.deploy || !!app.stopped || !!app.retiring || held} title={lockReason || (app.native&&!app.management_capabilities?.deploy?'Native deployment ownership requires the reviewed host change plan':undefined)} onClick={() => run(app, 'deploy')}><ArrowUpRight size={14} />Deploy</Button><Button variant="ghost" size="sm" data-history={deployment?.id || ''} disabled={!deployment} onClick={() => deployment && (navigate(detailHref('build-logs', deployment.id)))}><FileClock size={14} />Build logs</Button><Button variant="ghost" size="icon" className="app-settings-action" aria-label={`Settings for ${app.id}`} data-action="settings" data-id={app.id} onClick={() => { navigate(detailHref('settings', app.id)); }}><Settings size={16} /></Button>
          <ActionMenu label="More" compact><MenuLabel>Inspect</MenuLabel><MenuItem icon={<Activity size={16} />} onSelect={() => { navigate(detailHref('runtime-logs', app.id)); }}>Runtime logs</MenuItem><MenuItem icon={<ShieldCheck size={16} />} onSelect={() => { navigate(href('security', app.id)); }}>Security activity</MenuItem><ApplicationAutoDeploy appID={app.id} enabled={app.auto_deploy} disabled={!!app.retiring || !!app.stopped || !!app.native&&!app.management_capabilities?.deploy} requireAppPairing={!!app.native} locked={held} onChange={refresh}/>{!app.native && <><MenuSeparator /><MenuLabel>Resources</MenuLabel><MenuItem icon={<Database size={16} />} onSelect={() => { navigate(detailHref('database', app.id)); }}>Database & backups</MenuItem><MenuItem icon={<HardDrive size={16} />} onSelect={() => { navigate(detailHref('cache', app.id)); }}>Cache</MenuItem><MenuSeparator /><MenuLabel>Operations</MenuLabel><MenuItem disabled={held} icon={app.stopped ? <Play size={16} /> : <Square size={16} />} onSelect={async () => { try { if (new URLSearchParams(location.search).get('demo') !== '1') await put(`control/apps/${app.id}/state`, { stopped: !app.stopped }); notify(app.stopped ? 'Application start requested.' : 'Application stop requested.'); await refresh(); } catch (error) { notify(error instanceof Error ? error.message : String(error), true); } }}>{app.stopped ? 'Start application' : 'Stop application'}</MenuItem><MenuItem disabled={held} icon={<RefreshCw size={16} />} onSelect={() => run(app, 'reload')}>Reload</MenuItem><MenuItem disabled={held} icon={<RotateCcw size={16} />} onSelect={() => run(app, 'rollback')}>Rollback</MenuItem><MenuSeparator />
            {activeMove ? <MenuItem disabled icon={<Trash2 size={16} />}>Remove application</MenuItem> : <ConfirmAction title={`Remove ${app.id}?`} description="The application and its containers will be removed. Files, cache, databases, and backups remain retained separately." confirmLabel="Remove application" destructive onConfirm={async () => { if (new URLSearchParams(location.search).get('demo') !== '1') await del(`control/apps/${app.id}`, { confirm: app.id }); notify('Application removed. Retained data remains available.'); await refresh(); }}><div className="menu-confirm-trigger"><Trash2 size={16} />Remove application</div></ConfirmAction>}</>}{app.native && <><MenuSeparator /><MenuLabel>Resources</MenuLabel><MenuItem icon={<Cpu size={16} />} onSelect={() => { navigate(detailHref('settings', app.id, 'processes')); }}>Processes</MenuItem><MenuItem icon={<Database size={16} />} onSelect={() => { navigate(detailHref('settings', app.id, 'resources')); }}>Data & storage</MenuItem></>}{app.native&&app.management_capabilities?.rollback&&<MenuItem disabled={held} icon={<RotateCcw size={16}/>} onSelect={()=>run(app,'rollback')}>Rollback</MenuItem>}{app.native && app.management_capabilities?.remove && <><MenuSeparator />{activeMove ? <MenuItem disabled icon={<Trash2 size={16} />}>Remove application</MenuItem> : <ConfirmAction title={`Remove ${app.id}?`} description="Its services, account and web route are removed from the server. Its databases, Redis and ClickHouse data are kept as a retained resource with their backups, and application files stay on the server; remove the retained data from the server's settings when it is no longer needed." confirmLabel="Remove application" destructive onConfirm={async () => { if (new URLSearchParams(location.search).get('demo') !== '1') await del(`control/apps/${app.id}`, { confirm: app.id }); notify('Application removed. Its data remains on the server as a retained resource.'); await refresh(); }}><div className="menu-confirm-trigger"><Trash2 size={16} />Remove application</div></ConfirmAction>}</>}
          </ActionMenu>
        </div>
        <MigrationPlan appID={app.id} serverID={app.server_id} servers={servers} held={activeMove} onChanged={refresh} defaultOpen={moving === app.id} onOpenChange={open => { if (open) setSearchParams({ move: app.id }); else if (moving === app.id) setSearchParams({ move: undefined }); }} />
      </Card>; })}
      {!filtered && servers.length > 0 && <button className="new-app-card" onClick={() => { navigate(href('new-application')); }}><span className="new-app-icon"><Plus size={20} /></span><strong>Your next application</strong><small>Give your next idea a place to live.</small></button>}
    </div>
    {!visible.length && <EmptyState icon={<SearchX />} title={apps.length ? 'No matching applications' : servers.length ? 'Add your first application' : 'Connect your first server'} description={apps.length ? 'Try another name, domain, repository, status, runtime or server.' : servers.length ? 'Choose a repository and hosting server to prepare your first release.' : 'A hosting agent is required before an application can be created.'} action={<Button variant="primary" onClick={() => { navigate(href(servers.length ? 'new-application' : 'servers')); }}>{servers.length ? 'New application' : 'Connect server'}</Button>} />}
    <Card className="workspace-note"><ShieldCheck size={20} /><div><strong>Your applications run on your infrastructure.</strong><p>Līdza brings releases, configuration, and visibility into one workspace.</p></div><Badge>Self-hosted</Badge></Card>
  </>;
}

/** What a running deployment does, for the card's progress line. */
function deploymentLabel(kind?: string): string {
  return ({ reload: 'Reloading', rollback: 'Rolling back', start: 'Starting', stop: 'Stopping' } as Record<string, string>)[kind || ''] || 'Deploying';
}

/** The runtime an application card shows; applications without one are Līdza (Go). */
function appRuntime(app: { runtime?: string }): string {
  return app.runtime || 'go';
}

function runtimeLabel(runtime: string): string {
  return ({ go: 'Līdza (Go)', rails: 'Ruby on Rails', astro: 'Astro', node: 'Node.js', static: 'Static site' } as Record<string, string>)[runtime] || runtime;
}
