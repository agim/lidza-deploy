import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { DisplayPreferences, type PreferenceTransport } from './DisplayPreferences';
import { APIError } from '../lib/api';

function deferred<T>() { let resolve!: (value: T) => void; let reject!: (reason: Error) => void; const promise = new Promise<T>((a,b) => { resolve = a; reject = b; }); return { promise, resolve, reject }; }
function fixture() {
  let account = 'alice';
  const docs: Record<string, Record<string, unknown>> = { alice: {}, bob: {} };
  const remote: PreferenceTransport = {
    read: vi.fn(async () => ({ subject: account, values: { ...docs[account] } })),
    write: vi.fn(async (expected, values) => {
      if (expected !== account) throw new APIError('Account changed', 409);
      docs[account] = { ...docs[account], ...values };
      return { subject: account, values: { ...docs[account] } };
    }),
  };
  return { remote, docs, login: (who: string) => { account = who; } };
}
beforeEach(() => sessionStorage.clear());
afterEach(() => vi.restoreAllMocks());

it('persists views, filters and selections through refresh and logout/login without sharing accounts', async () => {
  const f = fixture();
  let store = new DisplayPreferences(f.remote, sessionStorage);
  await store.flush();
  store.set('deploy.apps.view', 'list'); store.set('deploy.apps.query', 'portal'); store.set('deploy.apps.status', 'stopped'); store.set('deploy.databases.app', 'portal');
  await store.flush();
  store = new DisplayPreferences(f.remote, sessionStorage); await store.flush();
  expect(store.getSnapshot().values).toMatchObject({ 'deploy.apps.view': 'list', 'deploy.apps.query': 'portal', 'deploy.apps.status': 'stopped', 'deploy.databases.app': 'portal' });
  f.login('bob'); store = new DisplayPreferences(f.remote, sessionStorage); await store.flush();
  expect(store.getSnapshot().values).toEqual({});
  store.set('deploy.apps.view', 'grid'); await store.flush();
  f.login('alice'); store = new DisplayPreferences(f.remote, sessionStorage); await store.flush();
  expect(store.getSnapshot().values['deploy.apps.view']).toBe('list');
});

it('merges edits made during a slow initial read without overwriting unrelated server choices', async () => {
  const f = fixture(), read = deferred<{subject:string;values:Record<string,unknown>}>();
  f.docs.alice['deploy.logs.follow'] = false;
  f.remote.read = () => read.promise;
  const store = new DisplayPreferences(f.remote, sessionStorage);
  const loading = store.flush();
  store.set('deploy.apps.view', 'list');
  read.resolve({ subject: 'alice', values: { 'deploy.apps.view': 'grid', 'deploy.logs.follow': false } });
  await loading;
  expect(store.getSnapshot().values['deploy.apps.view']).toBe('list');
  expect(f.remote.write).toHaveBeenCalledWith('alice', { 'deploy.apps.view': 'list' });
  expect(store.getSnapshot().values['deploy.logs.follow']).toBe(false);
  expect(store.getSnapshot().ready).toBe(true);
});

it('serializes writes and retains edits made while an older batch is saving', async () => {
  const f = fixture(), old = deferred<{subject:string;values:Record<string,unknown>}>();
  const write = f.remote.write;
  f.remote.write = vi.fn().mockImplementationOnce(() => old.promise).mockImplementation(write);
  const store = new DisplayPreferences(f.remote, sessionStorage); await store.flush();
  store.set('deploy.apps.view', 'list'); const saving = store.flush();
  await Promise.resolve(); await Promise.resolve();
  store.set('deploy.apps.view', 'grid'); store.set('deploy.apps.query', 'new');
  expect(store.flush()).toBe(saving);
  old.resolve({ subject: 'alice', values: { 'deploy.apps.view': 'list' } }); await saving;
  expect(f.remote.write).toHaveBeenCalledTimes(2);
  expect(store.getSnapshot().values['deploy.apps.view']).toBe('grid');
  expect(f.docs.alice).toMatchObject({ 'deploy.apps.view': 'grid', 'deploy.apps.query': 'new' });
  expect(store.getSnapshot().pending).toBe(false);
});

