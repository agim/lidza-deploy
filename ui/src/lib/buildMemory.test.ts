import { expect, it } from 'vitest';
import { assessBuildMemory, buildMemorySummary, formatMiB, reserveMiB } from './buildMemory';

const GiB = 2 ** 30;
const estimate = { mib: 2560, basis: ['Līdza build with a front end ~2.5 GiB'] };

it('matches the agent policy for reserve and formatting', () => {
  expect(reserveMiB(2048)).toBe(512);
  expect(reserveMiB(16384)).toBe(1638);
  expect(formatMiB(512)).toBe('512 MiB');
  expect(formatMiB(1536)).toBe('1.5 GiB');
});

it('assesses build memory as blocker, warning or ok', () => {
  expect(assessBuildMemory(estimate, { memory_total: 2 * GiB, memory_available: GiB }, 'Small')?.level).toBe('blocker');
  expect(assessBuildMemory(estimate, { memory_total: 4 * GiB, memory_available: 2 * GiB })?.level).toBe('warning');
  const ok = assessBuildMemory(estimate, { memory_total: 16 * GiB, memory_available: 12 * GiB }, 'Big');
  expect(ok?.level).toBe('ok');
  expect(buildMemorySummary(ok!)).toBe('about 2.5 GiB (Līdza build with a front end ~2.5 GiB) · server 16.0 GiB RAM, 12.0 GiB free');
  expect(assessBuildMemory(undefined, { memory_total: GiB })).toBeUndefined();
  expect(assessBuildMemory(estimate, {})).toBeUndefined();
});
