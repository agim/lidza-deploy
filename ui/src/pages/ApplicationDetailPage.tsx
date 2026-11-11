import { Activity, AlertTriangle, ArrowRightLeft, ArrowUpRight, Box, RotateCcw, ShieldCheck } from 'lucide-react';
import { useCallback, useState } from 'react';
import { post } from '../lib/api';
import { ARCHIVE_RETENTION_DAYS } from '../lib/archive';
import { formatDate, relativeTime } from '../lib/format';
import { moveActive, moveHolds, moveLockReason } from '../lib/move';
import { indexHref, navigate, resourceHref, withSticky } from '../lib/router';
import type { Application, Deployment } from '../lib/types';
import { useWorkspace } from '../state/WorkspaceContext';
import { useToast } from '../components/ToastProvider';
import { ActionMenu, Badge, Button, Card, EmptyState, MenuItem, MenuLabel, MenuLink, MenuSeparator, SectionHeader, StatusBadge } from '../components/ui';
import { ResourceDetailPage, visibleTabs } from '../components/resource';
import { ResourceList } from '../components/ResourceList';
import { SummaryList } from '../components/SummaryList';
import { AppLifecycleMenu, StartApplicationButton } from '../components/AppLifecycle';
import { ApplicationAutoDeploy } from '../components/ApplicationAutoDeploy';
import { ApplicationProcesses } from '../components/ApplicationProcesses';
import { MigrationPlan } from '../components/MigrationPlan';
import { MoveLockCard, useAppMoveLock } from '../components/MoveLock';
import { LiveProgress } from '../components/LiveProgress';
import { waitingLabel } from '../components/BuildProgress';
import { appRuntime, appState, releaseLabel, runtimeLabel } from './ApplicationsPage';
import { deploymentColumns } from './DeploymentsPage';
import { LiveLog } from './LogViews';
import { AppBackupsTab, AppDataTab, AppDomainsTab, AppEnvironmentTab, AppSettingsTab } from './AppSettingsPage';

/** What a running deployment does, for the progress line. */
function deploymentLabel(kind?: string): string {
  return ({ reload: 'Reloading', restart: 'Restarting', rollback: 'Rolling back', start: 'Starting', stop: 'Stopping' } as Record<string, string>)[kind || ''] || 'Deploying';
}

/**
 * ApplicationDetailPage: one application. The header deploys and holds the
 * lifecycle menu; its properties are tabs (docs/console-structure.md).
 */
