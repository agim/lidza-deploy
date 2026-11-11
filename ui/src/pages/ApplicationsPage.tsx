import { useUrlPreference } from '../state/useUrlPreference';
import { createHref, navigate, resourceHref, withSticky } from '../lib/router';
import { AlertTriangle, CheckCheck, Plus, RefreshCw, SearchX } from 'lucide-react';
import { useMemo } from 'react';
import { relativeTime } from '../lib/format';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '../components/primitives/select';
import type { Application, Deployment } from '../lib/types';
import { useWorkspace } from '../state/WorkspaceContext';
import { Button, EmptyState, ErrorBanner, SearchField, StatusBadge } from '../components/ui';
import type { Column } from '../components/ResourceList';
import { ResourceIndexPage, ResourceName } from '../components/resource';

/** appState is the status an application shows everywhere: index row, detail header. */
export function appState(app: Application, deployment?: { status?: string; kind?: string }) {
  if (app.observed) return 'observed';
  if (app.retiring) return 'removal pending';
  if (app.stopped) return deployment?.kind === 'start' && ['queued', 'building'].includes(deployment.status || '') ? 'starting' : 'stopped';
  if (app.native && !app.management_capabilities?.deploy) return 'native';
  if (app.agent_error) return 'failed';
  if (['building', 'queued'].includes(deployment?.status || '')) return deployment?.status || 'pending';
  if (deployment?.status === 'failed') return 'failed';
  if (app.current?.stack?.bootstrap) return 'setup_required';
  return app.current ? 'live' : deployment?.status || 'not deployed';
}

/** The release an application serves, in words. */
export function releaseLabel(app: Application) {
  if (app.current?.commit && app.current.commit !== 'pending') return app.current.commit.slice(0, 7);
  if (app.current) return 'Commit unavailable';
  return app.native ? 'Existing native release' : 'No release deployed';
}

/** The runtime an application shows; applications without one are Līdza (Go). */
export function appRuntime(app: { runtime?: string }): string {
  return app.runtime || 'go';
}

export function runtimeLabel(runtime: string): string {
  return ({ go: 'Līdza (Go)', rails: 'Ruby on Rails', astro: 'Astro', node: 'Node.js', static: 'Static site' } as Record<string, string>)[runtime] || runtime;
}

const statuses = [['all', 'All statuses'], ['observed', 'Observed'], ['native', 'Native services'], ['live', 'Live'], ['setup_required', 'Setup required'], ['building', 'Building'], ['queued', 'Queued'], ['failed', 'Failed'], ['stopped', 'Stopped'], ['not deployed', 'Not deployed']];

