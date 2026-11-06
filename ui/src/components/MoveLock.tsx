import { useEffect, useState, type ReactNode } from 'react';
import { AlertTriangle, ArrowUpRight, LoaderCircle, XCircle } from 'lucide-react';
import { get } from '../lib/api';
import { href, withSticky } from '../lib/router';
import { lastOutput, moveActive, moveCurrentStep, moveHolds, moveLockReason, movePhaseLabel, moveTone, stateLabels, working, type MoveFailure, type MoveJob } from '../lib/move';
import { formatElapsed, LiveProgress } from './LiveProgress';
import { Badge, Card } from './ui';

// A move copies in the background while the application stays usable
// ("Preparing move (app stays live)"); from the final sync until it finishes
// or is aborted it locks the application and the control plane refuses
// every change with 409 ("Final sync — app locked"). These pieces show that
// in the panel: one hook polls the move, a card explains the phase, a
// fieldset makes the forms read-only while locked, and a status line
// replaces the card's move summary.

export interface AppMoveLock {
  /** The application's latest move, when one is recorded. */
  job?: MoveJob;
  /** True while the move locks the application (its final sync). */
  locked: boolean;
  /** True while a move is neither finished nor aborted. */
  active: boolean;
  /** Why changes are refused, for hints and tooltips; empty when unlocked. */
  reason: string;
  /** The destination server's display name. */
  target: string;
}

export interface MoveLockOptions {
  /** Whether to ask at all; the application card asks only when the list reports a move. */
  watch?: boolean;
  /** Poll interval while the move holds the application. */
  pollMs?: number;
  /** Poll interval otherwise, to notice a move started elsewhere. */
  idleMs?: number;
}

/** moveHref opens the application's move panel on the applications page. */
export function moveHref(appID: string) {
  return `${href('applications')}?move=${encodeURIComponent(appID)}`;
}

/** useAppMoveLock follows the move of one application. */
export function useAppMoveLock(appID: string, { watch = true, pollMs = 3000, idleMs = 30_000 }: MoveLockOptions = {}): AppMoveLock {
  const [job, setJob] = useState<MoveJob>();
  const held = watch && moveActive(job);
  useEffect(() => {
    if (!watch) { setJob(undefined); return; }
    let stopped = false;
    const load = async () => {
      try {
        const next = await get<MoveJob | undefined>(`control/apps/${encodeURIComponent(appID)}/move`);
        if (!stopped) setJob(next && typeof next === 'object' && !Array.isArray(next) ? next : undefined);
      } catch { /* keep the last answer; the server enforces the lock either way */ }
    };
    void load();
    const timer = window.setInterval(() => { void load(); }, held ? pollMs : idleMs);
    return () => { stopped = true; window.clearInterval(timer); };
  }, [appID, watch, held, pollMs, idleMs]);
  const current = watch ? job : undefined;
  const target = current ? current.target_server_name || current.target_server : '';
  const locked = watch && moveHolds(current);
  return { job: current, locked, active: watch && moveActive(current), reason: locked ? moveLockReason(appID, target) : '', target };
}

/** Elapsed shows the time since a server timestamp, ticking every second. */
function Elapsed({ since }: { since?: string }) {
  const [now, setNow] = useState(Date.now);
  useEffect(() => { const timer = window.setInterval(() => setNow(Date.now()), 1000); return () => window.clearInterval(timer); }, []);
  const started = since ? Date.parse(since) : NaN;
  if (!Number.isFinite(started)) return null;
  return <span className="move-status-elapsed" title="Time since the move started">{formatElapsed(now - Math.min(started, now))}</span>;
}

/**
 * MoveStatusLine is one line of a held move: a spinner while the move works
 * (a warning or failure icon when it waits for the operator or failed),
 * the destination, the state, the current step and the elapsed time.
 */
