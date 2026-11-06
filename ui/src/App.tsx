import { useEffect } from 'react';
import { HOME, routeTitle, useRoute } from './lib/router';
import type { Route } from './lib/types';
import { AppShell } from './components/AppShell';
import { ErrorBanner, Loading } from './components/ui';
import { ApplicationsPage } from './pages/ApplicationsPage';
import { DeploymentsPage } from './pages/DeploymentsPage';
import { ServersPage } from './pages/ServersPage';
import { AppSettingsPage } from './pages/AppSettingsPage';
import { NewApplicationPage } from './pages/NewApplicationPage';
import { ServerCleanupPage } from './pages/ServerCleanupPage';
import { ContainersPage, LogPage, ServerConsolePage, ServerSettingsPage } from './pages/ServerDetailPages';
import { DatabaseDetailPage, DatabasesPage, RemoteBackupsPage } from './pages/DataPages';
import { IntegrationsPage } from './pages/IntegrationsPage';
import { ActivityPage, DefaultsPage, HelpPage, TeamPage, TwoFactorPage, UpdatesPage } from './pages/WorkspacePages';
import { CachePage, SimpleServerOperationPage } from './pages/ResourceDetailPages';

export default function App() {
  // main.tsx starts the router before the first render, so the route is
  // already normalized from any legacy hash or bare entry URL here.
  const route = useRoute();
  const { page, detail, id } = route;
  useEffect(() => { window.scrollTo({ top: 0 }); }, [page, detail, id]);
  useEffect(() => { document.title = routeTitle(route); }, [route]);
  return <AppShell route={route}><RouteView route={route} /></AppShell>;
}

function RouteView({ route }: { route: Route }) {
  if (route.page === 'applications') return <ApplicationsPage />;
  if (route.page === 'deployments') return <DeploymentsPage />;
  if (route.page === 'servers') return <ServersPage />;
  if (route.page === 'databases') return <DatabasesPage />;
  if (route.page === 'remote-backups') return <RemoteBackupsPage />;
  if (route.page === 'integrations') return <IntegrationsPage />;
  if (route.page === 'defaults') return <DefaultsPage />;
  if (route.page === 'updates') return <UpdatesPage />;
  if (route.page === 'team') return <TeamPage />;
  if (route.page === 'audit') return <TeamPage audit />;
  if (route.page === 'errors') return <ActivityPage selectedID={route.id} />;
  if (route.page === 'security') return <ActivityPage security selectedID={route.id} />;
  if (route.page === 'help') return <HelpPage />;
  if (route.page === 'new-application') return <NewApplicationPage serverID={route.id} />;
  if (route.page === 'detail' && route.id) {
    if (route.detail === 'settings') return <AppSettingsPage appID={route.id} />;
    if (route.detail === 'server') return <ServerSettingsPage serverID={route.id} />;
    if (route.detail === 'cleanup') return <ServerCleanupPage serverID={route.id} />;
    if (route.detail === 'containers') return <ContainersPage serverID={route.id} />;
    if (route.detail === 'console') return <ServerConsolePage key={route.id} serverID={route.id} />;
    if (route.detail === 'database') return <DatabaseDetailPage appID={route.id} />;
    if (route.detail === 'cache') return <CachePage appID={route.id} />;
    if (route.detail === 'build-logs' || route.detail === 'runtime-logs') return <LogPage kind={route.detail} id={route.id} />;
    if (route.detail === 'ssh' || route.detail === 'upgrade') return <SimpleServerOperationPage kind={route.detail} serverID={route.id} />;
    if (route.detail === 'two-factor') return <TwoFactorPage />;
  }
  return <ErrorBanner>That workspace page is not available. <a href={HOME}>Return to applications</a>.</ErrorBanner>;
}
