import { useMemo, useSyncExternalStore } from 'react';
import type { PageKey, Route } from './types';

// The console lives under one base path. Every view has a readable,
// reloadable URL; the server returns console.html for any path below it.
export const BASE = '/console';
export const HOME = `${BASE}/applications`;

const pages = new Set<PageKey>(['applications', 'deployments', 'errors', 'security', 'servers', 'databases', 'remote-backups', 'integrations', 'defaults', 'updates', 'team', 'audit', 'help', 'new-application']);
const serverDetails = new Set(['cleanup', 'containers', 'console', 'ssh', 'upgrade']);
// Preview parameters follow the visitor through the panel.
const sticky = ['demo', 'design'];

const enc = encodeURIComponent;
function dec(value: string) { try { return decodeURIComponent(value); } catch { return value; } }

export function isPanelPath(pathname: string) {
  return pathname === BASE || pathname.startsWith(`${BASE}/`) || pathname === '/console.html';
}

/** parsePath maps a console URL to the view it shows. */
export function parsePath(pathname: string, search = ''): Route {
  const params = new URLSearchParams(search);
  const rest = pathname.startsWith(`${BASE}/`) ? pathname.slice(BASE.length + 1) : '';
  const parts = rest.split('/').filter(Boolean).map(dec);
  const [section, id, detail, tab] = parts;
  if (!section) return { page: 'applications' };
  if (section === 'applications' && id) {
    if (!detail || detail === 'settings') return parts.length <= 4 ? { page: 'detail', detail: 'settings', id, ...(tab ? { tab } : {}) } : notFound;
    if (parts.length > 3) return notFound;
    if (detail === 'logs') return { page: 'detail', detail: 'runtime-logs', id };
    if (detail === 'cache') return { page: 'detail', detail: 'cache', id };
    return notFound;
  }
  if (section === 'deployments' && id) return parts.length === 2 ? { page: 'detail', detail: 'build-logs', id } : notFound;
  if (section === 'databases' && id) return parts.length === 2 ? { page: 'detail', detail: 'database', id } : notFound;
  if (section === 'servers' && id) {
    if (!detail) return { page: 'detail', detail: 'server', id };
    return parts.length === 3 && serverDetails.has(detail) ? { page: 'detail', detail, id } : notFound;
  }
  if (section === 'account' && id === 'two-factor' && parts.length === 2) return { page: 'detail', detail: 'two-factor', id: 'account' };
  if (section === 'new-application' && parts.length === 1) return { page: 'new-application', id: params.get('server') || undefined };
  if ((section === 'errors' || section === 'security') && parts.length <= 2) return { page: section, id };
  if (pages.has(section as PageKey) && parts.length === 1) return { page: section as PageKey };
  return notFound;
}
const notFound: Route = { page: 'not-found' };

/** buildPath is the inverse of parsePath (without preview parameters). */
export function buildPath(route: Route): string {
  const id = route.id ? enc(route.id) : '';
  if (route.page === 'detail') {
    switch (route.detail) {
      case 'settings': return `${BASE}/applications/${id}/settings${route.tab ? `/${enc(route.tab)}` : ''}`;
      case 'runtime-logs': return `${BASE}/applications/${id}/logs`;
      case 'cache': return `${BASE}/applications/${id}/cache`;
      case 'build-logs': return `${BASE}/deployments/${id}`;
      case 'database': return `${BASE}/databases/${id}`;
      case 'server': return `${BASE}/servers/${id}`;
      case 'two-factor': return `${BASE}/account/two-factor`;
      default: return route.detail && serverDetails.has(route.detail) ? `${BASE}/servers/${id}/${route.detail}` : HOME;
    }
  }
  if (route.page === 'new-application') return `${BASE}/new-application${route.id ? `?server=${enc(route.id)}` : ''}`;
  if (route.page === 'not-found') return HOME;
  return `${BASE}/${route.page}${id ? `/${id}` : ''}`;
}

