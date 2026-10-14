import { FileClock, PackageOpen } from 'lucide-react';
import { useMemo, useState } from 'react';
import { formatDate } from '../lib/format';
import type { Deployment } from '../lib/types';
import { useWorkspace } from '../state/WorkspaceContext';
import { Button, Card, EmptyState, Field, PageHeader, StatusBadge, Toolbar } from '../components/ui';
import { ResourceList, type Column } from '../components/ResourceList';

export function DeploymentsPage() {
  const { deployments, apps } = useWorkspace();
  const [appID, setAppID] = useState('all');
  const visible = useMemo(() => [...deployments].filter(item => appID === 'all' || item.app_id === appID).sort((a, b) => +new Date(b.created) - +new Date(a.created)), [deployments, appID]);
  const columns: Column<Deployment>[] = [
    { id: 'app', label: 'Application', render: row => <strong>{row.app_id}</strong> },
    { id: 'status', label: 'Status', render: row => <StatusBadge status={row.status} /> },
    { id: 'commit', label: 'Commit', render: row => <code>{row.commit?.slice(0, 8) || 'pending'}</code> },
    { id: 'started', label: 'Started', render: row => formatDate(row.created) },
    { id: 'result', label: 'Result', render: row => <div className="row-result"><span>{Math.round(row.duration_seconds || 0)}s · {row.error || row.commit_message || 'Completed'}</span><Button size="sm" onClick={() => { location.hash = `page/build-logs/${encodeURIComponent(row.id)}`; }}><FileClock size={15} />View logs</Button></div> },
  ];
  return <><PageHeader eyebrow="Release history" title="Deployments" description="Every release, from queued to live, without live updates moving the table." /><Card><Toolbar><h2>Release history</h2><Field label="Application" className="toolbar-filter"><select value={appID} onChange={event => setAppID(event.target.value)}><option value="all">All applications</option>{apps.map(app => <option key={app.id}>{app.id}</option>)}</select></Field></Toolbar><ResourceList rows={visible} columns={columns} rowKey={row => row.id} empty={<EmptyState icon={<PackageOpen />} title="No deployments yet" description="A deployment appears here as soon as it is queued." />} /></Card></>;
}
