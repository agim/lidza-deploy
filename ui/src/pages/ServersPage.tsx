import { Plus, RefreshCw, Server as ServerIcon } from 'lucide-react';
import { createHref, navigate, resourceHref, withSticky } from '../lib/router';
import { formatBytes } from '../lib/format';
import type { Server, ServerHealth } from '../lib/types';
import { useServerHealth } from '../components/ServerResources';
import { useWorkspace } from '../state/WorkspaceContext';
import { Button, EmptyState, ErrorBanner, StatusBadge } from '../components/ui';
import type { Column } from '../components/ResourceList';
import { ResourceIndexPage, ResourceName } from '../components/resource';

/** serverStatus: online when the agent answered the latest health check. */
export function serverStatus(server: Server, health: ServerHealth | undefined, unavailable: string[], loading: boolean) {
  if (unavailable.includes(server.id) || unavailable.includes(server.name)) return 'offline';
  if (health) return health.disk_low ? 'disk low' : 'online';
  return loading ? 'checking' : 'unknown';
}

function percent(value?: number | null) { return typeof value === 'number' && Number.isFinite(value) ? `${value.toFixed(1)}%` : '—'; }

/** ServersPage: the index of servers with their live usage. */
export function ServersPage() {
  const { servers, apps } = useWorkspace();
  const metrics = useServerHealth(servers.map(server => server.id).join(','));
  const healthOf = (id: string) => metrics.health.find(row => row.server_id === id);
  const columns: Column<Server>[] = [
    { id: 'name', label: 'Name', render: server => <ResourceName href={withSticky(resourceHref('servers', server.id))} name={server.name || server.id} detail={server.url} /> },
    { id: 'status', label: 'Status', render: server => <StatusBadge status={serverStatus(server, healthOf(server.id), metrics.unavailable, metrics.busy)} /> },
    { id: 'apps', label: 'Applications', render: server => { const count = apps.filter(app => app.server_id === server.id).length; return <a href={withSticky(resourceHref('servers', server.id, 'applications'))}>{count} application{count === 1 ? '' : 's'}</a>; } },
    { id: 'cpu', label: 'CPU', render: server => percent(healthOf(server.id)?.cpu_percent) },
    { id: 'memory', label: 'Memory', render: server => { const health = healthOf(server.id); return health?.memory_total ? `${formatBytes((health.memory_total || 0) - (health.memory_available || 0))} of ${formatBytes(health.memory_total)}` : '—'; } },
    { id: 'disk', label: 'Disk', render: server => { const disk = healthOf(server.id)?.disks?.[0]; return disk ? `${percent(disk.used_percent)} used` : '—'; } },
  ];
  return <ResourceIndexPage resource="servers" title="Servers" description="The hosts that run your applications, each with its own agent." count={servers.length}
    actions={<Button onClick={metrics.refresh} pending={metrics.busy} feedback disabled={metrics.busy} aria-label="Refresh resources"><RefreshCw size={16} className={metrics.busy ? 'spin' : ''} />Refresh</Button>}
    primaryAction={<Button variant="primary" onClick={() => { navigate(createHref('servers')); }}><Plus size={17} />Connect server</Button>}
    notices={metrics.error ? <ErrorBanner>{metrics.error}</ErrorBanner> : undefined}
    rows={servers} columns={columns} rowKey={server => server.id}
    empty={<EmptyState icon={<ServerIcon />} title="Connect your first server" description="Install a Līdza agent, then add its secure connection here." action={<Button variant="primary" onClick={() => { navigate(createHref('servers')); }}>Connect server</Button>} />} />;
}