export function ApplicationDetailPage({ appID, tab }: { appID: string; tab: string }) {
  const { apps, servers, loading, latestDeployment, refresh } = useWorkspace();
  const app = apps.find(item => item.id === appID);
  const notify = useToast();
  const [submitted, setSubmitted] = useState<Deployment>();
  const [actionError, setActionError] = useState('');
  const saved = latestDeployment(appID);
  const deployment = submitted && (!saved || +new Date(submitted.created) > +new Date(saved.created)) ? submitted : saved;
  const lock = useAppMoveLock(appID, { watch: Boolean(app && !app.observed) });
  const run = useCallback(async (action: 'deploy' | 'rollback') => {
    if (!app) return;
    try {
      setActionError('');
      if (new URLSearchParams(location.search).get('demo') !== '1') {
        const response = await post<Deployment>(`control/apps/${encodeURIComponent(app.id)}/${action}`);
        if (response?.id) setSubmitted({ ...response, app_id: app.id, server_id: app.server_id, status: response.status || 'queued', created: response.created || new Date().toISOString() });
      }
      notify(action === 'deploy' ? 'Deployment queued.' : 'Rollback requested.'); await refresh();
    } catch (reason) { const message = reason instanceof Error ? reason.message : String(reason); setActionError(message); notify(message, true); }
  }, [app, notify, refresh]);
  const server = servers.find(item => item.id === app?.server_id);
  const held = moveHolds(app?.move);
  const lockReason = held && app ? moveLockReason(app.id, servers.find(item => item.id === app.move?.target_server)?.name || app.move!.target_server) : '';
  const nativeLocked = Boolean(app?.native && !app.management_capabilities?.deploy);
  const tabs = app?.observed ? visibleTabs('applications', ['deployments', 'logs', 'environment', 'domains', 'data', 'backups', 'processes', 'move', 'settings']) : visibleTabs('applications', []);
  const running = deployment && ['queued', 'building'].includes(deployment.status) ? deployment : undefined;
  const actions = app && !app.observed ? <>
    {app.current?.stack?.bootstrap && <a className="button button-secondary button-md" href={`https://${app.domain}${app.current.stack.manifest?.bootstrap_readiness?.setup_path || '/setup'}`} target="_blank" rel="noreferrer">Open setup<ArrowUpRight size={13} /></a>}
    <ActionMenu label="Actions">
      <MenuLabel>Inspect</MenuLabel>
      <MenuLink icon={<Activity size={16} />} href={withSticky(indexHref('errors', { app: app.id }))}>Errors</MenuLink>
      <MenuLink icon={<ShieldCheck size={16} />} href={withSticky(indexHref('security', { app: app.id }))}>Security activity</MenuLink>
      <MenuSeparator /><MenuLabel>Release</MenuLabel>
      <ApplicationAutoDeploy appID={app.id} enabled={app.auto_deploy} disabled={!!app.retiring || !!app.stopped || nativeLocked} requireAppPairing={!!app.native} locked={held} onChange={refresh} />
      {(!app.native || app.management_capabilities?.rollback) && <MenuItem disabled={held || !!app.stopped} icon={<RotateCcw size={16} />} onSelect={() => run('rollback')}>Rollback</MenuItem>}
      <MenuLink icon={<ArrowRightLeft size={16} />} href={withSticky(resourceHref('applications', app.id, 'move'))}>Move to another server</MenuLink>
      <AppLifecycleMenu app={app} deployment={deployment} onChanged={refresh} onDeleted={async () => { notify(`${app.id} deleted. Restore it from Deleted apps within ${ARCHIVE_RETENTION_DAYS} days.`); navigate(indexHref('applications')); await refresh(); }} />
    </ActionMenu>
    {app.stopped ? <StartApplicationButton app={app} deployment={deployment} onChanged={refresh} /> : <Button variant="primary" data-action="deploy" data-id={app.id} disabled={nativeLocked || !!app.retiring || held} title={lockReason || (nativeLocked ? 'Native deployment ownership requires the reviewed host change plan' : undefined)} onClick={() => run('deploy')}><ArrowUpRight size={15} />Deploy</Button>}
  </> : undefined;
  return <ResourceDetailPage resource="applications" id={appID} tab={tab} tabs={tabs} title={app?.observed?.name || appID}
    status={app && <StatusBadge status={appState(app, deployment)} />}
    facts={app ? [app.domain && <a href={`https://${app.domain}`} target="_blank" rel="noreferrer">{app.domain}<ArrowUpRight size={12} /></a>, server ? <a href={withSticky(resourceHref('servers', server.id))}>{server.name}</a> : app.server_id, runtimeLabel(appRuntime(app)), app.branch && <>{app.branch} · <code>{releaseLabel(app)}</code></>] : undefined}
    actions={actions}
    notices={app && !app.observed ? <>
      {(actionError || deployment?.error) && <div className="alert alert-danger app-deployment-error" role="alert"><AlertTriangle size={18} /><div>{actionError ? `Request failed: ${actionError}` : <>Last deployment failed: {deployment!.error} <a href={withSticky(resourceHref('deployments', deployment!.id, 'log'))}>Open its log</a></>}</div></div>}
      {running && <Card className="app-progress-card"><LiveProgress compact label={waitingLabel(running.progress) || (running.status === 'queued' ? 'Queued' : deploymentLabel(running.kind))} startedAt={running.progress?.started_at || running.started || running.created} /><a href={withSticky(resourceHref('deployments', running.id, 'log'))}>Follow the build log</a></Card>}
      {!!app.warnings?.length && <div className="notice-stack app-warnings">{app.warnings.map(warning => <div key={warning} className="alert notice-row alert-warning" role="status"><AlertTriangle size={17} aria-hidden="true" /><span>{warning}</span></div>)}</div>}
      {lock.job && moveActive(lock.job) && <MoveLockCard appID={appID} lock={lock} />}
    </> : undefined}
    missing={!loading && !app ? <EmptyState icon={<Box />} title="Application not found" description={`No application called ${appID} is on this control panel. It may have been deleted; deleted applications can be restored for ${ARCHIVE_RETENTION_DAYS} days.`} action={<a className="button button-secondary button-md" href={withSticky(indexHref('deleted-apps'))}>Open deleted apps</a>} /> : undefined}>
    {app && tab === 'overview' && <ApplicationOverview app={app} deployment={deployment} />}
    {app && tab === 'deployments' && <ApplicationDeployments appID={appID} />}
    {app && tab === 'logs' && <LiveLog kind="runtime-logs" id={appID} />}
    {app && tab === 'environment' && <AppEnvironmentTab appID={appID} />}
    {app && tab === 'domains' && <AppDomainsTab appID={appID} />}
    {app && tab === 'data' && <AppDataTab appID={appID} />}
    {app && tab === 'backups' && <AppBackupsTab appID={appID} />}
    {app && tab === 'processes' && <ApplicationProcesses appID={appID} />}
    {app && tab === 'move' && <Card><SectionHeader title="Move to another server" description="Copies the application, its data and hostnames to another server while it keeps serving, then switches over at the final sync." /><MigrationPlan appID={app.id} serverID={app.server_id} servers={servers} held={moveActive(app.move)} onChanged={refresh} defaultOpen /></Card>}
    {app && tab === 'settings' && <AppSettingsTab appID={appID} />}
  </ResourceDetailPage>;
}

