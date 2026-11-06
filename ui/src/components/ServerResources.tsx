import { AlertTriangle, Boxes, Cpu, Database, HardDrive, Info, MemoryStick, Server } from 'lucide-react';
import { useEffect, useState } from 'react';
import { get } from '../lib/api';
import { formatBytes, formatDate } from '../lib/format';
import type { ServerHealth, ServerHealthResponse } from '../lib/types';
import { Badge, Notice, ProgressBar } from './ui';

export function useServerHealth(serverIDs: string) {
  const [health, setHealth] = useState<ServerHealth[]>([]);
  const [unavailable, setUnavailable] = useState<string[]>([]);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [refreshKey, setRefreshKey] = useState(0);
  useEffect(() => {
    if (!serverIDs) return;
    let active = true;
    let timer: ReturnType<typeof setTimeout>;
    const load = async () => {
      if (!active) return;
      if (!document.hidden) {
        setBusy(true);
        try {
          const value = await get<ServerHealthResponse>('control/server-health');
          if (!active) return;
          // Keep the last sample for offline hosts, clearly labelled stale below.
          setHealth(previous => [...value.servers, ...previous.filter(row => !value.servers.some(next => next.server_id === row.server_id))]);
          setUnavailable([...(value.unavailable_servers || []), ...serverIDs.split(',').filter(id => !value.servers.some(row => row.server_id === id))]);
          setError('');
        } catch (reason) {
          if (!active) return;
          setError(reason instanceof Error ? reason.message : String(reason));
        } finally { if (active) setBusy(false); }
      }
      if (active) timer = setTimeout(load, 15_000);
    };
    void load();
    return () => { active = false; clearTimeout(timer); };
  }, [serverIDs, refreshKey]);
  return { health, unavailable, error, busy, refresh: () => setRefreshKey(current => current + 1) };
}

function valid(value: number | null | undefined): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0;
}

export function ServerResources({ health, loading, unavailable }: { health?: ServerHealth; loading: boolean; unavailable?: string }) {
  if (!health) return <div className="server-resources"><h3>Server resources</h3><p className="muted-copy" role="status">{unavailable || (loading ? 'Loading server measurements…' : 'No server measurements available yet.')}</p></div>;
  const cpu = valid(health.cpu_percent) ? health.cpu_percent : undefined;
  const memory = valid(health.memory_total) && health.memory_total > 0 && valid(health.memory_available);
  const memoryUsed = memory ? Math.max(0, health.memory_total! - health.memory_available!) : 0;
  const swap = valid(health.swap_total) && valid(health.swap_free);
  const retained = health.retained_databases || {};
  const databases = [...new Set([...Object.keys(health.database_bytes || {}), ...Object.keys(retained)])].sort((a, b) => a.localeCompare(b))
    .map(id => ({ id, bytes: health.database_bytes?.[id], retained: id in retained ? retained[id] : undefined }));
  const systemLogs = Object.entries(health.clickhouse_system_logs || {}).sort(([a], [b]) => a.localeCompare(b));
  return <section className="server-resources" aria-label="Server resources">
    <div className="server-resources-heading"><h3>Server resources</h3>{health.disk_low && <Badge tone="danger">Disk low</Badge>}<small>{unavailable ? 'Last successful sample' : 'Sampled'} <time dateTime={health.checked}>{formatDate(health.checked)}</time></small></div>
    {unavailable && <Notice>{unavailable} These measurements are stale.</Notice>}
    <div className="host-metrics">
      <div className="host-metric"><span><Cpu size={15} />CPU</span><strong>{cpu === undefined ? 'Sampling…' : `${cpu.toFixed(1)}%`}</strong><small>{valid(health.cpus) ? `${health.cpus} cores` : 'Core count unavailable'} · Load {valid(health.load1) ? health.load1.toFixed(2) : 'unavailable'}</small>{cpu !== undefined && <ProgressBar value={cpu} tone={cpu > 90 ? 'warning' : 'accent'} />}</div>
      <div className="host-metric"><span><MemoryStick size={15} />Memory</span><strong>{memory ? `${formatBytes(memoryUsed)} used` : 'Unavailable'}</strong><small>{memory ? `${formatBytes(health.memory_available)} available / ${formatBytes(health.memory_total)} total` : 'Waiting for memory measurements'}</small>{memory && <ProgressBar value={memoryUsed / health.memory_total! * 100} tone={memoryUsed / health.memory_total! > .9 ? 'warning' : 'accent'} />}</div>
      <div className="host-metric host-metric-wide"><span><MemoryStick size={15} />Swap</span><strong>{swap ? health.swap_total === 0 ? 'Not configured' : `${formatBytes(Math.max(0, health.swap_total! - health.swap_free!))} used / ${formatBytes(health.swap_total)}` : 'Unavailable'}</strong><small>{swap && health.swap_total! > 0 ? `${formatBytes(health.swap_free)} free` : ''}{health.swap_provisioning?.message ? ` · ${health.swap_provisioning.message}` : ''}</small></div>
      {(health.disks || []).map(disk => <div className="host-metric host-metric-wide" key={disk.path}><span><HardDrive size={15} />Disk <code>{disk.path}</code></span><strong>{valid(disk.used_percent) ? `${disk.used_percent.toFixed(1)}% used` : 'Usage unavailable'}</strong><small>{valid(disk.available) ? `${formatBytes(disk.available)} free` : 'Free space unavailable'}{valid(disk.total) ? ` / ${formatBytes(disk.total)} total` : ''}</small>{valid(disk.used_percent) && <ProgressBar value={disk.used_percent} tone={disk.used_percent >= 95 ? 'danger' : disk.used_percent >= 85 ? 'warning' : 'accent'} />}</div>)}
    </div>
    {!health.disks?.length && <p className="muted-copy">Disk measurements unavailable.</p>}
    <details className="host-databases"><summary><Database size={15} />Database sizes <span>{databases.length} measured</span></summary>{databases.length ? <dl>{databases.map(row => <div key={row.id}><dt>{row.id}</dt><dd>{databaseSize(row)}</dd></div>)}</dl> : <p className="muted-copy">No database size measurements available.</p>}{systemLogs.length > 0 && <dl aria-label="ClickHouse system logs">{systemLogs.map(([id, bytes]) => <div key={id}><dt>{id === 'host' ? 'Host ClickHouse system logs' : `${id} ClickHouse system logs`}</dt><dd>{formatBytes(bytes)}</dd></div>)}</dl>}</details>
    <RuntimeSections health={health} />
    {Boolean(health.warnings?.length) && <div className="alert alert-warning" role="status" aria-label="Server warnings"><AlertTriangle size={18} /><div>{health.warnings!.map(message => <p key={message}>{message}</p>)}</div></div>}
    {Boolean(health.notes?.length) && <details className="host-databases host-notes" aria-label="Server notes"><summary><Info size={15} />Notes <span>{health.notes!.length} note{health.notes!.length === 1 ? '' : 's'}</span></summary>{health.notes!.map(message => <p className="muted-copy" key={message}>{message}</p>)}</details>}
    {Boolean(health.errors?.length) && <Notice><div>{health.errors!.map((message, index) => <p key={index}>{message}</p>)}</div></Notice>}
  </section>;
}

