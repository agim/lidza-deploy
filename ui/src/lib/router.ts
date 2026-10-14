import type { Route } from './types';

const pages = new Set(['applications', 'deployments', 'errors', 'security', 'servers', 'databases', 'remote-backups', 'integrations', 'defaults', 'updates', 'team', 'audit', 'help', 'new-application']);

export function parseRoute(hash = location.hash): Route {
  const clean = hash.replace(/^#/, '').replace(/\/$/, '') || 'applications';
  const parts = clean.split('/').filter(Boolean);
  if (parts[0] === 'page' && parts[1]) return { page: 'detail', detail: parts[1], id: decodeURIComponent(parts.slice(2).join('/')) };
  return pages.has(parts[0]) ? { page: parts[0] as Route['page'], id: parts.slice(1).join('/') || undefined } : { page: 'applications' };
}

export function href(page: string, id?: string) {
  return `#${page}${id ? `/${encodeURIComponent(id)}` : ''}`;
}

export function detailHref(detail: string, id: string) {
  return `#page/${detail}/${encodeURIComponent(id)}`;
}
