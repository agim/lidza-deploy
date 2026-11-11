// The console's route tree: every resource, its tabs, the URL of each view,
// the redirects from earlier URLs and the breadcrumb of every view. Pages,
// navigation and tests all read this one registry (docs/console-structure.md).

export const BASE = '/console';
export const HOME = `${BASE}/applications`;

export type CollectionKey =
  | 'applications' | 'servers' | 'databases' | 'deployments' | 'deleted-apps' | 'team' | 'errors' | 'security' | 'audit'
  | 'integrations' | 'defaults' | 'remote-backups' | 'updates' | 'control-panel' | 'help' | 'account';

export interface TabDef { id: string; label: string }

export interface ResourceDef {
  key: CollectionKey;
  /** The index (or singleton) title, also its breadcrumb. */
  label: string;
  /** collection: index + detail pages; singleton: one resource with tabs. */
  kind: 'collection' | 'singleton';
  /** Detail tabs; the first is the bare path. An index-only collection has none. */
  tabs: TabDef[];
  /** Label of the create form at /<collection>/new. */
  create?: string;
  /** A detail id comes with this query parameter (an error group belongs to an application). */
  parentParam?: string;
}

const overview = { id: 'overview', label: 'Overview' };

export const resources: Record<CollectionKey, ResourceDef> = {
  applications: { key: 'applications', label: 'Applications', kind: 'collection', create: 'New application', tabs: [
    overview, { id: 'deployments', label: 'Deployments' }, { id: 'logs', label: 'Logs' }, { id: 'environment', label: 'Environment' },
    { id: 'domains', label: 'Domains' }, { id: 'data', label: 'Database & cache' }, { id: 'backups', label: 'Backups' },
    { id: 'processes', label: 'Processes' }, { id: 'move', label: 'Move' }, { id: 'settings', label: 'Settings' },
  ] },
  servers: { key: 'servers', label: 'Servers', kind: 'collection', create: 'Connect server', tabs: [
    overview, { id: 'applications', label: 'Applications' }, { id: 'processes', label: 'Processes' }, { id: 'ingress', label: 'Web ingress' },
    { id: 'updates', label: 'Updates' }, { id: 'tuning', label: 'Tuning' }, { id: 'firewall', label: 'Firewall' },
    { id: 'cleanup', label: 'Disk cleanup' }, { id: 'retained', label: 'Retained data' }, { id: 'console', label: 'Console' },
    { id: 'ssh', label: 'SSH access' }, { id: 'agent', label: 'Agent' },
  ] },
  databases: { key: 'databases', label: 'Databases', kind: 'collection', tabs: [
    overview, { id: 'backups', label: 'Backups' }, { id: 'schedule', label: 'Schedule' }, { id: 'excluded', label: 'Excluded tables' }, { id: 'settings', label: 'Settings' },
  ] },
  deployments: { key: 'deployments', label: 'Deployments', kind: 'collection', tabs: [overview, { id: 'log', label: 'Log' }] },
  'deleted-apps': { key: 'deleted-apps', label: 'Deleted apps', kind: 'collection', tabs: [overview, { id: 'settings', label: 'Settings' }] },
  team: { key: 'team', label: 'Team', kind: 'collection', create: 'Add member', tabs: [overview, { id: 'settings', label: 'Settings' }] },
  errors: { key: 'errors', label: 'Errors', kind: 'collection', parentParam: 'app', tabs: [overview, { id: 'occurrences', label: 'Occurrences' }] },
  security: { key: 'security', label: 'Security activity', kind: 'collection', parentParam: 'app', tabs: [overview, { id: 'occurrences', label: 'Occurrences' }] },
  audit: { key: 'audit', label: 'Audit log', kind: 'collection', tabs: [] },
  integrations: { key: 'integrations', label: 'Integrations', kind: 'singleton', tabs: [{ id: 'github', label: 'GitHub' }, { id: 'storage', label: 'Backup storage' }, { id: 'email', label: 'Email' }] },
  defaults: { key: 'defaults', label: 'Application defaults', kind: 'singleton', tabs: [{ id: 'go', label: 'Līdza (Go)' }, { id: 'rails', label: 'Ruby on Rails' }, { id: 'astro', label: 'Astro' }, { id: 'node', label: 'Node.js' }] },
  'remote-backups': { key: 'remote-backups', label: 'Remote backups', kind: 'singleton', tabs: [{ id: 'objects', label: 'Objects' }, { id: 'retention', label: 'Retention' }] },
  updates: { key: 'updates', label: 'Updates', kind: 'singleton', tabs: [overview, { id: 'channel', label: 'Signed channel' }] },
  'control-panel': { key: 'control-panel', label: 'Control panel', kind: 'singleton', tabs: [overview, { id: 'move', label: 'Move' }] },
  help: { key: 'help', label: 'Help', kind: 'singleton', tabs: [{ id: 'guides', label: 'Guides' }, { id: 'components', label: 'Components' }] },
  account: { key: 'account', label: 'Account', kind: 'singleton', tabs: [{ id: 'two-factor', label: 'Two-factor authentication' }] },
};

