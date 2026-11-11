import { useMemo, useSyncExternalStore } from 'react';

// The console lives under one base path. Every view has a readable,
// reloadable URL; the server returns console.html for any path below it.
// The route tree (resources, tabs, redirects, breadcrumbs) is lib/resources.ts.
import { BASE, HOME, type Route, buildPath, parsePath, redirectPath } from './resources';
export { BASE, HOME, buildPath, createHref, indexHref, parsePath, redirectPath, resourceHref, routeTitle, singletonHref } from './resources';

// Preview parameters follow the visitor through the panel.
const sticky = ['demo', 'design'];

const enc = encodeURIComponent;
function dec(value: string) { try { return decodeURIComponent(value); } catch { return value; } }

export function isPanelPath(pathname: string) {
  return pathname === BASE || pathname.startsWith(`${BASE}/`) || pathname === '/console.html';
}

// Hash routes of the first console (`console.html#page/settings/x`) and the
// path each one shows now.
const legacyDetails: Record<string, (id: string) => string> = {
  settings: id => `${BASE}/applications/${enc(id)}/settings`, 'runtime-logs': id => `${BASE}/applications/${enc(id)}/logs`,
  cache: id => `${BASE}/applications/${enc(id)}/data`, 'build-logs': id => `${BASE}/deployments/${enc(id)}`,
  database: id => `${BASE}/databases/${enc(id)}`, server: id => `${BASE}/servers/${enc(id)}`,
  console: id => `${BASE}/servers/${enc(id)}/console`, containers: id => `${BASE}/servers/${enc(id)}/processes`,
  cleanup: id => `${BASE}/servers/${enc(id)}/cleanup`, ssh: id => `${BASE}/servers/${enc(id)}/ssh`,
  upgrade: id => `${BASE}/servers/${enc(id)}/agent`, 'two-factor': () => `${BASE}/account`,
};
const legacySections = new Set(['applications', 'deployments', 'errors', 'security', 'servers', 'databases', 'remote-backups', 'integrations', 'defaults', 'updates', 'team', 'audit', 'help', 'control-panel', 'new-application', 'deleted-apps']);

