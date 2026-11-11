import type { ReactNode } from 'react';
import { CheckCircle2, Circle, CircleAlert, Clock3, Info, LoaderCircle, XCircle } from 'lucide-react';
import { cx } from './ui';

/*
 * CheckList: a list of results, each a status icon on the first line of its
 * text (readiness checks, DNS answers, preflight results). The icon sits in a
 * box one line high, so it stays on the row of the text however long the text
 * wraps. Styles: .check-list in console.css.
 */

export type CheckStatus = 'ok' | 'warning' | 'failed' | 'waiting' | 'running' | 'pending' | 'info';

export interface CheckListItem {
  /** React key; defaults to the index. */
  id?: string;
  status: CheckStatus;
  children: ReactNode;
  /** A second, muted line under the text. */
  detail?: ReactNode;
  /** Replaces the status icon (still coloured by the status). */
  icon?: ReactNode;
}

const statusLabels: Record<CheckStatus, string> = { ok: 'Passed', warning: 'Needs attention', failed: 'Failed', waiting: 'Waiting', running: 'Running', pending: 'Not started', info: 'Note' };

export function CheckIcon({ status }: { status: CheckStatus }) {
  if (status === 'ok') return <CheckCircle2 size={14} />;
  if (status === 'warning') return <CircleAlert size={14} />;
  if (status === 'failed') return <XCircle size={14} />;
  if (status === 'waiting') return <Clock3 size={14} />;
  if (status === 'running') return <LoaderCircle size={14} className="spin" />;
  if (status === 'info') return <Info size={14} />;
  return <Circle size={14} />;
}

/** checkStatus maps a boolean result to ok, or to the given status when it failed. */
export function checkStatus(ok: boolean, otherwise: CheckStatus = 'warning'): CheckStatus { return ok ? 'ok' : otherwise; }

export function CheckList({ items, className, 'aria-label': label }: { items: CheckListItem[]; className?: string; 'aria-label'?: string }) {
  if (!items.length) return null;
  return <ul className={cx('check-list', className)} aria-label={label}>{items.map((item, index) => <li key={item.id ?? index} className={`check-list-item check-list-${item.status}`} data-status={item.status}>
    <span className="check-list-icon" aria-hidden="true">{item.icon ?? <CheckIcon status={item.status} />}</span>
    <span className="sr-only">{statusLabels[item.status]}: </span>
    <span className="check-list-text">{item.children}{item.detail && <small className="check-list-detail">{item.detail}</small>}</span>
  </li>)}</ul>;
}
