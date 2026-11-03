import { createContext, type ReactNode, useContext, useEffect, useState, useSyncExternalStore } from 'react';
import { DisplayPreferences, emptyPreferences, type PreferenceKey } from './DisplayPreferences';

const Context = createContext<DisplayPreferences | null>(null);
const noop = () => () => {};
const empty = () => emptyPreferences;
export function PreferencesProvider({ children, store: supplied }: { children: ReactNode; store?: DisplayPreferences }) {
  const [store] = useState(() => {
    if (supplied) return supplied;
    let buffer: Storage | undefined;
    try { buffer = window.sessionStorage; } catch { /* Optional retry buffer. */ }
    return new DisplayPreferences(undefined, buffer, new URLSearchParams(location.search).get('demo') === '1');
  });
  useEffect(() => {
    store.start();
    const retry = () => { void store.flush().catch(() => {}); };
    window.addEventListener('online', retry);
    window.addEventListener('pagehide', store.pagehide);
    return () => { store.stop(); window.removeEventListener('online', retry); window.removeEventListener('pagehide', store.pagehide); };
  }, [store]);
  return <Context.Provider value={store}>{children}</Context.Provider>;
}
export function usePreferences() {
  const store = useContext(Context);
  const snapshot = useSyncExternalStore(store?.subscribe ?? noop, store?.getSnapshot ?? empty);
  return { ...snapshot, flush: store?.flush ?? (() => Promise.resolve()) };
}
export function usePreference<T extends string | boolean>(key: PreferenceKey, fallback: T): [T, (value: T) => void] {
  const store = useContext(Context);
  const snapshot = useSyncExternalStore(store?.subscribe ?? noop, store?.getSnapshot ?? empty);
  // Components embedded outside the console keep ordinary local state.
  const [local, setLocal] = useState(fallback);
  return store ? [(snapshot.values[key] ?? fallback) as T, value => store.set(key, value)] : [local, setLocal];
}