export type View = 'index' | 'detail' | 'create' | 'singleton' | 'not-found';

export interface Route {
  view: View;
  collection?: CollectionKey;
  /** The resource of a detail view. */
  id?: string;
  /** The open tab of a detail or singleton view; the first tab when the path names none. */
  tab?: string;
  /** Query parameters that are part of the view: the server of a new application, the application of an error group or error index. */
  query?: Record<string, string>;
}

export const notFound: Route = { view: 'not-found' };

const enc = encodeURIComponent;
function dec(value: string) { try { return decodeURIComponent(value); } catch { return value; } }
function isKey(value: string): value is CollectionKey { return Object.hasOwn(resources, value); }
/** tabOf is the tab definition named by id, or undefined. */
export function tabOf(collection: CollectionKey, id?: string) { return resources[collection].tabs.find(tab => tab.id === id); }
export function firstTab(collection: CollectionKey) { return resources[collection].tabs[0]?.id; }

// ---- Redirects ----------------------------------------------------------------

const appSettingsTabs: Record<string, string> = { general: 'settings', environment: 'environment', resources: 'data', processes: 'processes' };
const renamed: Partial<Record<CollectionKey, Record<string, string>>> = {
  applications: { cache: 'data' },
  servers: { containers: 'processes', upgrade: 'agent' },
};

/**
 * redirectPath is the current URL of an earlier console URL, or null when the
 * path is current. It keeps unrelated query parameters (preview flags).
 */
export function redirectPath(pathname: string, search = ''): string | null {
  const params = new URLSearchParams(search);
  const rest = pathname.startsWith(`${BASE}/`) ? pathname.slice(BASE.length + 1) : '';
  const raw = rest.split('/').filter(Boolean);
  const parts = raw.map(dec);
  const [section, id, tab, extra] = parts;
  const out = (path: string, drop: string[] = [], add: Record<string, string> = {}) => {
    const next = new URLSearchParams(search);
    for (const key of drop) next.delete(key);
    for (const [key, value] of Object.entries(add)) next.set(key, value);
    const query = next.toString();
    return `${BASE}/${path}${query ? `?${query}` : ''}`;
  };
  if (pathname === '/console.html' || pathname === BASE || pathname === `${BASE}/` || pathname === '/') return out('applications');
  if (section === 'new-application' && parts.length === 1) return out('applications/new');
  if (section === 'applications' && !id && params.get('move')) return out(`applications/${enc(params.get('move')!)}/move`, ['move']);
  if (section === 'applications' && id && tab === 'settings' && extra && parts.length === 4 && appSettingsTabs[extra]) return out(`applications/${raw[1]}/${appSettingsTabs[extra]}`);
  if ((section === 'errors' || section === 'security') && id && parts.length === 2 && !params.get('app')) return out(section, [], { app: id });
  if (section === 'account' && id === 'two-factor' && parts.length === 2) return out('account');
  if (section && isKey(section) && id && tab && parts.length === 3) {
    const def = resources[section];
    const target = renamed[section]?.[tab];
    if (target) return out(`${section}/${raw[1]}/${target}`);
    if (def.kind === 'collection' && tab === def.tabs[0]?.id) return out(`${section}/${raw[1]}`);
  }
  if (section && isKey(section) && resources[section].kind === 'singleton' && id && id === resources[section].tabs[0]?.id && parts.length === 2) return out(section);
  return null;
}

// ---- Parsing and building ------------------------------------------------------

/** parsePath maps a console URL (after its redirect) to the view it shows. */
export function parsePath(pathname: string, search = ''): Route {
  const redirect = redirectPath(pathname, search);
  if (redirect) { const at = redirect.indexOf('?'); pathname = at < 0 ? redirect : redirect.slice(0, at); search = at < 0 ? '' : redirect.slice(at); }
  const params = new URLSearchParams(search);
  const rest = pathname.startsWith(`${BASE}/`) ? pathname.slice(BASE.length + 1) : '';
  const parts = rest.split('/').filter(Boolean).map(dec);
  const [section, id, tab] = parts;
  if (!section || !isKey(section)) return notFound;
  const def = resources[section];
  if (def.kind === 'singleton') {
    if (parts.length > 2) return notFound;
    const chosen = id ?? def.tabs[0].id;
    return tabOf(section, chosen) ? { view: 'singleton', collection: section, tab: chosen } : notFound;
  }
  if (!id) {
    if (def.parentParam && params.get(def.parentParam)) return { view: 'index', collection: section, query: { [def.parentParam]: params.get(def.parentParam)! } };
    return { view: 'index', collection: section };
  }
  if (id === 'new' && def.create) {
    if (parts.length !== 2) return notFound;
    return params.get('server') && section === 'applications' ? { view: 'create', collection: section, query: { server: params.get('server')! } } : { view: 'create', collection: section };
  }
  if (!def.tabs.length || parts.length > 3) return notFound;
  const chosen = tab ?? def.tabs[0].id;
  if (!tabOf(section, chosen)) return notFound;
  const route: Route = { view: 'detail', collection: section, id, tab: chosen };
  if (def.parentParam) { const parent = params.get(def.parentParam); if (!parent) return notFound; route.query = { [def.parentParam]: parent }; }
  return route;
}

