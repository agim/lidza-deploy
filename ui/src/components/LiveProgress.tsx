import { useEffect, useRef, useState } from 'react';
import { LoaderCircle } from 'lucide-react';
import { formatBytes } from '../lib/format';
import { cx } from './ui';

/**
 * Progress the backend reports for a running operation: the current phase,
 * bytes done in it and, when known, the total. Times are server timestamps:
 * started_at is when the phase began, updated_at when done_bytes last
 * changed.
 */
export interface OperationProgress {
  phase: string;
  done_bytes: number;
  total_bytes?: number;
  started_at?: string;
  updated_at?: string;
}

/** After this much silence a running step explains that it is still working. */
export const quietAfterMs = 20_000;
/** Screen readers hear the status line at most this often. */
const announceEveryMs = 15_000;
/** Transfer rates average over this recent window. */
const rateWindowMs = 30_000;

/** formatElapsed shows a duration as 25s, 2m 05s or 1h 02m. */
export function formatElapsed(ms: number) {
  const seconds = Math.max(0, Math.floor(ms / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${String(seconds % 60).padStart(2, '0')}s`;
  return `${Math.floor(minutes / 60)}h ${String(minutes % 60).padStart(2, '0')}m`;
}

/** formatRemaining shows an estimate of the time left. */
export function formatRemaining(seconds: number) {
  if (!Number.isFinite(seconds) || seconds < 0) return '';
  if (seconds < 60) return 'less than a minute left';
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `about ${minutes} min left`;
  const hours = Math.floor(minutes / 60);
  return `about ${hours} h${minutes % 60 ? ` ${minutes % 60} min` : ''} left`;
}

/** formatRate shows bytes per second, for example 12.0 MiB/s. */
export function formatRate(bytesPerSecond: number) {
  return `${formatBytes(bytesPerSecond)}/s`;
}

function parse(value?: string | number) {
  if (value === undefined || value === '') return undefined;
  const ms = typeof value === 'number' ? value : Date.parse(value);
  return Number.isFinite(ms) ? ms : undefined;
}

function prefersReducedMotion() {
  try { return typeof window !== 'undefined' && !!window.matchMedia?.('(prefers-reduced-motion: reduce)').matches; } catch { return false; }
}

function capitalize(text: string) { return text ? text[0].toUpperCase() + text.slice(1) : text; }

export interface LiveProgressProps {
  /** What is running, for example a step title. The progress phase is shown below it. */
  label?: string;
  /** When the step started (server time); otherwise when this view first saw it. */
  startedAt?: string;
  /** Newest output of the step (server time), for example its last log line. */
  activityAt?: string | number;
  /** Changes whenever new output arrives (for example the log length); seen client-side. */
  activityKey?: unknown;
  progress?: OperationProgress;
  /** Why the step can be silent; shown after quietAfterMs without output. */
  quietHint?: string;
  /** Compact: spinner, label and elapsed time on one line, for cards and summaries. */
  compact?: boolean;
  /** Hide the spinner when the surrounding row already shows one. */
  spinner?: boolean;
  className?: string;
  /** Clock, for tests. */
  now?: () => number;
}

/**
 * LiveProgress shows that a running job or step is working: an animated
 * spinner (still under reduced motion), time elapsed since it started, when
 * it last showed activity, a calm note when it has been silent for a while,
 * and a progress bar when the backend reports bytes: determinate with size,
 * rate and time left when the total is known, or an indeterminate bar with a
 * growing counter when it is not.
 */
export function LiveProgress({ label, startedAt, activityAt, activityKey, progress, quietHint = 'this step prints nothing until it finishes', compact = false, spinner = true, className, now = Date.now }: LiveProgressProps) {
  const [, tick] = useState(0);
  const [still, setStill] = useState(prefersReducedMotion);
  // keyAt is when activityKey last changed in this view; its first value is no news.
  const seen = useRef({ at: now(), key: activityKey, keyAt: 0 });
  const samples = useRef<Array<{ at: number; done: number }>>([]);
  const announced = useRef({ at: 0, text: '', phase: '' });

  useEffect(() => {
    const timer = window.setInterval(() => tick(value => value + 1), 1000);
    return () => window.clearInterval(timer);
  }, []);
  useEffect(() => {
    let query: MediaQueryList | undefined;
    try { query = window.matchMedia?.('(prefers-reduced-motion: reduce)'); } catch { query = undefined; }
    if (!query?.addEventListener) return;
    const change = (event: MediaQueryListEvent) => setStill(event.matches);
    query.addEventListener('change', change);
    return () => query?.removeEventListener('change', change);
  }, []);

  const current = now();
  if (!Object.is(activityKey, seen.current.key)) seen.current = { ...seen.current, key: activityKey, keyAt: current };
  // Server times later than this clock (skew) count as now.
  const clamp = (ms?: number) => ms === undefined ? undefined : Math.min(ms, current);
  const started = clamp(parse(startedAt)) ?? seen.current.at;
  const activity = Math.max(started, seen.current.keyAt, clamp(parse(activityAt)) ?? 0, clamp(parse(progress?.updated_at)) ?? 0);
  const quietFor = current - activity;
  const quiet = quietFor >= quietAfterMs;

  const done = Math.max(0, progress?.done_bytes || 0);
  const total = Math.max(0, progress?.total_bytes || 0);
  const determinate = !!progress && total > 0;
  const percent = determinate ? Math.min(100, Math.floor(done / total * 100)) : 0;

  // Rate over the recent window from the backend's own timestamps.
  let rate = 0;
  if (progress) {
    const at = clamp(parse(progress.updated_at)) ?? current;
    const list = samples.current;
    if (list.length && done < list[list.length - 1].done) list.length = 0; // a new phase
    if (!list.length || list[list.length - 1].done !== done) list.push({ at, done });
    while (list.length > 2 && at - list[1].at > rateWindowMs) list.shift();
    const first = list[0], last = list[list.length - 1];
    if (list.length > 1 && last.at - first.at >= 2000) rate = (last.done - first.done) / ((last.at - first.at) / 1000);
    else {
      const phaseStart = clamp(parse(progress.started_at));
      if (phaseStart !== undefined && at - phaseStart >= 2000) rate = done / ((at - phaseStart) / 1000);
    }
  }

  const elapsed = formatElapsed(current - started);
  const phase = progress?.phase ? capitalize(progress.phase) : '';
  const title = label || phase || 'Working';
  const parts: string[] = [];
  if (progress && determinate) {
    parts.push(`${formatBytes(done)} of ${formatBytes(total)}`);
    if (rate > 0 && done < total) parts.push(formatRate(rate), formatRemaining((total - done) / rate));
  } else if (progress && done > 0) {
    parts.push(`${formatBytes(done)} written`);
    if (rate > 0) parts.push(formatRate(rate));
  }
  const measure = parts.filter(Boolean).join(' · ');
  const lastActivity = quietFor < 5000 ? 'last activity just now' : `last activity ${formatElapsed(quietFor)} ago`;
  const quietNote = `Still working — ${quietHint} (no new output for ${formatElapsed(quietFor)}).`;

  // The status line is announced politely, at most every announceEveryMs
  // or when the phase changes, never every second.
  const spoken = [`${label && phase ? `${label}: ${phase}` : title}, running for ${elapsed}`, measure, quiet ? quietNote : ''].filter(Boolean).join('. ');
  const announcedPhase = progress?.phase || label || '';
  if (announced.current.phase !== announcedPhase || current - announced.current.at >= announceEveryMs) announced.current = { at: current, text: spoken, phase: announcedPhase };

  const icon = spinner && <LoaderCircle size={compact ? 14 : 15} className="live-progress-spinner" aria-hidden="true" />;
  if (compact) {
    return <span className={cx('live-progress', 'live-progress-compact', still && 'live-progress-still', className)} title={measure || undefined}>
      {icon}<span className="live-progress-label">{title}</span><span className="live-progress-elapsed">{elapsed}</span>
      <span className="sr-only" role="status" aria-live="polite">{announced.current.text}</span>
    </span>;
  }
  return <div className={cx('live-progress', still && 'live-progress-still', quiet && 'live-progress-quiet', className)}>
    <div className="live-progress-heading">
      {icon}
      <span className="live-progress-label">{title}</span>
      <span className="live-progress-elapsed" title="Time since this step started">{elapsed}</span>
    </div>
    {label && phase && <p className="live-progress-phase">{phase}</p>}
    {progress && (determinate
      ? <progress className="live-progress-bar" value={percent} max={100} role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={percent} aria-valuetext={measure} aria-label={title} />
      : <progress className="live-progress-bar indeterminate" role="progressbar" aria-valuetext={measure || 'In progress'} aria-label={title} />)}
    <p className="live-progress-detail">{[measure, lastActivity].filter(Boolean).join(' · ')}</p>
    {quiet && <p className="live-progress-note">{quietNote}</p>}
    <span className="sr-only" role="status" aria-live="polite">{announced.current.text}</span>
  </div>;
}