/** A live measurement, or for a retained database (no application uses it any more) its last backup when it no longer answers. */
function databaseSize({ bytes, retained }: { bytes?: number; retained?: { backup_bytes?: number; backup_created?: string } }) {
  if (!retained) return formatBytes(bytes);
  if (valid(bytes)) return `retained · ${formatBytes(bytes)}`;
  return retained.backup_bytes ? `retained · last backup ${formatBytes(retained.backup_bytes)}` : 'retained · no backups';
}

/** Native services are reported whenever the server has systemd; Docker only when it is installed. */
function RuntimeSections({ health }: { health: ServerHealth }) {
  const native = health.native;
  const docker = health.docker;
  if (!native && !docker) return null;
  const failed = native ? [...new Set([...(native.failed_units || []), ...(native.failed_host_units || [])])].sort() : [];
  const releases = [...(native?.releases || [])].sort((a, b) => b.bytes - a.bytes);
  return <>
    {native && <details className="host-databases" aria-label="Native services"><summary><Server size={15} />Native services <span>{native.units} service{native.units === 1 ? '' : 's'} · {native.active} active{failed.length ? ` · ${failed.length} failed` : ''}</span></summary>
      <dl>
        <div><dt>Applications</dt><dd>{native.applications}</dd></div>
        <div><dt>Active / inactive services</dt><dd>{native.active} / {native.inactive}</dd></div>
        <div><dt>Journal on disk</dt><dd>{native.journal_bytes ? formatBytes(native.journal_bytes) : 'Unavailable'}</dd></div>
        {failed.map(unit => <div key={unit}><dt><code>{unit}</code></dt><dd>Failed</dd></div>)}
        {releases.map(row => <div key={row.app_id}><dt>{row.app_id} releases <small>{row.releases} kept</small></dt><dd>{formatBytes(row.bytes)}</dd></div>)}
      </dl>
      {!releases.length && <p className="muted-copy">Release sizes are measured in the background and appear shortly.</p>}
    </details>}
    {docker && <details className="host-databases" aria-label="Containers"><summary><Boxes size={15} />Containers <span>{docker.running} running / {docker.containers} total</span></summary><dl><div><dt>Docker</dt><dd>{docker.version || 'Not answering'}</dd></div></dl></details>}
  </>;
}