/** buildPath is the inverse of parsePath (without preview parameters). */
export function buildPath(route: Route): string {
  const c = route.collection;
  if (route.view === 'not-found' || !c) return HOME;
  const query = route.query && Object.keys(route.query).length ? `?${new URLSearchParams(route.query)}` : '';
  if (route.view === 'singleton') return `${BASE}/${c}${route.tab && route.tab !== firstTab(c) && tabOf(c, route.tab) ? `/${enc(route.tab)}` : ''}`;
  if (route.view === 'index') return `${BASE}/${c}${query}`;
  if (route.view === 'create') return `${BASE}/${c}/new${query}`;
  const tab = route.tab && route.tab !== firstTab(c) && tabOf(c, route.tab) ? `/${enc(route.tab)}` : '';
  return `${BASE}/${c}/${enc(route.id || '')}${tab}${query}`;
}

/** indexHref links a collection's index (or a singleton's first tab). */
export function indexHref(collection: CollectionKey, query?: Record<string, string>) {
  return buildPath({ view: resources[collection].kind === 'singleton' ? 'singleton' : 'index', collection, query });
}

/** resourceHref links one resource, optionally one of its tabs. */
export function resourceHref(collection: CollectionKey, id: string, tab?: string, query?: Record<string, string>) {
  return buildPath({ view: 'detail', collection, id, tab, query });
}

/** tabHref links a tab of a singleton. */
export function singletonHref(collection: CollectionKey, tab?: string) {
  return buildPath({ view: 'singleton', collection, tab });
}

/** createHref links a collection's create form. */
export function createHref(collection: CollectionKey, query?: Record<string, string>) {
  return buildPath({ view: 'create', collection, query });
}

// ---- Breadcrumbs and titles ----------------------------------------------------

export interface Crumb { label: string; href?: string }

/**
 * breadcrumbs lists the path to a view, from Home (the applications) down. Every
 * segment but the last links; `name` is the resource's display name
 * (its id until known).
 */
export function breadcrumbs(route: Route, name?: string): Crumb[] {
  const trail: Crumb[] = [{ label: 'Home', href: HOME }];
  const c = route.collection;
  if (route.view === 'not-found' || !c) { trail.push({ label: 'Not found' }); return trail; }
  const def = resources[c];
  const parent = def.parentParam && route.query?.[def.parentParam] ? { [def.parentParam]: route.query[def.parentParam] } : undefined;
  if (route.view === 'index') trail.push({ label: def.label });
  else if (route.view === 'create') trail.push({ label: def.label, href: indexHref(c) }, { label: def.create || 'New' });
  else if (route.view === 'singleton') {
    if (route.tab && route.tab !== def.tabs[0].id) trail.push({ label: def.label, href: indexHref(c) }, { label: tabOf(c, route.tab)?.label || route.tab });
    else trail.push({ label: def.label });
  } else {
    trail.push({ label: def.label, href: indexHref(c, parent) });
    const resource = name || route.id || '';
    if (route.tab && route.tab !== def.tabs[0].id) trail.push({ label: resource, href: resourceHref(c, route.id || '', undefined, route.query) }, { label: tabOf(c, route.tab)?.label || route.tab });
    else trail.push({ label: resource });
  }
  for (let i = 0; i < trail.length - 1; i++) if (!trail[i].href) trail[i].href = HOME;
  delete trail[trail.length - 1].href;
  return trail;
}

/** routeTitle names a view in the browser tab: the breadcrumb, most specific first. */
export function routeTitle(route: Route, name?: string) {
  const trail = breadcrumbs(route, name).slice(1).map(crumb => crumb.label).reverse();
  return `${trail.join(' · ')} · Līdza Deploy`;
}

/** navSection is the sidebar entry a view belongs to. */
export function navSection(route: Route): CollectionKey | undefined {
  return route.collection;
}
