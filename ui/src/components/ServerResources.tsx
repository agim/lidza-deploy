import { Cpu, Database, HardDrive, MemoryStick } from 'lucide-react';
import { useEffect, useState } from 'react';
import { get } from '../lib/api';
import { formatBytes, formatDate } from '../lib/format';
import type { ServerHealth, ServerHealthResponse } from '../lib/types';
import { Notice, ProgressBar } from './ui';

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
  const databases = Object.entries(health.database_bytes || {}).sort(([a], [b]) => a.localeCompare(b));
  return <section className="server-resources" aria-label="Server resources">
    <div className="server-resources-heading"><h3>Server resources</h3><small>{unavailable ? 'Last successful sample' : 'Sampled'} <time dateTime={health.checked}>{formatDate(health.checked)}</time></small></div>
    {unavailable && <Notice>{unavailable} These measurements are stale.</Notice>}
    <div className="host-metrics">
      <div className="host-metric"><span><Cpu size={15} />CPU</span><strong>{cpu === undefined ? 'Sampling…' : `${cpu.toFixed(1)}%`}</strong><small>{valid(health.cpus) ? `${health.cpus} cores` : 'Core count unavailable'} · Load {valid(health.load1) ? health.load1.toFixed(2) : 'unavailable'}</small>{cpu !== undefined && <ProgressBar value={cpu} tone={cpu > 90 ? 'warning' : 'accent'} />}</div>
      <div className="host-metric"><span><MemoryStick size={15} />Memory</span><strong>{memory ? `${formatBytes(memoryUsed)} used` : 'Unavailable'}</strong><small>{memory ? `${formatBytes(health.memory_available)} available / ${formatBytes(health.memory_total)} total` : 'Waiting for memory measurements'}</small>{memory && <ProgressBar value={memoryUsed / health.memory_total! * 100} tone={memoryUsed / health.memory_total! > .9 ? 'warning' : 'accent'} />}</div>
      <div className="host-metric host-metric-wide"><span><MemoryStick size={15} />Swap</span><strong>{swap ? health.swap_total === 0 ? 'Not configured' : `${formatBytes(Math.max(0, health.swap_total! - health.swap_free!))} used / ${formatBytes(health.swap_total)}` : 'Unavailable'}</strong><small>{swap && health.swap_total! > 0 ? `${formatBytes(health.swap_free)} free` : ''}{health.swap_provisioning?.message ? ` · ${health.swap_provisioning.message}` : ''}</small></div>
      {(health.disks || []).map(disk => <div className="host-metric host-metric-wide" key={disk.path}><span><HardDrive size={15} />Disk <code>{disk.path}</code></span><strong>{valid(disk.used_percent) ? `${disk.used_percent.toFixed(1)}% used` : 'Usage unavailable'}</strong><small>{valid(disk.available) ? `${formatBytes(disk.available)} free` : 'Free space unavailable'}{valid(disk.total) ? ` / ${formatBytes(disk.total)} total` : ''}</small>{valid(disk.used_percent) && <ProgressBar value={disk.used_percent} tone={disk.used_percent > 90 ? 'danger' : disk.used_percent > 80 ? 'warning' : 'accent'} />}</div>)}
    </div>
    {!health.disks?.length && <p className="muted-copy">Disk measurements unavailable.</p>}
    <details className="host-databases"><summary><Database size={15} />Database sizes <span>{databases.length} measured</span></summary>{databases.length ? <dl>{databases.map(([id, bytes]) => <div key={id}><dt>{id}</dt><dd>{formatBytes(bytes)}</dd></div>)}</dl> : <p className="muted-copy">No database size measurements available.</p>}</details>
    {Boolean(health.errors?.length) && <Notice><div>{health.errors!.map((message, index) => <p key={index}>{message}</p>)}</div></Notice>}
  </section>;
}
