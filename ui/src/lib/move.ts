import type { OperationProgress } from '../components/LiveProgress';

// A move of an application to another server, as the control panel reports
// it (GET /api/control/apps/{id}/move and /api/control/moves/{id}).

export type MoveLogLine = { time: string; message: string; heartbeat?: boolean };
export type MoveStep = { key: string; title: string; status: string; started?: string; progress?: OperationProgress; log: MoveLogLine[] };

/** Retained data on the destination that stopped the Prepare step; the panel offers its removal. */
export type MoveRetainedData = { id: string; server: string; app_id: string; databases: number; last_backup?: string; offsite?: boolean; final_backup_required: boolean; label: string };

/** A step's last failure: classification, attempts and what to do. */
export type MoveFailure = {
  step: string;
  engine?: string;
  server?: string;
  class: 'transient' | 'operator' | 'fatal' | string;
  code: string;
  message: string;
  detail?: string;
  remediation?: string;
  attempt: number;
  max_attempts: number;
  next_retry?: string;
  time: string;
  diagnostics?: string[];
  retained?: MoveRetainedData;
};

/** A table whose data the source's backups leave out, with the operator's choice. */
export type MoveExcludedTable = { name: string; rows: number; bytes: number; choice: '' | 'copy' | 'leave'; copied_rows?: number };

export type MoveJob = {
  id: string;
  app_id: string;
  source_server: string;
  target_server: string;
  target_runtime: string;
  runtime: string;
  state: string;
  error?: string;
  steps: MoveStep[];
  hostnames: string[];
  addresses: string[];
  dns: Array<{ hostname: string; addresses: string[]; ready: boolean; error?: string }>;
  dns_checked?: string;
  dns_ahead?: boolean;
  tls_ready: boolean;
  tls_detail?: string;
  records: Array<{ hostname: string; type: string; value: string }>;
  actions: string[];
  source_frozen?: boolean;
  final_synced?: boolean;
  switched: boolean;
  created?: string;
  /** The destination's display name. */
  target_server_name?: string;
  /** The final-sync lock; phase is live (background copy), locked or done. */
  locked?: boolean;
  phase?: 'live' | 'locked' | 'done';
  failure?: MoveFailure;
  stall?: { since: string; phase: string; done_bytes: number; total_bytes?: number; cancel_at: string };
  lock_estimate?: { seconds: number; basis: string; confirm: boolean };
  destination_stale?: boolean;
  source_changes?: Array<{ time: string; kind: string; detail: string }>;
  databases?: Array<{ source_id: string; target_id?: string; copyable: boolean; excluded_tables?: MoveExcludedTable[] }>;
};

/** What the application list reports of an application's active move. */
export type MoveSummary = { id: string; state: string; source_server: string; target_server: string; locked?: boolean; phase?: string; step?: string; error?: string; retry?: string };

export const terminal = (state?: string) => state === 'finished' || state === 'aborted';
// States in which the move's runner works on its steps.
export const working = (state?: string) => state === 'running' || state === 'syncing' || state === 'finishing' || state === 'aborting';
// The newest line the step itself logged; heartbeats only repeat progress.
export const lastOutput = (step: MoveStep) => [...step.log].reverse().find(line => !line.heartbeat)?.time;
export const stateLabels: Record<string, string> = { running: 'In progress', failed: 'Failed', cutover: 'Waiting for DNS', ready: 'Ready to finish', syncing: 'Final sync', finishing: 'Finishing', finished: 'Finished', aborting: 'Aborting', aborted: 'Aborted' };

/** moveTone is the badge tone of a move state. */
export const moveTone = (state: string): 'danger' | 'success' | 'warning' => state === 'failed' ? 'danger' : terminal(state) || state === 'ready' ? 'success' : 'warning';

/** moveActive reports a move that is neither finished nor aborted. */
export const moveActive = (job?: { state?: unknown }) => !!job && typeof job.state === 'string' && job.state !== '' && !terminal(job.state);

/**
 * moveHolds reports whether a move locks its application. The application
 * stays usable while the move copies in the background; the lock starts at
 * the final sync and lasts until the move finishes or is aborted (also
 * while a failed final sync waits). A server that does not report the lock
 * holds the application for the whole move.
 */
export const moveHolds = (job?: { state?: unknown; locked?: unknown }) => moveActive(job) && (typeof job!.locked === 'boolean' ? job!.locked : true);

/** movePhaseLabel names the move's phase for summaries and cards. */
export const movePhaseLabel = (job?: { state?: unknown; locked?: unknown }) => !moveActive(job) ? '' : moveHolds(job) ? 'Final sync — app locked' : 'Preparing move (app stays live)';

/** moveCurrentStep is the step a move is on: running, failed or waiting, else the first not done. */
export function moveCurrentStep(job: MoveJob): MoveStep | undefined {
  const steps = Array.isArray(job.steps) ? job.steps : [];
  return steps.find(step => step.status === 'running' || step.status === 'failed' || step.status === 'waiting') || steps.find(step => step.status === 'pending');
}

/** moveLockReason explains why an application's settings are locked. */
export const moveLockReason = (appID: string, target: string) => `${appID} is in the final sync of its move to ${target}; finish or abort the move first.`;

/** moveDuration is a duration in seconds as "2h 5m", "12m" or "40s". */
export function moveDuration(seconds: number) {
  const s = Math.max(0, Math.round(seconds));
  if (s < 60) return `${s}s`;
  const minutes = Math.round(s / 60);
  if (minutes < 60) return `${minutes}m`;
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
}
