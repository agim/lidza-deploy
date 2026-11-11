import { ChevronRight, MoreHorizontal } from 'lucide-react';
import { Fragment } from 'react';
import type { Crumb } from '../../lib/resources';
import { withSticky } from '../../lib/router';
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from '../primitives/dropdown-menu';

/**
 * Breadcrumbs: the location of the view, generated from the route
 * (lib/resources.ts breadcrumbs). Every segment but the current one is a link.
 * On phones the middle segments collapse into a "…" menu of the same links.
 */
export function Breadcrumbs({ trail }: { trail: Crumb[] }) {
  const middle = trail.slice(1, -1);
  return <nav className="breadcrumbs" aria-label="Breadcrumb" data-breadcrumbs>
    <ol className="breadcrumb-list">
      {trail.map((crumb, index) => {
        const last = index === trail.length - 1;
        return <Fragment key={`${index}-${crumb.label}`}>
          {index === 1 && middle.length > 0 && <li className="breadcrumb-collapse" aria-hidden={false}>
            <DropdownMenu><DropdownMenuTrigger asChild><button type="button" className="breadcrumb-more" aria-label="Show the path to this page"><MoreHorizontal size={14} /></button></DropdownMenuTrigger>
              <DropdownMenuContent align="start" className="menu-content">{middle.map(item => <DropdownMenuItem key={item.label} asChild className="menu-item"><a href={withSticky(item.href || '')}>{item.label}</a></DropdownMenuItem>)}</DropdownMenuContent></DropdownMenu>
            <ChevronRight size={13} aria-hidden="true" />
          </li>}
          <li className={index > 0 && !last ? 'breadcrumb-middle' : undefined} data-crumb={last ? 'current' : 'link'}>
            {last ? <strong aria-current="page">{crumb.label}</strong> : <a href={withSticky(crumb.href || '')}>{crumb.label}</a>}
            {!last && <ChevronRight size={13} aria-hidden="true" />}
          </li>
        </Fragment>;
      })}
    </ol>
  </nav>;
}
