import type { BuildMemoryAssessment, BuildMemoryEstimate, ServerHealth } from './types';

/**
 * Build memory policy, as the agent applies it (internal/buildmem): while a
 * build runs, 10% of the server's memory (at least 512 MiB) stays free for
 * running applications.
 */
export function reserveMiB(totalMiB: number) { return Math.max(512, Math.floor(totalMiB / 10)); }

/** formatMiB shows 512 MiB or 1.5 GiB. */
export function formatMiB(mib: number) {
  return mib < 1024 ? `${Math.max(0, Math.round(mib))} MiB` : `${(mib / 1024).toFixed(1)} GiB`;
}

/**
 * assessBuildMemory compares a build estimate with a server's memory: a
 * blocker when the estimate exceeds the server's memory minus the reserve
 * (it certainly cannot build), a warning when it exceeds the memory that is
 * free now minus the reserve (builds wait for memory and may fail).
 * Unknown memory is not assessed.
 */
export function assessBuildMemory(estimate: BuildMemoryEstimate | undefined, health: Pick<ServerHealth, 'memory_total' | 'memory_available'> | undefined, server = 'this server'): BuildMemoryAssessment | undefined {
  if (!estimate || estimate.mib <= 0 || !health?.memory_total) return undefined;
  const total = Math.floor(health.memory_total / 2 ** 20);
  const available = Math.floor((health.memory_available || 0) / 2 ** 20);
  const reserve = reserveMiB(total);
  const need = formatMiB(estimate.mib);
  const base = { estimate, total_mib: total, available_mib: available, reserve_mib: reserve };
  if (estimate.mib > total - reserve) return { ...base, level: 'blocker', message: `Building needs about ${need} of memory, but ${server} has ${formatMiB(total)} in total and keeps ${formatMiB(reserve)} free for running applications. Choose a server with more memory.` };
  if (estimate.mib > available - reserve) return { ...base, level: 'warning', message: `Building needs about ${need} of memory; ${server} has ${formatMiB(available)} free now (${formatMiB(total)} total, ${formatMiB(reserve)} kept for running applications). Builds wait for memory to free up and may fail.` };
  return { ...base, level: 'ok', message: `Building needs about ${need} of memory; ${server} has ${formatMiB(available)} free.` };
}

/** buildMemorySummary is one line: the estimate, why, and the server's memory. */
export function buildMemorySummary(assessment: BuildMemoryAssessment) {
  const { estimate } = assessment;
  const why = estimate.basis?.length ? ` (${estimate.basis.join(', ')})` : '';
  const server = assessment.total_mib ? ` · server ${formatMiB(assessment.total_mib)} RAM, ${formatMiB(assessment.available_mib || 0)} free` : '';
  return `about ${formatMiB(estimate.mib)}${why}${server}`;
}
