import type { ReactNode } from 'react';
import type { CollectionKey } from '../../lib/resources';
import { ResourceList, type Column } from '../ResourceList';
import { Card, Loading, PageHeader, Toolbar } from '../ui';

/**
 * ResourceIndexPage: the one layout of every index. A header (title, one
 * line, count, primary action), a filter row, then one table whose name
 * column links to each resource, and an empty state that offers creating
 * one. It holds no detail content: no per-item cards, logs or settings.
 * Phones show each row as a stacked block (ResourceList).
 */
export function ResourceIndexPage<T>({ resource, title, description, count, primaryAction, actions, filters, notices, loading, rows, columns, rowKey, empty, children }: {
  resource: CollectionKey;
  title: string;
  description: string;
  /** Total resources (not the filtered count). */
  count?: number;
  primaryAction?: ReactNode;
  /** Secondary header actions, such as Refresh. */
  actions?: ReactNode;
  filters?: ReactNode;
  notices?: ReactNode;
  loading?: boolean;
  rows: T[];
  columns: Column<T>[];
  rowKey: (row: T) => string;
  empty: ReactNode;
  /** Further lists of the same resource (e.g. permanently deleted), each its own Section. */
  children?: ReactNode;
}) {
  return <div className="resource-page" data-template="index" data-resource={resource}>
    <PageHeader title={title} description={description} count={count} actions={actions || primaryAction ? <>{actions}{primaryAction}</> : undefined} />
    {notices}
    {filters && <Toolbar>{filters}</Toolbar>}
    <Card className="resource-index-list" data-resource-list={resource}>
      {loading ? <Loading label={`Loading ${title.toLowerCase()}…`} /> : <ResourceList rows={rows} columns={columns} rowKey={rowKey} empty={empty} />}
    </Card>
    {children}
  </div>;
}

/** ResourceName: an index's name cell, a link to the resource with one line under it. */
export function ResourceName({ href, name, detail }: { href?: string; name: ReactNode; detail?: ReactNode }) {
  return <div className="resource-name">{href ? <a href={href}>{name}</a> : <strong>{name}</strong>}{detail && <small>{detail}</small>}</div>;
}
