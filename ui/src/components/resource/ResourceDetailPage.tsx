import type { ReactNode } from 'react';
import { type CollectionKey, type TabDef, resources, resourceHref, singletonHref } from '../../lib/resources';
import { navigate, withSticky } from '../../lib/router';
import { SectionBoundary } from '../SectionBoundary';
import { Notice, PageHeader, Tabs } from '../ui';
import { useNameResource } from './ResourceName';

/**
 * ResourceDetailPage: the one layout of every resource. A header (name,
 * status, primary facts, actions), then the resource's property tabs, one
 * URL each: /console/<collection>/<id>/<tab>, the first tab at the bare
 * path. A singleton (integrations, defaults, …) omits the id. Only the open
 * tab's panel renders, inside its own SectionBoundary, so each tab loads its
 * own data. Destructive actions belong in a Danger zone on the settings tab.
 */
export function ResourceDetailPage({ resource, id, query, name, title, status, facts, description, actions, tab, tabs, notices, missing, children }: {
  resource: CollectionKey;
  /** The resource id; omitted for a singleton. */
  id?: string;
  /** Query parameters that are part of the resource's URL (an error group's application). */
  query?: Record<string, string>;
  /** Display name for the breadcrumb; defaults to the title. */
  name?: string;
  title: string;
  status?: ReactNode;
  facts?: Array<ReactNode | false | null | undefined>;
  description?: string;
  actions?: ReactNode;
  /** The open tab (from the route). */
  tab: string;
  /** The tabs this resource has; defaults to all the collection's tabs. */
  tabs?: TabDef[];
  notices?: ReactNode;
  /** Shown instead of the tabs when the resource does not exist. */
  missing?: ReactNode;
  children: ReactNode;
}) {
  useNameResource(name || title);
  const all = resources[resource].tabs;
  const visible = tabs || all;
  const hrefOf = (next: string) => id === undefined ? singletonHref(resource, next) : resourceHref(resource, id, next, query);
  const current = visible.find(item => item.id === tab);
  const label = all.find(item => item.id === tab)?.label || tab;
  return <div className="resource-page" data-template="detail" data-resource={resource} data-tab={tab}>
    <PageHeader title={title} status={status} facts={facts} description={description} actions={actions} />
    {notices}
    {missing || <Tabs scroll label="Sections" active={current ? tab : ''} onChange={next => navigate(hrefOf(next))} items={visible.map(item => ({ ...item, href: withSticky(hrefOf(item.id)) }))}>
      <SectionBoundary label={label} resetKey={`${resource}/${id}/${tab}`}>
        {current ? <div className="tab-panel" data-tab-panel={tab}>{children}</div> : <Notice>{title} has no {label.toLowerCase()}. <a href={withSticky(hrefOf(visible[0]?.id || ''))}>Open its {(visible[0]?.label || 'overview').toLowerCase()}</a>.</Notice>}
      </SectionBoundary>
    </Tabs>}
  </div>;
}

/** visibleTabs keeps the collection's tabs whose ids are not hidden. */
export function visibleTabs(resource: CollectionKey, hidden: Array<string | false | null | undefined>) {
  return resources[resource].tabs.filter(item => !hidden.includes(item.id));
}
