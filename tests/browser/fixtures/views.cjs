// Every view of the console with fixture data (fixtures/console.cjs and
// fixtures/server-host.cjs), and the earlier URLs that redirect to a view.
// Shared by layout-spacing.cjs (structure and spacing) and console-errors.cjs
// (no console errors, failed requests or form hints). A new view or tab is
// added here once and both suites cover it.
// A view is [path, template, open tab]: indexes use the index template; every
// tab of every resource (detail or singleton) uses the detail template with
// that tab selected.
const index = path => [path, 'index'];
const tabs = (base, ids) => ids.map((id, i) => [i === 0 ? base : `${base}/${id}`, 'detail', id]);
const withQuery = (rows, query) => rows.map(([path, template, tab]) => [`${path}${query}`, template, tab]);
const views = [
  ...['applications', 'deployments', 'servers', 'databases', 'deleted-apps', 'team', 'audit', 'errors', 'security'].map(name => index(`/console/${name}`)),
  ...tabs('/console/applications/portal', ['overview', 'deployments', 'logs', 'environment', 'domains', 'data', 'backups', 'processes', 'move', 'settings']),
  ...tabs('/console/servers/local', ['overview', 'applications', 'processes', 'ingress', 'updates', 'tuning', 'firewall', 'cleanup', 'retained', 'console', 'ssh', 'agent']),
  ...tabs('/console/databases/portal', ['overview', 'backups', 'schedule', 'excluded', 'settings']),
  ...tabs('/console/deployments/build-one', ['overview', 'log']),
  ...tabs('/console/deleted-apps/archive-one', ['overview', 'settings']),
  ...tabs('/console/team/ops%40example.com', ['overview', 'settings']),
  ...withQuery(tabs('/console/errors/fixture-error', ['overview', 'occurrences']), '?app=portal'),
  ...withQuery(tabs('/console/security/fixture-error', ['overview', 'occurrences']), '?app=portal'),
  ...tabs('/console/integrations', ['github', 'storage', 'email']),
  ...tabs('/console/defaults', ['go', 'rails', 'astro', 'node']),
  ...tabs('/console/remote-backups', ['objects', 'retention']),
  ...tabs('/console/updates', ['overview', 'channel']),
  ...tabs('/console/control-panel', ['overview', 'move']),
  ...tabs('/console/help', ['guides', 'components']),
  ...tabs('/console/account', ['two-factor']),
  ['/console/applications/new', 'create'], ['/console/servers/new', 'create'], ['/console/team/new', 'create'],
  ['/console/no-such-page', 'not-found'], ['/console/servers/local/no-such-tab', 'not-found'],
];
// Earlier URLs and the view each one opens now.
const redirects = [
  ['/console/servers/local/containers', '/console/servers/local/processes'], ['/console/servers/local/upgrade', '/console/servers/local/agent'],
  ['/console/applications/portal/settings/environment', '/console/applications/portal/environment'], ['/console/applications/portal/cache', '/console/applications/portal/data'],
  ['/console/applications/portal/settings/general', '/console/applications/portal/settings'], ['/console/new-application?server=local', '/console/applications/new?server=local'],
  ['/console/applications?move=portal', '/console/applications/portal/move'], ['/console/account/two-factor', '/console/account'], ['/console/errors/portal', '/console/errors?app=portal'],
  ['/console/servers/local/overview', '/console/servers/local'],
];
module.exports = { views, redirects };