export function MoveStatusLine({ job, target, step: showStep = true }: { job: MoveJob; target: string; step?: boolean }) {
  const step = moveCurrentStep(job);
  const icon = working(job.state)
    ? <LoaderCircle size={15} className="spin move-status-icon" aria-label="Move in progress" />
    : job.state === 'failed'
      ? <XCircle size={15} className="move-status-icon move-status-failed" aria-label="Move failed" />
      : <AlertTriangle size={15} className="move-status-icon move-status-waiting" aria-label="Move waiting for you" />;
  return <span className="move-status-line">
    {icon}
    <strong className="move-status-target">Moving to {target}</strong>
    <Badge tone={moveTone(job.state)}>{stateLabels[job.state] || job.state}</Badge>
    {moveActive(job) && <span className={moveHolds(job) ? 'move-phase move-phase-locked' : 'move-phase move-phase-live'}>{movePhaseLabel(job)}</span>}
    {showStep && step && <span className="move-status-step">{step.title}</span>}
    <Elapsed since={job.created || step?.started} />
  </span>;
}

/** MoveFailureNote shows a move's last failure: what failed, where, the cause and what to do. */
export function MoveFailureNote({ failure }: { failure: MoveFailure }) {
  const retry = failure.next_retry ? new Date(failure.next_retry) : undefined;
  return <div className={`move-failure move-failure-${failure.class}`} role="alert">
    <p className="move-failure-message">{failure.message}</p>
    <dl className="move-failure-facts">
      <dt>Step</dt><dd>{failure.step}{failure.engine ? ` · ${failure.engine}` : ''}{failure.server ? ` · ${failure.server}` : ''}</dd>
      <dt>Cause</dt><dd>{failure.class === 'transient' ? 'Transient' : failure.class === 'operator' ? 'Needs your action' : 'Failed'} ({failure.code}) · attempt {failure.attempt}/{failure.max_attempts}</dd>
      {retry && <><dt>Next attempt</dt><dd><time dateTime={failure.next_retry}>{retry.toLocaleTimeString()}</time></dd></>}
    </dl>
    {failure.remediation && <p className="move-failure-remediation"><strong>What to do:</strong> {failure.remediation}</p>}
    {failure.detail && failure.detail !== failure.message && <details className="move-failure-detail"><summary>Error detail</summary><code>{failure.detail}</code></details>}
    {failure.diagnostics && failure.diagnostics.length > 0 && <details className="move-failure-detail"><summary>Diagnostics ({failure.diagnostics.length})</summary><ul>{failure.diagnostics.map((line, index) => <li key={index}>{line}</li>)}</ul></details>}
  </div>;
}

/**
 * MoveLockCard shows, on every settings page, an active move: while it
 * copies in the background the application stays usable and the card says
 * so; during the final sync the card explains the lock.
 */
export function MoveLockCard({ appID, lock }: { appID: string; lock: AppMoveLock }) {
  if (!lock.active || !lock.job) return null;
  const job = lock.job;
  const step = moveCurrentStep(job);
  return <Card className={lock.locked ? 'move-lock-card' : 'move-lock-card move-live-card'} role="region" aria-label="Move in progress">
    <div className="move-lock-heading"><MoveStatusLine job={job} target={lock.target} step={false} /><a className="button button-secondary button-sm" href={withSticky(moveHref(appID))}>Open move<ArrowUpRight size={13} /></a></div>
    {step && (step.status === 'running' && working(job.state)
      ? <LiveProgress label={step.title} startedAt={step.started} activityAt={lastOutput(step)} activityKey={step.log?.filter(line => !line.heartbeat).length} progress={step.progress} />
      : <p className="move-lock-step">Current step: <strong>{step.title}</strong></p>)}
    {job.stall && <p className="move-stall" role="status">No progress since {new Date(job.stall.since).toLocaleTimeString()} while {job.stall.phase}; the attempt is retried if nothing moves by {new Date(job.stall.cancel_at).toLocaleTimeString()}.</p>}
    {job.failure ? <MoveFailureNote failure={job.failure} /> : job.error && <p className="move-lock-error" role="alert">{job.error}</p>}
    {lock.locked
      ? <p className="muted-copy">Settings are locked during the final sync. Finish or abort the move to change them.</p>
      : <p className="muted-copy">The application stays usable while the move copies in the background. Deploys and changes made now are picked up by the final sync, which locks the application until the move finishes.</p>}
  </Card>;
}

/** MoveLockFieldset makes every control inside read-only while a move holds the application. */
export function MoveLockFieldset({ lock, children }: { lock: AppMoveLock; children: ReactNode }) {
  return <fieldset className="move-lock-fieldset" disabled={lock.locked} title={lock.locked ? lock.reason : undefined}>{children}</fieldset>;
}
