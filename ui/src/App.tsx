import { useEffect } from 'react';
import { HOME, redirectPath, routeTitle, useLocation, useRoute } from './lib/router';
import type { Route } from './lib/types';
import { AppShell } from './components/AppShell';
import { ResourceNameProvider, useResourceName } from './components/resource';
import { EmptyState, PageHeader } from './components/ui';
import { ApplicationsPage } from './pages/ApplicationsPage';
import { ApplicationDetailPage } from './pages/ApplicationDetailPage';
import { DeploymentDetailPage, DeploymentsPage } from './pages/DeploymentsPage';
import { ServersPage } from './pages/ServersPage';
import { NewApplicationPage } from './pages/NewApplicationPage';
import { ConnectServerPage, ServerDetailPage } from './pages/ServerDetailPages';
import { DatabaseDetailPage, DatabasesPage, RemoteBackupsPage } from './pages/DataPages';
import { IntegrationsPage } from './pages/IntegrationsPage';
import { AccountPage, ActivityDetailPage, ActivityPage, AuditPage, DefaultsPage, HelpPage, NewMemberPage, TeamMemberPage, TeamPage, UpdatesPage } from './pages/WorkspacePages';
import { ControlPanelPage } from './pages/ControlPanelPage';
import { DeletedAppDetailPage, DeletedAppsPage } from './pages/DeletedAppsPage';
import { SearchX } from 'lucide-react';

export default function App() {
  return <ResourceNameProvider><Routed /></ResourceNameProvider>;
}

function Routed() {
  // main.tsx starts the router before the first render, so the route is
  // already normalized from any legacy hash, bare entry URL or earlier path.
  const route = useRoute();
  const { pathname, search } = useLocation();
  const name = useResourceName();
  // An earlier URL reached through Back/Forward is rewritten to its current form.
  useEffect(() => { const target = redirectPath(pathname, search); if (target) history.replaceState(history.state, '', target); }, [pathname, search]);
  useEffect(() => { window.scrollTo({ top: 0 }); }, [route.view, route.collection, route.id]);
  useEffect(() => { document.title = routeTitle(route, name); }, [route, name]);
  return <AppShell route={route}><RouteView route={route} /></AppShell>;
}

function RouteView({ route }: { route: Route }) {
  const { view, collection: c, id = '', tab = '', query } = route;
  if (view === 'index') {
    if (c === 'applications') return <ApplicationsPage />;
    if (c === 'deployments') return <DeploymentsPage />;
    if (c === 'servers') return <ServersPage />;
    if (c === 'databases') return <DatabasesPage />;
    if (c === 'deleted-apps') return <DeletedAppsPage />;
    if (c === 'team') return <TeamPage />;
    if (c === 'audit') return <AuditPage />;
    if (c === 'errors' || c === 'security') return <ActivityPage key={c} security={c === 'security'} appID={query?.app} />;
  }
  if (view === 'create') {
    if (c === 'applications') return <NewApplicationPage serverID={query?.server} />;
    if (c === 'servers') return <ConnectServerPage />;
    if (c === 'team') return <NewMemberPage />;
  }
  if (view === 'detail') {
    if (c === 'applications') return <ApplicationDetailPage key={id} appID={id} tab={tab} />;
    if (c === 'servers') return <ServerDetailPage key={id} serverID={id} tab={tab} />;
    if (c === 'databases') return <DatabaseDetailPage key={id} appID={id} tab={tab} />;
    if (c === 'deployments') return <DeploymentDetailPage key={id} id={id} tab={tab} />;
    if (c === 'deleted-apps') return <DeletedAppDetailPage key={id} id={id} tab={tab} />;
    if (c === 'team') return <TeamMemberPage key={id} id={id} tab={tab} />;
    if (c === 'errors' || c === 'security') return <ActivityDetailPage key={`${c}/${id}`} security={c === 'security'} appID={query?.app || ''} id={id} tab={tab} />;
  }
  if (view === 'singleton') {
    if (c === 'integrations') return <IntegrationsPage tab={tab} />;
    if (c === 'defaults') return <DefaultsPage tab={tab} />;
    if (c === 'remote-backups') return <RemoteBackupsPage tab={tab} />;
    if (c === 'updates') return <UpdatesPage tab={tab} />;
    if (c === 'control-panel') return <ControlPanelPage tab={tab} />;
    if (c === 'help') return <HelpPage tab={tab} />;
    if (c === 'account') return <AccountPage tab={tab} />;
  }
  return <div className="resource-page" data-template="not-found"><PageHeader title="Page not found" description="The address may be mistyped, or the page has moved. The breadcrumb above leads back Home." /><EmptyState icon={<SearchX />} title="Nothing lives at this address" description="Open the applications to find your way again." action={<a className="button button-primary button-md" href={HOME}>Open applications</a>} /></div>;
}
