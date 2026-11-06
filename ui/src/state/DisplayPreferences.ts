import { api, APIError } from '../lib/api';

type Choice = string | boolean;
const text = (v: unknown): v is string => typeof v === 'string' && v.length <= 256;
const oneOf = (...choices: string[]) => (v: unknown) => typeof v === 'string' && choices.includes(v);
const rules = {
  'deploy.apps.view': oneOf('grid', 'list'),
  'deploy.apps.query': text,
  'deploy.apps.status': oneOf('all', 'observed', 'native', 'live', 'setup_required', 'building', 'queued', 'failed', 'stopped', 'not deployed'),
  'deploy.apps.runtime': oneOf('all', 'go', 'rails', 'astro', 'node', 'static'),
  'deploy.apps.server': text,
  'deploy.deployments.app': text,
  'deploy.databases.app': text,
  'deploy.backups.query': text,
  'deploy.team.query': text,
  'deploy.audit.query': text,
  'deploy.errors.app': text,
  'deploy.errors.query': text,
  'deploy.security.app': text,
  'deploy.security.query': text,
  'deploy.logs.follow': (v: unknown) => typeof v === 'boolean',
  'deploy.settings.tab': oneOf('general', 'environment', 'resources', 'processes'),
};
export type PreferenceKey = keyof typeof rules;
/** validPreference applies the stored-value rule of key to a candidate value. */
export function validPreference(key: PreferenceKey, value: unknown) { return rules[key](value); }
type Values = Partial<Record<PreferenceKey, Choice>>;
interface AccountState { subject: string; values: Record<string, unknown> }
export interface PreferenceSnapshot { values: Values; ready: boolean; error: string; pending: boolean }
export interface PreferenceTransport { read(): Promise<AccountState>; write(account: string, values: Values, keepalive?: boolean): Promise<AccountState> }
const transport: PreferenceTransport = {
  read: () => api('v1/auth/preferences', { signal: AbortSignal.timeout(5000) }),
  write: (account, values, keepalive = false) => api('v1/auth/preferences', { method: 'PATCH', body: JSON.stringify({ account, values }), keepalive, signal: AbortSignal.timeout(5000) }),
};
function checked(input: unknown): Values {
  const result: Values = {};
  if (!input || typeof input !== 'object') return result;
  for (const key of Object.keys(rules) as PreferenceKey[]) {
    const value = (input as Record<string, unknown>)[key];
    if (rules[key](value)) result[key] = value as Choice;
  }
  return result;
}
export const emptyPreferences: PreferenceSnapshot = { values: {}, ready: true, error: '', pending: false };

// PostgreSQL is the durable source. sessionStorage holds only this tab's bounded,
// unsaved display edits, and is never read until the server identifies the account.
export class DisplayPreferences {
  private snapshot: PreferenceSnapshot = { ...emptyPreferences, ready: false };
  private listeners = new Set<() => void>();
  private account = '';
  private pending: Values = {};
  private versions = new Map<PreferenceKey, number>();
  private revision = 0;
  private reading?: Promise<void>;
  private writing?: Promise<void>;
  private timer?: ReturnType<typeof setTimeout>;
  private running = false;
  constructor(private remote = transport, private buffer?: Storage, private demo = false) {}
  getSnapshot = () => this.snapshot;
  subscribe = (listener: () => void) => { this.listeners.add(listener); return () => this.listeners.delete(listener); };
  private publish(update: Partial<PreferenceSnapshot>) {
    this.snapshot = { ...this.snapshot, ...update, pending: Object.keys(this.pending).length > 0 };
    this.listeners.forEach(listener => listener());
  }
  private bufferKey() { return `lidza.display.pending.v1.${this.account}`; }
  private remember() {
    if (!this.account) return;
    try {
      if (Object.keys(this.pending).length) this.buffer?.setItem(this.bufferKey(), JSON.stringify(this.pending));
      else this.buffer?.removeItem(this.bufferKey());
    } catch { /* Storage can be disabled; server persistence still works. */ }
  }
  private schedule(delay = 250) {
    clearTimeout(this.timer);
    if (this.running) this.timer = setTimeout(() => { void this.flush().catch(() => {}); }, delay);
  }
  start() {
    this.running = true;
    if (this.demo) { this.publish({ ready: true }); return; }
    void this.flush().catch(() => {});
  }
  stop() { this.running = false; clearTimeout(this.timer); }
  set(key: PreferenceKey, value: Choice) {
    if (!rules[key](value)) return;
    this.pending[key] = value;
    this.versions.set(key, ++this.revision);
    this.remember();
    this.publish({ values: { ...this.snapshot.values, [key]: value } });
    if (!this.demo) this.schedule();
  }
  private load() {
    if (this.account) return Promise.resolve();
    if (!this.reading) this.reading = this.remote.read().then(state => {
      if (!state.subject || state.subject.length > 256) throw new Error('Account preferences are unavailable.');
      this.account = state.subject;
      let buffered: Values = {};
      try {
        const raw = this.buffer?.getItem(this.bufferKey());
        if (raw && raw.length <= 16384) buffered = checked(JSON.parse(raw));
      } catch { /* Ignore corrupt retry buffers. */ }
      this.pending = { ...buffered, ...this.pending };
      for (const key of Object.keys(this.pending) as PreferenceKey[]) if (!this.versions.has(key)) this.versions.set(key, ++this.revision);
      this.remember();
      this.publish({ values: { ...checked(state.values), ...this.pending }, ready: true, error: '' });
    }).finally(() => { this.reading = undefined; });
    return this.reading;
  }
  flush = (): Promise<void> => {
    clearTimeout(this.timer);
    if (this.demo) return Promise.resolve();
    if (this.writing) return this.writing;
    this.writing = (async () => {
      try {
        await this.load();
        while (Object.keys(this.pending).length) {
          const batch = { ...this.pending }, versions = new Map(this.versions);
          const response = await this.remote.write(this.account, batch);
          if (response.subject !== this.account) throw new APIError('Signed-in account changed.', 409);
          for (const key of Object.keys(batch) as PreferenceKey[]) if (versions.get(key) === this.versions.get(key)) delete this.pending[key];
          this.remember();
          this.publish({ values: { ...checked(response.values), ...this.pending }, error: '' });
        }
      } catch (reason) {
        if (reason instanceof APIError && reason.status === 409) {
          // Preserve the old account's retry buffer, but never display/replay
          // its choices into a newly authenticated account in this tab.
          this.remember();
          this.account = '';
          this.pending = {};
          this.versions.clear();
          this.publish({ values: {} });
        }
        this.publish({ ready: true, error: 'Display choices could not sync with your account. Changes will retry.' });
        this.schedule(15000);
        throw reason;
      } finally { this.writing = undefined; }
    })();
    return this.writing;
  };
  pagehide = () => {
    this.remember();
    // Never race a newer write against an in-flight batch. The retry buffer
    // survives a fast refresh if the browser cannot finish the current request.
    if (!this.demo && this.account && !this.writing && Object.keys(this.pending).length) {
      void this.remote.write(this.account, { ...this.pending }, true).catch(() => {});
    }
  };
}
