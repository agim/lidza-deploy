import { useCallback, useEffect, useRef, useState } from 'react';
import { get } from './api';

// The activity feed (GET /api/control/activity): every operation the panel
// knows is running or waiting, and what finished recently.

export type ActivityState = 'running' | 'waiting' | 'succeeded' | 'failed';
export type ActivityKind = 'move' | 'deployment' | 'update' | 'job' | string;

export interface ActivityProgress { done: number; total: number; unit: string }

export interface ActivityItem {
  id: string;
  kind: ActivityKind;
  title: string;
  server_id?: string;
  server_name?: string;
  app_id?: string;
  state: ActivityState;
  message?: string;
  waiting_on?: string;
  started?: string;
  updated: string;
  progress: ActivityProgress | null;
  link?: string;
  details: string[];
}

export interface ActivityFeed { items: ActivityItem[]; generated?: string; unavailable_servers?: string[] }

export const OPEN_POLL_MS = 5_000;
export const CLOSED_POLL_MS = 20_000;
const DISMISSED_KEY = 'deploy.activity.dismissed';

/** activeCount is what the header badge shows: running plus waiting items. */
export function activeCount(items: ActivityItem[]) {
  return items.filter(item => item.state === 'running' || item.state === 'waiting').length;
}

/** groupActivity splits items into the drawer's three groups, dropping dismissed finished ones. */
export function groupActivity(items: ActivityItem[], dismissed: Set<string> = new Set()) {
  return {
    running: items.filter(item => item.state === 'running'),
    waiting: items.filter(item => item.state === 'waiting'),
    finished: items.filter(item => (item.state === 'succeeded' || item.state === 'failed') && !dismissed.has(item.id)),
  };
}

function formatBytes(value: number) {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let n = value, unit = 0;
  while (n >= 1024 && unit < units.length - 1) { n /= 1024; unit++; }
  return `${unit === 0 ? n : n.toFixed(n >= 100 ? 0 : 1)} ${units[unit]}`;
}

/** progressLabel describes progress in its unit ("3 of 9 steps", "1.2 GiB of 4.0 GiB"). */
export function progressLabel(progress: ActivityProgress) {
  if (progress.unit === 'bytes') return `${formatBytes(progress.done)} of ${formatBytes(progress.total)}`;
  const unit = progress.total === 1 ? progress.unit.replace(/s$/, '') : progress.unit;
  return `${progress.done} of ${progress.total} ${unit}`;
}

export function progressPercent(progress: ActivityProgress) {
  return progress.total > 0 ? Math.min(100, Math.max(0, (progress.done / progress.total) * 100)) : 0;
}

/** elapsed is a compact duration since start ("42s", "5m 03s", "2h 10m"). */
export function elapsed(from?: string, now = Date.now()) {
  if (!from) return '';
  const seconds = Math.max(0, Math.floor((now - new Date(from).valueOf()) / 1000));
  if (!Number.isFinite(seconds)) return '';
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${String(seconds % 60).padStart(2, '0')}s`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ${String(minutes % 60).padStart(2, '0')}m`;
  return `${Math.floor(hours / 24)}d ${hours % 24}h`;
}

function readDismissed(): Set<string> {
  try { const raw = localStorage.getItem(DISMISSED_KEY); return new Set(raw ? JSON.parse(raw) as string[] : []); } catch { return new Set(); }
}
function writeDismissed(ids: Set<string>) {
  try { localStorage.setItem(DISMISSED_KEY, JSON.stringify([...ids].slice(-200))); } catch { /* storage unavailable: dismissal lasts this page */ }
}

const demoItems = (): ActivityItem[] => {
  const ago = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();
  return [
    { id: 'demo-move', kind: 'move', title: 'Moving content-studio to Virginia staging', server_id: 'us-1', server_name: 'Virginia staging', app_id: 'content-studio', state: 'running', message: 'Copy data · copying postgresql content-studio', started: ago(14), updated: ago(0), progress: { done: 1288490188, total: 4294967296, unit: 'bytes' }, link: '/console/applications?move=content-studio', details: ['From Frankfurt production to Virginia staging', 'Step 4 of 9: Copy data'] },
    { id: 'demo-update', kind: 'update', title: 'Updating Virginia staging to v0.3.0', server_id: 'us-1', server_name: 'Virginia staging', state: 'waiting', message: 'Waits for Frankfurt production to finish updating', waiting_on: 'Waits for Frankfurt production to finish updating', updated: ago(2), progress: { done: 0, total: 2, unit: 'servers' }, link: '/console/updates', details: ['Server 2 of 2 in the rollout of v0.3.0'] },
    { id: 'demo-deploy', kind: 'deployment', title: 'Deployed customer-portal', server_id: 'eu-1', server_name: 'Frankfurt production', app_id: 'customer-portal', state: 'succeeded', message: 'Finished in 42s', started: ago(30), updated: ago(29), progress: null, link: '/console/deployments/demo-0', details: ['Commit a6b92fe'] },
  ];
};

/**
 * useActivity polls the feed: every 5 s while the drawer is open, every
 * 20 s otherwise, and not at all while the tab is hidden (it refreshes as
 * soon as the tab is shown again).
 */
export function useActivity(open: boolean) {
  const demo = new URLSearchParams(location.search).get('demo') === '1';
  const [items, setItems] = useState<ActivityItem[]>(() => demo ? demoItems() : []);
  const [error, setError] = useState('');
  const [loaded, setLoaded] = useState(demo);
  const [dismissed, setDismissed] = useState<Set<string>>(readDismissed);
  const busy = useRef(false);

  const refresh = useCallback(async () => {
    if (demo || busy.current || document.hidden) return;
    busy.current = true;
    try {
      const feed = await get<ActivityFeed>('control/activity');
      setItems(feed?.items || []);
      setError('');
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      busy.current = false;
      setLoaded(true);
    }
  }, [demo]);

  useEffect(() => {
    void refresh();
    const timer = window.setInterval(() => { void refresh(); }, open ? OPEN_POLL_MS : CLOSED_POLL_MS);
    const onVisible = () => { if (!document.hidden) void refresh(); };
    document.addEventListener('visibilitychange', onVisible);
    return () => { clearInterval(timer); document.removeEventListener('visibilitychange', onVisible); };
  }, [open, refresh]);

  const dismiss = useCallback((ids: string[]) => {
    setDismissed(current => { const next = new Set(current); for (const id of ids) next.add(id); writeDismissed(next); return next; });
  }, []);

  return { items, error, loaded, dismissed, dismiss, refresh };
}
