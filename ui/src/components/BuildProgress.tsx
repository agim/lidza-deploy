import { useRef } from 'react';
import { Check, LoaderCircle, X } from 'lucide-react';
import { LiveProgress, type OperationProgress } from './LiveProgress';

/** One step of a build: an "==> " line, or a "[lidza] " stage inside a Līdza build. */
export interface BuildStep { label: string; sub: boolean }

/** buildSteps lists a build log's steps in order. */
export function buildSteps(log: string): BuildStep[] {
  const steps: BuildStep[] = [];
  for (const raw of log.split('\n')) {
    const line = raw.trim();
    if (line.startsWith('==> ')) steps.push({ label: line.slice(4).trim(), sub: false });
    else if (/^\[lidza\] [a-z]/i.test(line) && !/^\[lidza\] (compressed|wrote|prerendered)\b/i.test(line)) steps.push({ label: line.slice(8).trim(), sub: true });
  }
  return steps.filter(step => step.label);
}

/** waitingLabel is what a queued or waiting deployment waits for, from its live progress. */
export function waitingLabel(progress?: OperationProgress) {
  const phase = progress?.phase?.trim();
  return phase ? phase[0].toUpperCase() + phase.slice(1) : '';
}

/**
 * BuildProgress shows where a build is: finished steps, the running step
 * with its elapsed time, and a hint when a step has been quiet for a while
 * (compilers and installers often print nothing for minutes). A deployment
 * that waits (for another build on the server, or for memory) shows why.
 */
export function BuildProgress({ log, status, started, updated, progress, now = Date.now }: { log: string; status?: string; started?: string; updated?: string; progress?: OperationProgress; now?: () => number }) {
  const steps = buildSteps(log);
  const running = status === 'queued' || status === 'building';
  const failed = status === 'failed' || status === 'cancelled';
  // Steps carry no server timestamps: a step's start is when this page saw it.
  const seen = useRef<{ count: number; stepAt: number }>({ count: 0, stepAt: now() });
  if (steps.length !== seen.current.count) seen.current = { count: steps.length, stepAt: now() };
  const waiting = running ? waitingLabel(progress) : '';
  if (!steps.length) return running ? <LiveProgress className="build-progress-waiting" label={waiting || (status === 'queued' ? 'Waiting for the build to start' : 'Building')} startedAt={waiting ? progress?.started_at || started : started} activityAt={updated} activityKey={log.length} quietHint={waiting ? 'it starts when the server is free' : 'the build has not printed its first step yet'} now={now} /> : null;
  const current = steps.length - 1;
  return <ol className="build-progress" aria-label="Build steps">
    {steps.map((step, index) => {
      const state = index < current ? 'done' : running ? 'running' : failed ? 'failed' : 'done';
      return <li key={`${index}-${step.label}`} className={`build-step ${state}${step.sub ? ' sub' : ''}`} aria-current={state === 'running' ? 'step' : undefined}>
        <span className="build-step-icon" aria-hidden="true">{state === 'running' ? <LoaderCircle size={15} className="spin" /> : state === 'failed' ? <X size={15} /> : <Check size={15} />}</span>
        {state === 'running'
          ? <LiveProgress label={step.label} spinner={false} startedAt={new Date(seen.current.stepAt).toISOString()} activityAt={updated} activityKey={log.length} progress={waiting && progress ? { ...progress, phase: waiting } : undefined} quietHint={waiting ? 'it continues when the server is free' : 'compiling and installing often print nothing until they finish'} now={now} />
          : <span className="build-step-label">{step.label}</span>}
      </li>;
    })}
  </ol>;
}
