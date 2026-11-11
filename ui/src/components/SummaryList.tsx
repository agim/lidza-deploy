import type { ReactNode } from 'react';
import { cx } from './ui';

/*
 * SummaryList: the console's key-value summary (plans, detected settings,
 * failure facts, measurements). Labels in a narrow muted column, values in
 * --text-body that wrap anywhere, so long hostnames, IPv6 addresses and paths
 * never overflow; one column on phones. Styles: .summary-list in console.css.
 */

export interface SummaryItem {
  label: ReactNode;
  value: ReactNode;
  /** React key; defaults to the label when it is a string. */
  id?: string;
}

export function SummaryList({ items, className, 'aria-label': label }: { items: Array<SummaryItem | false | null | undefined | '' | 0>; className?: string; 'aria-label'?: string }) {
  const rows = items.filter((item): item is SummaryItem => Boolean(item));
  if (!rows.length) return null;
  return <dl className={cx('summary-list', className)} aria-label={label}>{rows.map((row, index) => <div key={row.id ?? (typeof row.label === 'string' ? row.label : index)}><dt>{row.label}</dt><dd>{row.value}</dd></div>)}</dl>;
}