/** legacyHashPath maps an old `console.html#…` route to its path, or null. */
export function legacyHashPath(hash: string): string | null {
  const clean = hash.replace(/^#\/?/, '').replace(/\/+$/, '');
  if (!clean) return null;
  const parts = clean.split('/');
  if (parts[0] === 'page') {
    const detail = parts[1], id = dec(parts.slice(2).join('/'));
    if (!detail || !id || !legacyDetails[detail]) return null;
    return legacyDetails[detail](id);
  }
  const page = parts[0];
  if (!legacySections.has(page)) return null;
  const id = parts.length > 1 ? dec(parts.slice(1).join('/')) : undefined;
  if (page === 'new-application') return `${BASE}/applications/new${id ? `?server=${enc(id)}` : ''}`;
  if ((page === 'errors' || page === 'security') && id) return `${BASE}/${page}?app=${enc(id)}`;
  // Help and the other sections never had selectable items; keep only the section.
  return `${BASE}/${page}`;
}

/** withSticky carries preview parameters (demo, design) onto a panel path. */
export function withSticky(to: string, from = location.search) {
  const url = new URL(to, location.origin);
  const current = new URLSearchParams(from);
  for (const key of sticky) if (current.has(key) && !url.searchParams.has(key)) url.searchParams.set(key, current.get(key)!);
  return url.pathname + url.search + url.hash;
}

// ---- History ----------------------------------------------------------------

interface PanelState { lidza: 1; idx: number }
const listeners = new Set<() => void>();
// URLs of this tab's panel entries by index, as far as this page load saw them.
const entries: string[] = [];
let lastIndex = 0;
function emit() { lastIndex = historyIndex(); for (const listener of [...listeners]) listener(); }
function panelState(state: unknown): state is PanelState { return !!state && typeof state === 'object' && (state as PanelState).lidza === 1 && typeof (state as PanelState).idx === 'number'; }
function here() { return location.pathname + location.search + location.hash; }
function record(idx: number, url: string, push: boolean) { entries[idx] = url; if (push) entries.length = idx + 1; lastIndex = idx; }
/** historyIndex is how many panel entries precede the current one in this tab. */
export function historyIndex() { return panelState(history.state) ? history.state.idx : 0; }
/** canGoBack is true when the previous history entry is a panel view. */
export function canGoBack() { return historyIndex() > 0; }
/** previousEntry is the URL the browser Back button opens, when known. */
export function previousEntry() { const idx = historyIndex(); return idx > 0 ? entries[idx - 1] : undefined; }

export function navigate(to: string, { replace = false }: { replace?: boolean } = {}) {
  // An earlier URL followed inside the panel opens its current view.
  const [path, query = ''] = to.split('?');
  const current = redirectPath(path, query ? `?${query}` : '');
  const url = withSticky(current || to);
  // Following a link to the view already shown adds no history entry.
  if (url === here()) replace = true;
  const idx = replace ? historyIndex() : historyIndex() + 1;
  if (replace) history.replaceState({ lidza: 1, idx }, '', url);
  else history.pushState({ lidza: 1, idx }, '', url);
  record(idx, url, !replace);
  emit();
}

/** goBack matches the browser Back button inside the panel, else opens the parent. */
export function goBack(parent: string) {
  if (canGoBack()) history.back();
  else navigate(parent);
}

/** searchHref is the current view with some query parameters changed. */
export function searchHref(values: Record<string, string | undefined>) {
  const params = new URLSearchParams(location.search);
  for (const [key, value] of Object.entries(values)) { if (value === undefined || value === '') params.delete(key); else params.set(key, value); }
  const query = params.toString();
  return withSticky(`${location.pathname}${query ? `?${query}` : ''}`);
}

/** setSearchParams updates query parameters of the current view (replacing the entry by default). */
export function setSearchParams(values: Record<string, string | undefined>, { replace = true }: { replace?: boolean } = {}) {
  navigate(searchHref(values), { replace });
}

/** normalizeLocation redirects legacy hash routes, bare entry URLs and earlier paths in place. */
export function normalizeLocation() {
  const legacy = legacyHashPath(location.hash);
  const entry = location.pathname === '/console.html' || location.pathname === BASE || location.pathname === `${BASE}/` || location.pathname === '/';
  let target: string | undefined;
  if (legacy && (entry || isPanelPath(location.pathname))) target = legacy;
  else if (entry) target = HOME;
  else if (isPanelPath(location.pathname)) target = redirectPath(location.pathname, location.search) || undefined;
  if (target) {
    const [path, query = ''] = target.split('?');
    const merged = new URLSearchParams(target === legacy || target === HOME ? location.search : '');
    for (const [key, value] of new URLSearchParams(query)) merged.set(key, value);
    const search = merged.toString();
    const redirected = redirectPath(path, search ? `?${search}` : '');
    history.replaceState({ lidza: 1, idx: historyIndex() }, '', redirected || `${path}${search ? `?${search}` : ''}`);
  } else if (!panelState(history.state)) history.replaceState({ lidza: 1, idx: 0 }, '', here());
  record(historyIndex(), here(), false);
}

function interceptClick(event: MouseEvent) {
  if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
  const anchor = (event.target as Element | null)?.closest?.('a[href]') as HTMLAnchorElement | null;
  if (!anchor || (anchor.target && anchor.target !== '_self') || anchor.hasAttribute('download')) return;
  const url = new URL(anchor.href, location.href);
  if (url.origin !== location.origin || !url.pathname.startsWith(`${BASE}/`)) return;
  event.preventDefault();
  navigate(url.pathname + url.search + url.hash);
}

// An old `#route` link followed inside the panel becomes a new entry with
// the matching path.
function onHashChange() {
  const legacy = legacyHashPath(location.hash);
  if (!panelState(history.state)) history.replaceState({ lidza: 1, idx: lastIndex + 1 }, '', here());
  if (legacy) navigate(legacy, { replace: true });
  else { record(historyIndex(), here(), true); emit(); }
}

/** startRouter normalizes the entry URL and intercepts in-panel link clicks.
 * Views subscribe to Back/Forward (popstate) through useLocation. */
export function startRouter() {
  normalizeLocation();
  window.addEventListener('hashchange', onHashChange);
  document.addEventListener('click', interceptClick);
  return () => {
    window.removeEventListener('hashchange', onHashChange);
    document.removeEventListener('click', interceptClick);
  };
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  window.addEventListener('popstate', listener);
  return () => { listeners.delete(listener); window.removeEventListener('popstate', listener); };
}
window.addEventListener('popstate', () => { lastIndex = historyIndex(); });
const snapshot = () => location.pathname + location.search;

/** useLocation re-renders on every panel navigation, Back and Forward. */
export function useLocation() {
  const current = useSyncExternalStore(subscribe, snapshot);
  return useMemo(() => { const at = current.indexOf('?'); return { pathname: at < 0 ? current : current.slice(0, at), search: at < 0 ? '' : current.slice(at) }; }, [current]);
}

export function useRoute(): Route {
  const { pathname, search } = useLocation();
  return useMemo(() => parsePath(pathname, search), [pathname, search]);
}

export function useSearchParam(name: string) {
  const { search } = useLocation();
  return new URLSearchParams(search).get(name) ?? undefined;
}

