import { usePreference } from '../state/PreferencesContext';
import { resourceHref, withSticky } from '../lib/router';
import { PackageOpen } from 'lucide-react';
import { useEffect, useMemo, useState } from 'react';
import { get } from '../lib/api';
import { formatDate, relativeTime } from '../lib/format';
import type { Deployment } from '../lib/types';
import { useWorkspace } from '../state/WorkspaceContext';
import { Card, EmptyState, ErrorBanner, Field, Loading, SectionHeader, StatusBadge } from '../components/ui';
import type { Column } from '../components/ResourceList';
import { ResourceDetailPage, ResourceIndexPage, ResourceName } from '../components/resource';
import { SummaryList } from '../components/SummaryList';
import { BuildProgress } from '../components/BuildProgress';
import { LiveLog } from './LogViews';

const kinds: Record<string, string> = { reload: 'Reload', restart: 'Restart', rollback: 'Rollback', start: 'Start', stop: 'Stop' };
export function deploymentKind(kind?: string) { return kinds[kind || ''] || 'Deploy'; }
export function shortID(id: string) { return id.length > 12 ? id.slice(0, 12) : id; }

/** deploymentColumns: the deployment table of the index and of an application's Deployments tab. */
export function deploymentColumns(showApp: boolean): Column<Deployment>[] {
  return [
    { id: 'name', label: 'Deployment', render: row => <ResourceName href={withSticky(resourceHref('deployments', row.id))} name={shortID(row.id)} detail={showApp ? row.app_id : row.commit_message} /> },
    { id: 'status', label: 'Status', render: row => <StatusBadge status={row.status} /> },
    { id: 'kind', label: 'Kind', render: row => deploymentKind(row.kind) },
    { id: 'commit', label: 'Commit', render: row => <code>{row.commit?.slice(0, 8) || 'pending'}</code> },
    { id: 'duration', label: 'Duration', render: row => `${Math.round(row.duration_seconds || 0)}s` },
    { id: 'started', label: 'Started', render: row => <span title={formatDate(row.created)}>{relativeTime(row.created)}</span> },
  ];
}

/** DeploymentsPage: the index of deployments, newest first, optionally for one application. */
export function DeploymentsPage() {
  const { deployments, apps } = useWorkspace();
  const [appID, setAppID] = usePreference<string>('deploy.deployments.app', 'all');
  const visible = useMemo(() => [...deployments].filter(item => appID === 'all' || item.app_id === appID).sort((a, b) => +new Date(b.created) - +new Date(a.created)), [deployments, appID]);
  return <ResourceIndexPage resource="deployments" title="Deployments" description="Every release, from queued to live. Live updates never reorder the table." count={deployments.length}
    filters={<Field label="Application" className="toolbar-filter"><select aria-label="Application" value={appID} onChange={event => setAppID(event.target.value)}><option value="all">All applications</option>{apps.map(app => <option key={app.id}>{app.id}</option>)}</select></Field>}
    rows={visible} columns={deploymentColumns(true)} rowKey={row => row.id}
    empty={<EmptyState icon={<PackageOpen />} title="No deployments yet" description="A deployment appears here as soon as it is queued. Deploy an application from its page." />} />;
}

/** useDeployment follows one deployment until it finishes. */
function useDeployment(id: string) {
  const { deployments } = useWorkspace();
  const [deployment, setDeployment] = useState<Deployment | undefined>(() => deployments.find(item => item.id === id));
  const [error, setError] = useState('');
  const [loaded, setLoaded] = useState(false);
  useEffect(() => {
    let active = true; let timer: number | undefined;
    const load = async () => {
      try { const current = await get<Deployment>(`control/deployments/${encodeURIComponent(id)}`); if (!active) return; setDeployment(current); setError(''); if (['queued', 'building'].includes(current.status)) timer = window.setTimeout(load, 1500); }
      catch (reason) { if (active) setError(reason instanceof Error ? reason.message : String(reason)); }
      finally { if (active) setLoaded(true); }
    };
    void load();
    return () => { active = false; if (timer) clearTimeout(timer); };
  }, [id]);
  return { deployment, error, loaded };
}

/** DeploymentDetailPage: one deployment; Overview (facts, timeline) and Log. */
export function DeploymentDetailPage({ id, tab }: { id: string; tab: string }) {
  const { deployment, error, loaded } = useDeployment(id);
  const { apps, servers } = useWorkspace();
  const app = apps.find(item => item.id === deployment?.app_id);
  const serverName = (id: string) => (servers || []).find(item => item.id === id)?.name || id;
  return <ResourceDetailPage resource="deployments" id={id} tab={tab} title={`Deployment ${shortID(id)}`} name={shortID(id)}
    status={deployment && <StatusBadge status={deployment.status} />}
    facts={deployment ? [<>{deploymentKind(deployment.kind)} of {app ? <a href={withSticky(resourceHref('applications', app.id))}>{app.id}</a> : deployment.app_id}</>, deployment.commit && <code>{deployment.commit.slice(0, 8)}</code>, <>Queued {formatDate(deployment.created)}</>] : undefined}
    notices={error && !deployment ? <ErrorBanner>{error}</ErrorBanner> : undefined}
    missing={loaded && !deployment ? <EmptyState icon={<PackageOpen />} title="Deployment not found" description="It may have been removed from the retained history." /> : undefined}>
    {tab === 'overview' && (deployment ? <div className="settings-stack">
      <Card><SectionHeader title="Summary" />
        <SummaryList items={[{ label: 'Status', value: <StatusBadge status={deployment.status} /> }, { label: 'Application', value: app ? <a href={withSticky(resourceHref('applications', app.id))}>{app.id}</a> : deployment.app_id }, { label: 'Kind', value: deploymentKind(deployment.kind) }, { label: 'Commit', value: <code>{deployment.commit || 'pending'}</code> }, deployment.commit_message && { label: 'Message', value: deployment.commit_message }, deployment.server_id && { label: 'Server', value: <a href={withSticky(resourceHref('servers', deployment.server_id))}>{serverName(deployment.server_id)}</a> }, { label: 'Queued', value: formatDate(deployment.created) }, deployment.finished && { label: 'Finished', value: formatDate(deployment.finished) }, { label: 'Duration', value: `${Math.round(deployment.duration_seconds || 0)} seconds` }, deployment.error && { label: 'Failure', value: deployment.error }]} />
      </Card>
      <BuildProgress log={deployment.log || ''} status={deployment.status} started={deployment.started || deployment.created} updated={deployment.updated} progress={deployment.progress} />
    </div> : <Loading label="Loading the deployment…" />)}
    {tab === 'log' && <LiveLog kind="build-logs" id={id} />}
  </ResourceDetailPage>;
}
