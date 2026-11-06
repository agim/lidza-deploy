import { act, cleanup, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { BuildProgress, buildSteps } from './BuildProgress';

afterEach(() => { cleanup(); vi.useRealTimers(); });

const log = ['release', '==> fetching 4c0c6f9', '==> install lidza', 'go: downloading github.com/jackc/pgx/v5 v5.11.0', '==> lidza build', '[lidza] npm run build', 'prerendered /', '[lidza] compressed 7 file(s)', '[lidza] go build'].join('\n');

it('lists engine steps and Līdza stages, ignoring tool chatter', () => {
  expect(buildSteps(log)).toEqual([
    { label: 'fetching 4c0c6f9', sub: false }, { label: 'install lidza', sub: false }, { label: 'lidza build', sub: false },
    { label: 'npm run build', sub: true }, { label: 'go build', sub: true },
  ]);
});

it('marks the running step, counts its time and explains a quiet step', () => {
  let clock = 1_000_000;
  vi.useFakeTimers();
  render(<BuildProgress log={log} status="building" now={() => clock} />);
  const current = screen.getByText('go build').closest('li')!;
  expect(current).toHaveAttribute('aria-current', 'step');
  expect(screen.getByText('fetching 4c0c6f9').closest('li')).toHaveClass('done');
  expect(current).not.toHaveTextContent('Still working');
  clock += 25_000;
  act(() => { vi.advanceTimersByTime(1000); });
  expect(current).toHaveTextContent('25s');
  expect(current).toHaveTextContent('Still working — compiling and installing often print nothing until they finish (no new output for 25s).');
  expect(screen.getByRole('status')).toHaveTextContent('no new output for 25s');
});

it('counts server-reported output as activity', () => {
  let clock = Date.parse('2026-11-05T10:00:00Z');
  vi.useFakeTimers();
  const { rerender } = render(<BuildProgress log={log} status="building" now={() => clock} />);
  clock += 25_000;
  rerender(<BuildProgress log={log} status="building" updated="2026-11-05T10:00:20Z" now={() => clock} />);
  expect(screen.getByText('go build').closest('li')).not.toHaveTextContent('Still working');
  expect(screen.getByText('go build').closest('li')).toHaveTextContent('last activity 5s ago');
});

it('shows the last step as failed when the build failed', () => {
  render(<BuildProgress log={log} status="failed" />);
  expect(screen.getByText('go build').closest('li')).toHaveClass('failed');
  expect(screen.getByText('install lidza').closest('li')).toHaveClass('done');
});

it('shows what a waiting deployment waits for', () => {
  const progress = { phase: 'waiting for another build on this server (deploying shop)', done_bytes: 0, started_at: new Date(1_000_000).toISOString() };
  render(<BuildProgress log="" status="queued" progress={progress} now={() => 1_005_000} />);
  expect(screen.getByText('Waiting for another build on this server (deploying shop)')).toBeInTheDocument();
  cleanup();
  render(<BuildProgress log={'==> fetching 4c0c6f9'} status="building" progress={{ ...progress, phase: 'waiting for memory to build blog (npm ci needs ~1.4 GiB, 900 MiB free)' }} now={() => 1_005_000} />);
  expect(screen.getByText('Waiting for memory to build blog (npm ci needs ~1.4 GiB, 900 MiB free)')).toBeInTheDocument();
  cleanup();
  render(<BuildProgress log="" status="queued" now={() => 1_005_000} />);
  expect(screen.getByText('Waiting for the build to start')).toBeInTheDocument();
});
