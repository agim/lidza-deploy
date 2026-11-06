import { describe, expect, it } from 'vitest';
import type { Route } from './types';
import { buildPath, detailHref, href, legacyHashPath, normalizeLocation, parentPath, parsePath, routeTitle, withSticky } from './router';

const table: Array<[string, Route]> = [
  ['/console/applications', { page: 'applications' }],
  ['/console/deployments', { page: 'deployments' }],
  ['/console/servers', { page: 'servers' }],
  ['/console/remote-backups', { page: 'remote-backups' }],
  ['/console/errors/portal', { page: 'errors', id: 'portal' }],
  ['/console/security/portal', { page: 'security', id: 'portal' }],
  ['/console/applications/acme-analytics/settings', { page: 'detail', detail: 'settings', id: 'acme-analytics' }],
  ['/console/applications/acme-analytics/settings/environment', { page: 'detail', detail: 'settings', id: 'acme-analytics', tab: 'environment' }],
  ['/console/applications/acme-analytics/logs', { page: 'detail', detail: 'runtime-logs', id: 'acme-analytics' }],
  ['/console/applications/acme-analytics/cache', { page: 'detail', detail: 'cache', id: 'acme-analytics' }],
  ['/console/deployments/build-one', { page: 'detail', detail: 'build-logs', id: 'build-one' }],
  ['/console/databases/portal', { page: 'detail', detail: 'database', id: 'portal' }],
  ['/console/servers/local', { page: 'detail', detail: 'server', id: 'local' }],
  ['/console/servers/new', { page: 'detail', detail: 'server', id: 'new' }],
  ['/console/servers/local/console', { page: 'detail', detail: 'console', id: 'local' }],
  ['/console/servers/local/containers', { page: 'detail', detail: 'containers', id: 'local' }],
  ['/console/servers/local/cleanup', { page: 'detail', detail: 'cleanup', id: 'local' }],
  ['/console/servers/local/ssh', { page: 'detail', detail: 'ssh', id: 'local' }],
  ['/console/servers/local/upgrade', { page: 'detail', detail: 'upgrade', id: 'local' }],
  ['/console/account/two-factor', { page: 'detail', detail: 'two-factor', id: 'account' }],
  ['/console/new-application', { page: 'new-application' }],
  ['/console/new-application?server=local', { page: 'new-application', id: 'local' }],
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
    const path = detailHref('settings', 'team/app one');
    expect(path).toBe('/console/applications/team%2Fapp%20one/settings');
    expect(parsePath(path)).toEqual({ page: 'detail', detail: 'settings', id: 'team/app one' });
  });

  it('opens applications at the base path and reports unknown views', () => {
    expect(parsePath('/console')).toEqual({ page: 'applications' });
    expect(parsePath('/console/')).toEqual({ page: 'applications' });
    expect(parsePath('/console/not-a-page')).toEqual({ page: 'not-found' });
    expect(parsePath('/console/servers/local/bogus')).toEqual({ page: 'not-found' });
    expect(parsePath('/console/applications/x/settings/general/extra')).toEqual({ page: 'not-found' });
  });

  it('links sections and details', () => {
    expect(href('servers')).toBe('/console/servers');
    expect(href('new-application', 'local')).toBe('/console/new-application?server=local');
    expect(href('security', 'portal')).toBe('/console/security/portal');
    expect(detailHref('settings', 'portal', 'processes')).toBe('/console/applications/portal/settings/processes');
  });

  it('names each view in the page title', () => {
    expect(routeTitle({ page: 'applications' })).toBe('Applications · Līdza Deploy');
    expect(routeTitle(parsePath('/console/applications/portal/settings/environment'))).toBe('portal settings · Līdza Deploy');
    expect(routeTitle(parsePath('/console/applications/portal/logs'))).toBe('portal runtime logs · Līdza Deploy');
    expect(routeTitle(parsePath('/console/servers/local/console'))).toBe('local console · Līdza Deploy');
  });

  it('finds the parent view of a detail page', () => {
    expect(parentPath(parsePath('/console/deployments/build-one'))).toBe('/console/deployments');
    expect(parentPath(parsePath('/console/servers/local/console'))).toBe('/console/servers');
    expect(parentPath(parsePath('/console/applications/portal/cache'))).toBe('/console/applications/portal/settings');
    expect(parentPath(parsePath('/console/databases/portal'))).toBe('/console/databases');
    expect(parentPath(parsePath('/console/applications/portal/settings'))).toBe('/console/applications');
  });
});

describe('legacy hash links', () => {
  const legacy: Array<[string, string | null]> = [
    ['#applications', '/console/applications'],
    ['#servers', '/console/servers'],
    ['#remote-backups', '/console/remote-backups'],
    ['#deployments/', '/console/deployments'],
    ['#help/first-admin', '/console/help'],
    ['#new-application', '/console/new-application'],
    ['#new-application/local', '/console/new-application?server=local'],
    ['#security/portal', '/console/security/portal'],
    ['#errors/portal', '/console/errors/portal'],
    ['#page/settings/acme-analytics', '/console/applications/acme-analytics/settings'],
    ['#page/settings/team%2Fapp', '/console/applications/team%2Fapp/settings'],
    ['#page/runtime-logs/portal', '/console/applications/portal/logs'],
    ['#page/build-logs/build-one', '/console/deployments/build-one'],
    ['#page/database/portal', '/console/databases/portal'],
    ['#page/cache/portal', '/console/applications/portal/cache'],
    ['#page/server/new', '/console/servers/new'],
    ['#page/console/local', '/console/servers/local/console'],
    ['#page/containers/local', '/console/servers/local/containers'],
    ['#page/cleanup/local', '/console/servers/local/cleanup'],
    ['#page/ssh/local', '/console/servers/local/ssh'],
    ['#page/upgrade/local', '/console/servers/local/upgrade'],
    ['#page/two-factor/account', '/console/account/two-factor'],
    ['#not-a-page', null],
    ['#page/bogus/x', null],
    ['#page/settings', null],
    ['', null],
    ['#main-content', null],
  ];
  it('maps every old route to its path', () => {
    for (const [hash, path] of legacy) expect(legacyHashPath(hash), hash).toBe(path);
  });

  it('rewrites old bookmarks in place, keeping preview parameters', () => {
    history.replaceState(null, '', '/console.html?demo=1#page/settings/portal');
    const before = history.length;
    normalizeLocation();
    expect(location.pathname + location.search + location.hash).toBe('/console/applications/portal/settings?demo=1');
    expect(history.length).toBe(before);
    history.replaceState(null, '', '/console.html?demo=1&design=terminal#new-application/local');
    normalizeLocation();
    expect(location.pathname + location.search).toBe('/console/new-application?demo=1&design=terminal&server=local');
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

  it('leaves current path routes untouched', () => {
    history.replaceState(null, '', '/console/servers/local/console?x=1');
    normalizeLocation();
    expect(location.pathname + location.search).toBe('/console/servers/local/console?x=1');
    expect(history.state).toEqual({ lidza: 1, idx: 0 });
  });
});

describe('preview parameters', () => {
  it('follow the visitor through panel links', () => {
    expect(withSticky('/console/servers', '?demo=1&design=walnut&q=x')).toBe('/console/servers?demo=1&design=walnut');
    expect(withSticky('/console/servers?demo=0', '?demo=1')).toBe('/console/servers?demo=0');
    expect(withSticky('/console/servers', '')).toBe('/console/servers');
  });
});
