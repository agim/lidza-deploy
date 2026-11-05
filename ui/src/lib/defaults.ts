// Workspace defaults per application runtime. The control panel returns the
// Līdza (Go) set at the top level, as before, and every set under `runtimes`.
export type DefaultsRuntime = 'go' | 'rails' | 'astro' | 'node';
export interface DefaultsProfile { values?: Record<string, string>; revision?: string }
export interface DefaultsResponse extends DefaultsProfile { runtimes?: Partial<Record<DefaultsRuntime, DefaultsProfile>> }

export const defaultsRuntimes: DefaultsRuntime[] = ['go', 'rails', 'astro', 'node'];
export const defaultsRuntimeLabels: Record<DefaultsRuntime, string> = { go: 'Līdza (Go)', rails: 'Ruby on Rails', astro: 'Astro', node: 'Node.js' };
export const maxRuntimeDefaults = 50;

export function isDefaultsRuntime(value: unknown): value is DefaultsRuntime {
  return typeof value === 'string' && (defaultsRuntimes as string[]).includes(value);
}

export function runtimeProfile(response: DefaultsResponse, runtime: DefaultsRuntime): DefaultsProfile {
  return response.runtimes?.[runtime] || (runtime === 'go' ? { values: response.values, revision: response.revision } : {});
}