/** href links a workspace section, optionally with one selected item. */
export function href(page: PageKey, id?: string) {
  return buildPath({ page, id });
}

/** detailHref links a full-page resource view. */
export function detailHref(detail: string, id: string, tab?: string) {
  return buildPath({ page: 'detail', detail, id, tab });
}

/** legacyHashPath maps an old `console.html#…` route to its path, or null. */
export function legacyHashPath(hash: string): string | null {
  const clean = hash.replace(/^#\/?/, '').replace(/\/+$/, '');
  if (!clean) return null;
  const parts = clean.split('/');
  if (parts[0] === 'page') {
    const detail = parts[1], id = dec(parts.slice(2).join('/'));
    if (!detail || !id || !['settings', 'runtime-logs', 'cache', 'build-logs', 'database', 'server', 'two-factor', ...serverDetails].includes(detail)) return null;
    return detailHref(detail, id);
  }
  const page = parts[0] as PageKey;
  if (!pages.has(page)) return null;
  const id = parts.length > 1 ? dec(parts.slice(1).join('/')) : undefined;
  // Help and the other sections never had selectable items; keep only the section.
  return buildPath({ page, id: page === 'new-application' || page === 'errors' || page === 'security' ? id : undefined });
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
  const url = withSticky(to);
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

/** normalizeLocation redirects legacy hash routes and bare entry URLs in place. */
export function normalizeLocation() {
  const legacy = legacyHashPath(location.hash);
  const entry = location.pathname === '/console.html' || location.pathname === BASE || location.pathname === `${BASE}/` || location.pathname === '/';
  let target: string | undefined;
  if (legacy && (entry || isPanelPath(location.pathname))) target = legacy;
  else if (entry) target = HOME;
  if (target) {
    const [path, query = ''] = target.split('?');
    const merged = new URLSearchParams(location.search);
    for (const [key, value] of new URLSearchParams(query)) merged.set(key, value);
    const search = merged.toString();
    history.replaceState({ lidza: 1, idx: historyIndex() }, '', `${path}${search ? `?${search}` : ''}`);
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

// ---- Titles -----------------------------------------------------------------

const sectionTitles: Partial<Record<PageKey, string>> = { applications: 'Applications', deployments: 'Deployments', errors: 'Errors', security: 'Security activity', servers: 'Servers', databases: 'Databases & backups', 'remote-backups': 'Remote backups', integrations: 'Integrations', defaults: 'Application defaults', updates: 'Updates', team: 'Team', audit: 'Audit log', help: 'Help', 'new-application': 'New application', 'not-found': 'Not found' };
const detailTitles: Record<string, (id: string) => string> = {
  settings: id => `${id} settings`, 'runtime-logs': id => `${id} runtime logs`, cache: id => `${id} cache`, 'build-logs': id => `Build ${id.slice(0, 12)}`,
  database: id => `${id} database`, server: id => id === 'new' ? 'Connect server' : `${id} server`, cleanup: id => `${id} cleanup`, containers: id => `${id} processes`,
  console: id => `${id} console`, ssh: id => `${id} SSH access`, upgrade: id => `${id} upgrade`, 'two-factor': () => 'Two-factor authentication',
};

/** routeLabel names the view for the breadcrumb. */
export function routeLabel(route: Route) {
  if (route.page === 'detail') return route.detail && route.id && detailTitles[route.detail] ? detailTitles[route.detail](route.id) : route.id || 'Details';
  return sectionTitles[route.page] || 'Workspace';
}

export function routeTitle(route: Route) {
  return `${routeLabel(route)} · Līdza Deploy`;
}

/** parentPath is where the on-page Back goes without in-panel history. */
export function parentPath(route: Route): string {
  if (route.page !== 'detail') return HOME;
  switch (route.detail) {
    case 'build-logs': return href('deployments');
    case 'database': return href('databases');
    case 'cache': return detailHref('settings', route.id || '');
    case 'server': case 'cleanup': case 'containers': case 'console': case 'ssh': case 'upgrade': return href('servers');
    default: return HOME;
  }
}