/** ApplicationsPage: the index of applications. Everything about one application is on its detail page. */
export function ApplicationsPage() {
  const { apps, servers, latestDeployment, error, status } = useWorkspace();
  const [query, setQuery] = useUrlPreference('deploy.apps.query', 'q', '');
  const [filter, setFilter] = useUrlPreference('deploy.apps.status', 'status', 'all');
  const [runtimeFilter, setRuntimeFilter] = useUrlPreference('deploy.apps.runtime', 'runtime', 'all');
  const [serverFilter, setServerFilter] = useUrlPreference('deploy.apps.server', 'server', 'all');
  const runtimes = useMemo(() => [...new Set(apps.map(appRuntime))].sort(), [apps]);
  const visible = useMemo(() => apps.filter(app => `${app.id} ${app.domain} ${app.repository}`.toLowerCase().includes(query.toLowerCase()) && (filter === 'all' || appState(app, latestDeployment(app.id)) === filter) && (runtimeFilter === 'all' || appRuntime(app) === runtimeFilter) && (serverFilter === 'all' || app.server_id === serverFilter)), [apps, query, filter, runtimeFilter, serverFilter, latestDeployment]);
  const filtered = !!query || filter !== 'all' || runtimeFilter !== 'all' || serverFilter !== 'all';
  const inventory = status?.native_inventory?.filter(item => item.state !== 'unsupported') || [];
  const serverName = (id: string) => servers.find(server => server.id === id)?.name || id;
  const columns: Column<Application>[] = [
    { id: 'name', label: 'Name', render: app => <ResourceName href={withSticky(resourceHref('applications', app.id))} name={app.observed?.name || app.id} detail={app.domain} /> },
    { id: 'status', label: 'Status', render: app => <StatusBadge status={appState(app, latestDeployment(app.id))} /> },
    { id: 'runtime', label: 'Runtime', render: app => runtimeLabel(appRuntime(app)) },
    { id: 'server', label: 'Server', render: app => servers.some(server => server.id === app.server_id) ? <a href={withSticky(resourceHref('servers', app.server_id))}>{serverName(app.server_id)}</a> : serverName(app.server_id) },
    { id: 'release', label: 'Release', render: app => <span className="resource-fact">{app.branch && <span>{app.branch}</span>}<code>{releaseLabel(app)}</code></span> },
    { id: 'updated', label: 'Updated', render: app => { const deployment: Deployment | undefined = latestDeployment(app.id); return deployment ? <a href={withSticky(resourceHref('deployments', deployment.id))} title={new Date(deployment.created).toLocaleString()}>{relativeTime(deployment.created)}</a> : <span className="muted-copy">Never deployed</span>; } },
  ];
  const create = servers.length ? <Button variant="primary" onClick={() => { navigate(createHref('applications')); }}><Plus size={16} />New application</Button> : undefined;
  return <ResourceIndexPage resource="applications" title="Applications" description="Every application on this control panel, its status and where it runs." count={apps.length}
    primaryAction={create}
    notices={<>{error && <ErrorBanner>{error}</ErrorBanner>}{inventory.length > 0 && <div className="notice-stack">{inventory.map(item => <div key={item.server_id} className={`alert notice-row ${item.state === 'error' ? 'alert-warning' : 'alert-info'}`} role="status">{item.state === 'error' ? <AlertTriangle size={17} aria-hidden="true" /> : item.state === 'synced' ? <CheckCheck size={17} aria-hidden="true" /> : <RefreshCw size={17} className="spin" aria-hidden="true" />}<span>{serverName(item.server_id)}: {item.state === 'synced' ? `Application inventory synced automatically · ${item.apps} apps · ${relativeTime(item.checked)}` : item.state === 'error' ? item.message : 'Discovering applications automatically…'}</span></div>)}</div>}</>}
    filters={<><SearchField value={query} onChange={event => setQuery(event.target.value)} placeholder="Search applications…" aria-label="Search applications" />
      <Select value={filter} onValueChange={setFilter}><SelectTrigger className="status-select" aria-label="Filter by application status"><SelectValue /></SelectTrigger><SelectContent>{statuses.map(([value, label]) => <SelectItem key={value} value={value}>{label}</SelectItem>)}</SelectContent></Select>
      {runtimes.length > 1 && <Select value={runtimes.includes(runtimeFilter) ? runtimeFilter : 'all'} onValueChange={setRuntimeFilter}><SelectTrigger className="status-select" aria-label="Filter by runtime"><SelectValue /></SelectTrigger><SelectContent><SelectItem value="all">All runtimes</SelectItem>{runtimes.map(runtime => <SelectItem key={runtime} value={runtime}>{runtimeLabel(runtime)}</SelectItem>)}</SelectContent></Select>}
      {servers.length > 1 && <Select value={servers.some(server => server.id === serverFilter) ? serverFilter : 'all'} onValueChange={setServerFilter}><SelectTrigger className="status-select" aria-label="Filter by server"><SelectValue /></SelectTrigger><SelectContent><SelectItem value="all">All servers</SelectItem>{servers.map(server => <SelectItem key={server.id} value={server.id}>{server.name}</SelectItem>)}</SelectContent></Select>}
      {filtered && <Button variant="ghost" onClick={() => { setQuery(''); setFilter('all'); setRuntimeFilter('all'); setServerFilter('all'); }}>Clear filters</Button>}</>}
    rows={visible} columns={columns} rowKey={app => app.id}
    empty={<EmptyState icon={<SearchX />} title={apps.length ? 'No matching applications' : servers.length ? 'Add your first application' : 'Connect your first server'} description={apps.length ? 'Try another name, domain, repository, status, runtime or server.' : servers.length ? 'Choose a repository and hosting server to prepare your first release.' : 'A hosting agent is required before an application can be created.'} action={apps.length ? undefined : <Button variant="primary" onClick={() => { navigate(servers.length ? createHref('applications') : createHref('servers')); }}>{servers.length ? 'New application' : 'Connect server'}</Button>} />} />;
}

