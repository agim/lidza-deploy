import { describe, expect, it } from 'vitest';
import { type Route, breadcrumbs, buildPath, createHref, indexHref, parsePath, redirectPath, resourceHref, resources, routeTitle, singletonHref } from './resources';
import { legacyHashPath, navigate, normalizeLocation, withSticky } from './router';

const table: Array<[string, Route]> = [
  ['/console/applications', { view: 'index', collection: 'applications' }],
  ['/console/deployments', { view: 'index', collection: 'deployments' }],
  ['/console/servers', { view: 'index', collection: 'servers' }],
  ['/console/audit', { view: 'index', collection: 'audit' }],
  ['/console/errors?app=portal', { view: 'index', collection: 'errors', query: { app: 'portal' } }],
  ['/console/applications/new', { view: 'create', collection: 'applications' }],
  ['/console/applications/new?server=local', { view: 'create', collection: 'applications', query: { server: 'local' } }],
  ['/console/servers/new', { view: 'create', collection: 'servers' }],
  ['/console/team/new', { view: 'create', collection: 'team' }],
  ['/console/applications/portal', { view: 'detail', collection: 'applications', id: 'portal', tab: 'overview' }],
  ['/console/applications/portal/settings', { view: 'detail', collection: 'applications', id: 'portal', tab: 'settings' }],
  ['/console/applications/portal/logs', { view: 'detail', collection: 'applications', id: 'portal', tab: 'logs' }],
  ['/console/applications/portal/data', { view: 'detail', collection: 'applications', id: 'portal', tab: 'data' }],
  ['/console/servers/local', { view: 'detail', collection: 'servers', id: 'local', tab: 'overview' }],
  ['/console/servers/local/firewall', { view: 'detail', collection: 'servers', id: 'local', tab: 'firewall' }],
  ['/console/servers/local/console', { view: 'detail', collection: 'servers', id: 'local', tab: 'console' }],
  ['/console/databases/portal', { view: 'detail', collection: 'databases', id: 'portal', tab: 'overview' }],
  ['/console/databases/portal/schedule', { view: 'detail', collection: 'databases', id: 'portal', tab: 'schedule' }],
  ['/console/deployments/build-one', { view: 'detail', collection: 'deployments', id: 'build-one', tab: 'overview' }],
  ['/console/deployments/build-one/log', { view: 'detail', collection: 'deployments', id: 'build-one', tab: 'log' }],
  ['/console/errors/fixture-error?app=portal', { view: 'detail', collection: 'errors', id: 'fixture-error', tab: 'overview', query: { app: 'portal' } }],
  ['/console/integrations', { view: 'singleton', collection: 'integrations', tab: 'github' }],
  ['/console/integrations/storage', { view: 'singleton', collection: 'integrations', tab: 'storage' }],
  ['/console/account', { view: 'singleton', collection: 'account', tab: 'two-factor' }],
];

describe('console path routes', () => {
  it('parses and builds every view symmetrically', () => {
    for (const [path, route] of table) {
      const [pathname, search = ''] = path.split('?');
      expect(parsePath(pathname, search ? `?${search}` : ''), path).toEqual(route);
      expect(buildPath(route), path).toBe(path);
    }
  });

  it('round-trips identifiers that need escaping', () => {
    const path = resourceHref('applications', 'team/app one', 'environment');
    expect(path).toBe('/console/applications/team%2Fapp%20one/environment');
    expect(parsePath(path)).toEqual({ view: 'detail', collection: 'applications', id: 'team/app one', tab: 'environment' });
  });

  it('opens the first tab at the bare path and reports unknown views', () => {
    expect(resourceHref('servers', 'local', 'overview')).toBe('/console/servers/local');
    expect(resourceHref('servers', 'local', 'bogus')).toBe('/console/servers/local');
    expect(singletonHref('integrations', 'github')).toBe('/console/integrations');
    expect(parsePath('/console/not-a-page')).toEqual({ view: 'not-found' });
    expect(parsePath('/console/servers/local/bogus')).toEqual({ view: 'not-found' });
    expect(parsePath('/console/servers/local/firewall/extra')).toEqual({ view: 'not-found' });
    expect(parsePath('/console/servers/new/firewall')).toEqual({ view: 'not-found' });
    expect(parsePath('/console/audit/entry')).toEqual({ view: 'not-found' });
    expect(parsePath('/console/integrations/bogus')).toEqual({ view: 'not-found' });
    // An error group belongs to an application.
    expect(parsePath('/console/errors/fixture-error', '?app=')).toEqual({ view: 'index', collection: 'errors', query: { app: 'fixture-error' } });
  });

  it('links indexes, resources and create forms', () => {
    expect(indexHref('servers')).toBe('/console/servers');
    expect(indexHref('integrations')).toBe('/console/integrations');
    expect(indexHref('security', { app: 'portal' })).toBe('/console/security?app=portal');
    expect(createHref('applications', { server: 'local' })).toBe('/console/applications/new?server=local');
    expect(resourceHref('applications', 'portal', 'processes')).toBe('/console/applications/portal/processes');
  });

  it('defines every collection with an overview first and unique tabs', () => {
    for (const def of Object.values(resources)) {
      const ids = def.tabs.map(tab => tab.id);
      expect(new Set(ids).size, def.key).toBe(ids.length);
      if (def.kind === 'collection' && ids.length) expect(ids[0], def.key).toBe('overview');
    }
  });
});