it('recovers an unsaved edit after failed writes and a fast refresh, scoped to the server identity', async () => {
  const f = fixture(), write = f.remote.write;
  const store = new DisplayPreferences(f.remote, sessionStorage); await store.flush();
  f.remote.write = vi.fn(async () => { throw new Error('offline'); });
  store.set('deploy.apps.view', 'list'); await expect(store.flush()).rejects.toThrow();
  expect(store.getSnapshot().error).toContain('retry'); expect(store.getSnapshot().values['deploy.apps.view']).toBe('list');
  f.remote.write = write;
  f.login('bob'); const bob = new DisplayPreferences(f.remote, sessionStorage); await bob.flush();
  expect(bob.getSnapshot().values).toEqual({}); expect(f.docs.bob).toEqual({});
  f.login('alice'); const refreshed = new DisplayPreferences(f.remote, sessionStorage); await refreshed.flush();
  expect(refreshed.getSnapshot().values['deploy.apps.view']).toBe('list'); expect(refreshed.getSnapshot().error).toBe('');
  expect(sessionStorage.length).toBe(0);
});

it('keeps an edit temporary until identity loads after a read failure', async () => {
  const f = fixture(), read = f.remote.read;
  f.remote.read = vi.fn().mockRejectedValueOnce(new Error('offline')).mockImplementation(read);
  const store = new DisplayPreferences(f.remote, sessionStorage); await expect(store.flush()).rejects.toThrow();
  store.set('deploy.apps.view', 'list'); expect(sessionStorage.length).toBe(0);
  await store.flush(); expect(f.docs.alice['deploy.apps.view']).toBe('list'); expect(store.getSnapshot().error).toBe('');
});

it('sends the original account expectation when a session changes during a delayed write', async () => {
  const f = fixture(), store = new DisplayPreferences(f.remote, sessionStorage); await store.flush();
  store.set('deploy.apps.view', 'list'); f.login('bob');
  await expect(store.flush()).rejects.toThrow('Account changed');
  expect(f.docs.bob).toEqual({}); expect(store.getSnapshot().values).toEqual({});
  await store.flush(); expect(store.getSnapshot().values).toEqual({}); expect(f.docs.bob).toEqual({});
  f.login('alice'); const alice=new DisplayPreferences(f.remote,sessionStorage);await alice.flush();
  expect(alice.getSnapshot().values['deploy.apps.view']).toBe('list');
});

it('validates persisted display values and excludes credentials/action state from its retry buffer', async () => {
  const f = fixture(); f.docs.alice = { 'deploy.apps.view': 'broken', 'deploy.apps.status': 'unknown', secret: 'not a display preference', 'deploy.logs.follow': false };
  sessionStorage.setItem('lidza.display.pending.v1.alice', JSON.stringify({ password: 'ignored', 'deploy.apps.view': 'list' }));
  const store = new DisplayPreferences(f.remote, sessionStorage); await store.flush();
  expect(store.getSnapshot().values).toEqual({ 'deploy.logs.follow': false, 'deploy.apps.view': 'list' });
  expect(f.remote.write).toHaveBeenCalledWith('alice', { 'deploy.apps.view': 'list' });
});

it('does not race a pagehide keepalive against an existing write', async () => {
  const f = fixture(), delayed = deferred<{subject:string;values:Record<string,unknown>}>();
  const store = new DisplayPreferences(f.remote, sessionStorage); await store.flush();
  f.remote.write = vi.fn(() => delayed.promise);
  store.set('deploy.apps.view', 'list'); const saving = store.flush(); await Promise.resolve(); await Promise.resolve();
  store.set('deploy.apps.view', 'grid'); store.pagehide(); expect(f.remote.write).toHaveBeenCalledTimes(1);
  f.remote.write = async (subject,values) => ({ subject, values });
  delayed.resolve({subject:'alice',values:{'deploy.apps.view':'list'}}); await saving;
  expect(store.getSnapshot().values['deploy.apps.view']).toBe('grid');
});
