import { act, cleanup, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { LiveProgress, formatElapsed, formatRemaining } from './LiveProgress';

afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); });

const GiB = 2 ** 30, MiB = 2 ** 20;
const at = (ms: number) => new Date(ms).toISOString();

it('formats elapsed time and time left', () => {
  expect(formatElapsed(25_400)).toBe('25s');
  expect(formatElapsed(125_000)).toBe('2m 05s');
  expect(formatElapsed(3_720_000)).toBe('1h 02m');
  expect(formatRemaining(30)).toBe('less than a minute left');
  expect(formatRemaining(130)).toBe('about 2 min left');
  expect(formatRemaining(3600 + 25 * 60)).toBe('about 1 h 25 min left');
  expect(formatRemaining(7200)).toBe('about 2 h left');
  expect(formatRemaining(Number.POSITIVE_INFINITY)).toBe('');
});

it('shows a determinate bar with size, rate and time left', () => {
  let clock = Date.parse('2026-11-05T10:00:00Z');
  vi.useFakeTimers();
  const started = clock - 100_000;
  const progress = (done: number) => ({ phase: 'streaming the dump of shop to beta', done_bytes: done, total_bytes: 3.1 * GiB, started_at: at(started), updated_at: at(clock) });
  const { rerender } = render(<LiveProgress label="Copy data" startedAt={at(started)} progress={progress(1.2 * GiB)} now={() => clock} />);
  clock += 10_000;
  rerender(<LiveProgress label="Copy data" startedAt={at(started)} progress={progress(1.2 * GiB + 120 * MiB)} now={() => clock} />);
  const bar = screen.getByRole('progressbar');
  expect(bar).toHaveAttribute('aria-valuenow', '42');
  expect(bar).toHaveAttribute('aria-valuemax', '100');
  // 120 MiB in 10 s, 1.78 GiB left.
  expect(screen.getByText('1.3 GiB of 3.1 GiB · 12.0 MiB/s · about 3 min left · last activity just now')).toBeInTheDocument();
  expect(screen.getByText('Streaming the dump of shop to beta')).toBeInTheDocument();
  expect(screen.getByText('1m 50s')).toBeInTheDocument();
  expect(screen.queryByText(/Still working/)).toBeNull();
});

it('shows an indeterminate bar and a growing counter without a total', () => {
  const clock = Date.parse('2026-11-05T10:00:00Z');
  render(<LiveProgress progress={{ phase: 'dumping portal on alpha', done_bytes: 1.4 * GiB, started_at: at(clock - 200_000), updated_at: at(clock - 1000) }} now={() => clock} />);
  const bar = screen.getByRole('progressbar');
  expect(bar).not.toHaveAttribute('aria-valuenow');
  expect(bar).not.toHaveAttribute('value');
  expect(bar).toHaveAttribute('aria-valuetext', expect.stringContaining('1.4 GiB written'));
  expect(screen.getByText('Dumping portal on alpha')).toBeInTheDocument();
  expect(screen.getByText(/^1\.4 GiB written · 7\.\d MiB\/s · last activity just now$/)).toBeInTheDocument();
});

it('explains a quiet step and announces it politely without repeating every second', () => {
  let clock = Date.parse('2026-11-05T10:00:00Z');
  vi.useFakeTimers();
  render(<LiveProgress label="Restoring" startedAt={at(clock - 5000)} now={() => clock} />);
  const status = screen.getByRole('status');
  expect(status).toHaveAttribute('aria-live', 'polite');
  expect(status).toHaveTextContent('Restoring, running for 5s');
  expect(screen.queryByText(/Still working/)).toBeNull();
  clock += 3000;
  act(() => { vi.advanceTimersByTime(1000); });
  expect(screen.getByText('8s')).toBeInTheDocument();
  expect(status).toHaveTextContent('Restoring, running for 5s');
  clock += 13_000;
  act(() => { vi.advanceTimersByTime(1000); });
  expect(screen.getByText('Still working — this step prints nothing until it finishes (no new output for 21s).')).toBeInTheDocument();
  expect(status).toHaveTextContent('Restoring, running for 21s. Still working');
  expect(screen.getByText('last activity 21s ago')).toBeInTheDocument();
});

it('counts new output as activity', () => {
  let clock = Date.parse('2026-11-05T10:00:00Z');
  const { rerender } = render(<LiveProgress label="Deploying" startedAt={at(clock - 60_000)} activityAt={at(clock - 30_000)} activityKey={1} now={() => clock} />);
  expect(screen.getByText(/Still working/, { selector: '.live-progress-note' })).toBeInTheDocument();
  clock += 1000;
  rerender(<LiveProgress label="Deploying" startedAt={at(clock - 61_000)} activityAt={at(clock - 31_000)} activityKey={2} now={() => clock} />);
  expect(screen.queryByText(/Still working/, { selector: '.live-progress-note' })).toBeNull();
});

it('stops the spinner when the viewer prefers reduced motion', () => {
  vi.stubGlobal('matchMedia', (query: string) => ({ matches: query === '(prefers-reduced-motion: reduce)', media: query, addEventListener() {}, removeEventListener() {} }));
  const { container } = render(<LiveProgress compact label="Deploying" />);
  expect(container.firstElementChild).toHaveClass('live-progress', 'live-progress-compact', 'live-progress-still');
  expect(screen.getByText('Deploying')).toBeInTheDocument();
  expect(screen.getByText('0s')).toBeInTheDocument();
  cleanup();
  vi.unstubAllGlobals();
  const { container: moving } = render(<LiveProgress label="Deploying" />);
  expect(moving.firstElementChild).not.toHaveClass('live-progress-still');
});