describe('redirects from earlier URLs', () => {
  const redirects: Array<[string, string]> = [
    ['/console', '/console/applications'],
    ['/console.html', '/console/applications'],
    ['/console/new-application', '/console/applications/new'],
    ['/console/new-application?server=local', '/console/applications/new?server=local'],
    ['/console/applications?move=portal', '/console/applications/portal/move'],
    ['/console/applications?move=portal&demo=1', '/console/applications/portal/move?demo=1'],
    ['/console/applications/portal/settings/general', '/console/applications/portal/settings'],
    ['/console/applications/portal/settings/environment', '/console/applications/portal/environment'],
    ['/console/applications/portal/settings/resources', '/console/applications/portal/data'],
    ['/console/applications/portal/settings/processes', '/console/applications/portal/processes'],
    ['/console/applications/portal/cache', '/console/applications/portal/data'],
    ['/console/servers/local/containers', '/console/servers/local/processes'],
    ['/console/servers/local/upgrade', '/console/servers/local/agent'],
    ['/console/servers/local/overview', '/console/servers/local'],
    ['/console/integrations/github', '/console/integrations'],
    ['/console/account/two-factor', '/console/account'],
    ['/console/errors/portal', '/console/errors?app=portal'],
    ['/console/security/portal', '/console/security?app=portal'],
  ];
  it('maps every earlier URL to its current view', () => {
    for (const [from, to] of redirects) {
      const [pathname, search = ''] = from.split('?');
      expect(redirectPath(pathname, search ? `?${search}` : ''), from).toBe(to);
      const [toPath, toSearch = ''] = to.split('?');
      expect(parsePath(pathname, search ? `?${search}` : ''), from).toEqual(parsePath(toPath, toSearch ? `?${toSearch}` : ''));
    }
  });

  it('leaves current URLs alone', () => {
    for (const [path] of table) { const [pathname, search = ''] = path.split('?'); expect(redirectPath(pathname, search ? `?${search}` : ''), path).toBeNull(); }
    for (const path of ['/console/applications/portal/settings', '/console/applications/portal/logs', '/console/databases/portal', '/console/deployments/build-one']) expect(redirectPath(path), path).toBeNull();
  });

  it('rewrites an earlier URL in place on entry and on navigation', () => {
    history.replaceState(null, '', '/console/servers/local/containers?demo=1');
    const before = history.length;
    normalizeLocation();
    expect(location.pathname + location.search).toBe('/console/servers/local/processes?demo=1');
    expect(history.length).toBe(before);
    navigate('/console/applications/portal/cache');
    expect(location.pathname + location.search).toBe('/console/applications/portal/data?demo=1');
  });
});

