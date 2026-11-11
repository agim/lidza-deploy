import type { AppSettings, Deployment } from './types';

// What saving application settings does (internal/agent/settings_apply.go).
// The agent sends its table as apply_effects with the settings; the console
// only reads it to label the Save button, and reports the save's own result.

export type SettingsEffect = 'immediate' | 'restart' | 'deploy';
export type SettingsApplied = SettingsEffect | 'saved_restart_required';

export interface SettingsApplyEffects {
  fields: Record<string, SettingsEffect>;
  /** The effect of a variable's value change, except build_env_prefixes (a deploy). */
  env: SettingsEffect;
  build_env_prefixes?: string[];
}

export interface SettingsApplyResult {
  applied: SettingsApplied;
  deployment?: Deployment;
  deployment_id?: string;
  message: string;
  /** The save succeeded but the restart or deployment did not start. */
  error?: string;
  changes?: string[];
}

const rank: Record<SettingsEffect, number> = { immediate: 1, restart: 2, deploy: 3 };
const same = (a: unknown, b: unknown) => JSON.stringify(a ?? null) === JSON.stringify(b ?? null);

/** settingsChanges names the fields edited from before to after, and env:NAME for each variable whose value changes. */
export function settingsChanges(before: AppSettings | undefined, after: AppSettings | undefined, envKeys: string[] = []): string[] {
  if (!before || !after) return [];
  const changes: string[] = [];
  const fields: Array<[string, (value: AppSettings) => unknown]> = [
    ['branch', value => value.branch], ['domain', value => value.domain], ['build', value => value.build?.source_directory || ''],
    ['astro', value => value.astro ? { mode: value.astro.mode || 'static', output_directory: value.astro.output_directory || 'dist' } : null],
    ['runtime_profile', value => value.runtime_profile || ''], ['additional_domains', value => value.additional_domains || []],
    ['customer_hosts', value => Boolean(value.customer_hosts)], ['backup_before_deploy', value => value.backup_before_deploy !== false],
  ];
  for (const [field, read] of fields) if (!same(read(before), read(after))) changes.push(field);
  return [...changes, ...envKeys.map(key => `env:${key}`)];
}

/** settingsEffect is the strongest effect of changes under effects; '' when nothing changes or the agent sends no table. */
export function settingsEffect(effects: SettingsApplyEffects | undefined, changes: string[]): SettingsEffect | '' {
  if (!effects) return '';
  let strongest: SettingsEffect | '' = '';
  for (const change of changes) {
    let effect: SettingsEffect;
    if (change.startsWith('env:')) {
      const key = change.slice(4);
      effect = (effects.build_env_prefixes || []).some(prefix => key.startsWith(prefix)) ? 'deploy' : effects.env;
    } else effect = effects.fields[change] || 'deploy';
    if (!strongest || rank[effect] > rank[strongest]) strongest = effect;
  }
  return strongest;
}

/** saveLabel is the Save button's label: "Save and deploy" when the form holds a build-affecting change. */
export function saveLabel(effect: SettingsEffect | '', fallback: string) {
  return effect === 'deploy' ? 'Save and deploy' : fallback;
}

/** settingsSavedMessage is the toast shown when a save returns. */
export function settingsSavedMessage(result: SettingsApplyResult | undefined) {
  if (!result) return 'Settings saved.';
  if (result.error) return `${result.message} ${result.error}`;
  if (result.applied === 'restart' && result.deployment_id) return 'Settings saved. Restarting…';
  if (result.applied === 'deploy' && result.deployment_id) return 'Settings saved. Deploying…';
  return result.message || 'Settings saved.';
}

/** restartOutcome is the message when a followed restart finishes. */
export function restartOutcome(deployment: Deployment) {
  return deployment.status === 'live' ? 'Restarted.' : `Restart failed: ${deployment.error || deployment.status}`;
}