/** Overview tab: what the application is and where it stands. */
function ApplicationOverview({ app, deployment }: { app: Application; deployment?: Deployment }) {
  const { servers } = useWorkspace();
  const server = servers.find(item => item.id === app.server_id);
  if (app.observed) return <Card><SectionHeader title="Observed application" description={app.observed.missing ? 'Not present in the latest successful discovery. The previous record is retained.' : 'Discovered on a server; the panel shows it but does not manage it.'} /><SummaryList items={[{ label: 'Name', value: app.observed.name }, app.domain && { label: 'Hostname', value: app.domain }, { label: 'Source', value: servers.find(item => item.id === app.observed?.source)?.name || app.observed.source }, { label: 'Processes', value: app.observed.processes.join(', ') || 'None reported' }, { label: 'Observed', value: formatDate(app.observed.observed_at) }]} /></Card>;
  const hostnames = [app.domain, ...(app.additional_domains || [])].filter(Boolean);
  return <div className="settings-stack">
    <Card><SectionHeader title="Release" />
      <SummaryList items={[
        { label: 'Status', value: <StatusBadge status={appState(app, deployment)} /> },
        { label: 'Current release', value: <code>{releaseLabel(app)}</code> },
        { label: 'Repository', value: <>{app.repository}{app.branch ? ` · ${app.branch}` : ''}</> },
        { label: 'Auto-deploy', value: app.auto_deploy ? 'Enabled for pushes to the branch' : 'Disabled' },
        deployment && { label: 'Latest deployment', value: <a href={withSticky(resourceHref('deployments', deployment.id))}>{deployment.status} · {relativeTime(deployment.created)}</a> },
      ]} />
    </Card>
    <Card><SectionHeader title="Hosting" actions={<a className="button button-secondary button-sm" href={withSticky(resourceHref('applications', app.id, 'domains'))}>Manage domains</a>} />
      <SummaryList items={[
        { label: hostnames.length === 1 ? 'Hostname' : 'Hostnames', value: <span className="value-list">{hostnames.map(host => <a key={host} href={`https://${host}`} target="_blank" rel="noreferrer">{host}</a>)}</span> },
        app.redirect_domains?.length ? { label: 'Redirects', value: app.redirect_domains.map(item => item.host).join(', ') } : false,
        { label: 'Server', value: server ? <a href={withSticky(resourceHref('servers', server.id))}>{server.name}</a> : app.server_id },
        { label: 'Runtime', value: <>{runtimeLabel(appRuntime(app))}{app.native ? <> <Badge>native</Badge></> : null}</> },
        { label: 'Resources', value: <a href={withSticky(resourceHref('applications', app.id, 'data'))}>Database & cache</a> },
      ]} />
    </Card>
  </div>;
}

/** Deployments tab: this application's releases; each opens its deployment. */
function ApplicationDeployments({ appID }: { appID: string }) {
  const { deployments } = useWorkspace();
  const rows = [...deployments].filter(item => item.app_id === appID).sort((a, b) => +new Date(b.created) - +new Date(a.created));
  return <Card><SectionHeader title="Deployments" description="Newest first. Open one for its timeline and log." /><ResourceList rows={rows} columns={deploymentColumns(false)} rowKey={row => row.id} empty={<EmptyState title="No deployments yet" description="Deploy this application to create its first release." />} /></Card>;
}