describe('breadcrumbs', () => {
  const trail = (path: string, name?: string) => { const [pathname, search = ''] = path.split('?'); return breadcrumbs(parsePath(pathname, search ? `?${search}` : ''), name); };
  it('link every segment but the current one', () => {
    expect(trail('/console/servers/local/firewall', 'Production')).toEqual([
      { label: 'Home', href: '/console/applications' }, { label: 'Servers', href: '/console/servers' },
      { label: 'Production', href: '/console/servers/local' }, { label: 'Firewall' },
    ]);
    expect(trail('/console/servers/local', 'Production')).toEqual([{ label: 'Home', href: '/console/applications' }, { label: 'Servers', href: '/console/servers' }, { label: 'Production' }]);
    expect(trail('/console/servers')).toEqual([{ label: 'Home', href: '/console/applications' }, { label: 'Servers' }]);
  });

  it('names a resource by its id until its name is known', () => {
    expect(trail('/console/databases/portal/backups').map(crumb => crumb.label)).toEqual(['Home', 'Databases', 'portal', 'Backups']);
  });

  it('covers singletons, create forms, query resources and not found', () => {
    expect(trail('/console/integrations')).toEqual([{ label: 'Home', href: '/console/applications' }, { label: 'Integrations' }]);
    expect(trail('/console/integrations/storage')).toEqual([{ label: 'Home', href: '/console/applications' }, { label: 'Integrations', href: '/console/integrations' }, { label: 'Backup storage' }]);
    expect(trail('/console/applications/new')).toEqual([{ label: 'Home', href: '/console/applications' }, { label: 'Applications', href: '/console/applications' }, { label: 'New application' }]);
    expect(trail('/console/errors/fixture-error/occurrences?app=portal', 'Fixture request rejected')).toEqual([
      { label: 'Home', href: '/console/applications' }, { label: 'Errors', href: '/console/errors?app=portal' },
      { label: 'Fixture request rejected', href: '/console/errors/fixture-error?app=portal' }, { label: 'Occurrences' },
    ]);
    expect(trail('/console/nowhere')).toEqual([{ label: 'Home', href: '/console/applications' }, { label: 'Not found' }]);
  });

  it('every link of every trail parses to a real view', () => {
    for (const [path] of table) for (const crumb of trail(path, 'Name').slice(0, -1)) {
      const [pathname, search = ''] = crumb.href!.split('?');
      expect(parsePath(pathname, search ? `?${search}` : '').view, `${path} → ${crumb.href}`).not.toBe('not-found');
    }
  });

  it('titles the browser tab most specific first', () => {
    expect(routeTitle(parsePath('/console/servers/local/tuning'), 'Production')).toBe('Tuning · Production · Servers · Līdza Deploy');
    expect(routeTitle(parsePath('/console/applications'))).toBe('Applications · Līdza Deploy');
  });
});

describe('legacy hash links', () => {
  const legacy: Array<[string, string | null]> = [
    ['#applications', '/console/applications'],
    ['#servers', '/console/servers'],
    ['#deployments/', '/console/deployments'],
    ['#help/first-admin', '/console/help'],
    ['#new-application', '/console/applications/new'],
    ['#new-application/local', '/console/applications/new?server=local'],
    ['#security/portal', '/console/security?app=portal'],
    ['#page/settings/acme-analytics', '/console/applications/acme-analytics/settings'],
    ['#page/settings/team%2Fapp', '/console/applications/team%2Fapp/settings'],
    ['#page/runtime-logs/portal', '/console/applications/portal/logs'],
    ['#page/build-logs/build-one', '/console/deployments/build-one'],
    ['#page/database/portal', '/console/databases/portal'],
    ['#page/cache/portal', '/console/applications/portal/data'],
    ['#page/server/new', '/console/servers/new'],
    ['#page/console/local', '/console/servers/local/console'],
    ['#page/containers/local', '/console/servers/local/processes'],
    ['#page/upgrade/local', '/console/servers/local/agent'],
    ['#page/two-factor/account', '/console/account'],
    ['#not-a-page', null], ['#page/bogus/x', null], ['#page/settings', null], ['', null], ['#main-content', null],
  ];
  it('maps every old route to its path', () => {
    for (const [hash, path] of legacy) expect(legacyHashPath(hash), hash).toBe(path);
  });

  it('rewrites old bookmarks in place, keeping preview parameters', () => {
    history.replaceState(null, '', '/console.html?demo=1#page/settings/portal');
    normalizeLocation();
    expect(location.pathname + location.search + location.hash).toBe('/console/applications/portal/settings?demo=1');
    history.replaceState(null, '', '/console.html?demo=1&design=terminal#new-application/local');
    normalizeLocation();
    expect(location.pathname + location.search).toBe('/console/applications/new?demo=1&design=terminal&server=local');
  });

  it('opens applications for bare and unknown entry URLs', () => {
    for (const entry of ['/console.html', '/console', '/console/', '/console.html#unknown', '/console.html?github=connected']) {
      history.replaceState(null, '', entry);
      normalizeLocation();
      expect(location.pathname, entry).toBe('/console/applications');
      expect(location.hash, entry).toBe('');
    }
    expect(location.search).toBe('?github=connected');
  });
});

describe('preview parameters', () => {
  it('follow the visitor through panel links', () => {
    expect(withSticky('/console/servers', '?demo=1&design=walnut&q=x')).toBe('/console/servers?demo=1&design=walnut');
    expect(withSticky('/console/servers?demo=0', '?demo=1')).toBe('/console/servers?demo=0');
    expect(withSticky('/console/servers', '')).toBe('/console/servers');
  });
});
