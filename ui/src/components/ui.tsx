import * as AlertDialog from '@radix-ui/react-alert-dialog';
import * as DropdownMenu from '@radix-ui/react-dropdown-menu';
import { AlertTriangle, Check, ChevronDown, Copy, LoaderCircle, MoreHorizontal, Search, X } from 'lucide-react';
import { type ButtonHTMLAttributes, type HTMLAttributes, type InputHTMLAttributes, type ReactNode, useEffect, useState } from 'react';

export function cx(...values: Array<string | false | null | undefined>) { return values.filter(Boolean).join(' '); }

export function Button({ variant = 'secondary', size = 'md', className, children, ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: 'primary' | 'secondary' | 'ghost' | 'danger'; size?: 'sm' | 'md' | 'icon' }) {
  return <button className={cx('button', `button-${variant}`, `button-${size}`, className)} {...props}>{children}</button>;
}

export function IconButton({ label, children, ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { label: string }) {
  return <Button size="icon" variant="ghost" aria-label={label} title={label} {...props}>{children}</Button>;
}

export function Card({ className, ...props }: HTMLAttributes<HTMLElement>) { return <section className={cx('card', className)} {...props} />; }
export function CardHeader({ className, ...props }: HTMLAttributes<HTMLDivElement>) { return <div className={cx('card-header', className)} {...props} />; }
export function CardBody({ className, ...props }: HTMLAttributes<HTMLDivElement>) { return <div className={cx('card-body', className)} {...props} />; }

export function Badge({ tone = 'neutral', children }: { tone?: 'success' | 'warning' | 'danger' | 'neutral' | 'accent'; children: ReactNode }) {
  return <span className={cx('badge', `badge-${tone}`)}>{children}</span>;
}

export function StatusBadge({ status = 'unknown' }: { status?: string }) {
  const lower = status.toLowerCase();
  const tone = ['live', 'ready', 'done', 'running', 'healthy'].includes(lower) ? 'success' : ['failed', 'error', 'unhealthy'].includes(lower) ? 'danger' : ['building', 'queued', 'pending', 'retrying', 'waiting'].includes(lower) ? 'warning' : 'neutral';
  return <Badge tone={tone}><span className="status-dot" />{status || 'unknown'}</Badge>;
}

export function Field({ label, hint, error, className, children }: { label: string; hint?: ReactNode; error?: string; className?: string; children: ReactNode }) {
  return <label className={cx('field', className)}><span className="field-label">{label}</span>{children}{hint && <span className="field-hint">{hint}</span>}{error && <span className="field-error">{error}</span>}</label>;
}

export function SearchField(props: InputHTMLAttributes<HTMLInputElement>) {
  return <label className="search-field"><Search size={17} aria-hidden="true" /><span className="sr-only">Search</span><input type="search" {...props} /></label>;
}

export function PageHeader({ eyebrow, title, description, actions, back }: { eyebrow?: string; title: string; description?: string; actions?: ReactNode; back?: string }) {
  return <header className="page-header"><div className="page-title-row">{back && <a className="back-link" href={back}>← Back</a>}<div>{eyebrow && <p className="eyebrow">{eyebrow}</p>}<h1>{title}<span aria-hidden="true">.</span></h1>{description && <p className="page-description">{description}</p>}</div></div>{actions && <div className="page-actions">{actions}</div>}</header>;
}

export function SectionHeader({ title, description, actions }: { title: string; description?: string; actions?: ReactNode }) {
  return <div className="section-header"><div><h2>{title}</h2>{description && <p>{description}</p>}</div>{actions && <div className="section-actions">{actions}</div>}</div>;
}

export function Toolbar({ children }: { children: ReactNode }) { return <div className="toolbar">{children}</div>; }

export function EmptyState({ icon, title, description, action }: { icon?: ReactNode; title: string; description: string; action?: ReactNode }) {
  return <div className="empty-state">{icon && <div className="empty-icon">{icon}</div>}<h2>{title}</h2><p>{description}</p>{action}</div>;
}

export function Loading({ label = 'Loading…', overlay = false }: { label?: string; overlay?: boolean }) {
  return <div className={cx('loading', overlay && 'loading-overlay')} role="status"><LoaderCircle className="spin" size={22} /><span>{label}</span></div>;
}

export function ErrorBanner({ children, onDismiss }: { children: ReactNode; onDismiss?: () => void }) {
  return <div className="alert alert-danger" role="alert"><AlertTriangle size={18} /><div>{children}</div>{onDismiss && <IconButton label="Dismiss" onClick={onDismiss}><X size={17} /></IconButton>}</div>;
}

export function Notice({ children }: { children: ReactNode }) { return <div className="alert alert-info" role="status">{children}</div>; }

export function ActionMenu({ label = 'More actions', children }: { label?: string; children: ReactNode }) {
  return <DropdownMenu.Root><DropdownMenu.Trigger asChild><Button variant="secondary" size="sm"><MoreHorizontal size={16} />{label}<ChevronDown size={14} /></Button></DropdownMenu.Trigger><DropdownMenu.Portal><DropdownMenu.Content className="menu-content" align="end" sideOffset={7}>{children}</DropdownMenu.Content></DropdownMenu.Portal></DropdownMenu.Root>;
}

export function MenuItem({ icon, danger, children, onSelect }: { icon?: ReactNode; danger?: boolean; children: ReactNode; onSelect?: () => void }) {
  return <DropdownMenu.Item className={cx('menu-item', danger && 'menu-danger')} onSelect={onSelect}>{icon}{children}</DropdownMenu.Item>;
}

export function MenuLabel({ children }: { children: ReactNode }) { return <DropdownMenu.Label className="menu-label">{children}</DropdownMenu.Label>; }
export function MenuSeparator() { return <DropdownMenu.Separator className="menu-separator" />; }

export function ConfirmAction({ title, description, confirmLabel, destructive = false, children, onConfirm }: { title: string; description: ReactNode; confirmLabel: string; destructive?: boolean; children: ReactNode; onConfirm: () => void | Promise<void> }) {
  const [busy, setBusy] = useState(false);
  const confirm = async () => { setBusy(true); try { await onConfirm(); } finally { setBusy(false); } };
  return <AlertDialog.Root><AlertDialog.Trigger asChild>{children}</AlertDialog.Trigger><AlertDialog.Portal><AlertDialog.Overlay className="dialog-overlay" /><AlertDialog.Content className="dialog"><AlertDialog.Title>{title}</AlertDialog.Title><AlertDialog.Description>{description}</AlertDialog.Description><div className="dialog-actions"><AlertDialog.Cancel asChild><Button>Cancel</Button></AlertDialog.Cancel><AlertDialog.Action asChild><Button variant={destructive ? 'danger' : 'primary'} onClick={confirm} disabled={busy}>{busy && <LoaderCircle className="spin" size={16} />}{confirmLabel}</Button></AlertDialog.Action></div></AlertDialog.Content></AlertDialog.Portal></AlertDialog.Root>;
}

export function CopyButton({ value, label = 'Copy' }: { value: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  useEffect(() => { if (!copied) return; const timer = setTimeout(() => setCopied(false), 1600); return () => clearTimeout(timer); }, [copied]);
  return <Button size="sm" onClick={async () => { await navigator.clipboard.writeText(value); setCopied(true); }}>{copied ? <Check size={15} /> : <Copy size={15} />}{copied ? 'Copied' : label}</Button>;
}

export function ProgressBar({ value, tone = 'accent' }: { value: number; tone?: 'accent' | 'danger' | 'warning' }) {
  const safe = Math.max(0, Math.min(100, value || 0));
  return <div className="progress" aria-label={`${safe.toFixed(0)} percent`}><span className={`progress-${tone}`} style={{ width: `${safe}%` }} /></div>;
}

export function Tabs({ items, active, onChange }: { items: Array<{ id: string; label: string }>; active: string; onChange: (id: string) => void }) {
  return <div className="tabs" role="tablist">{items.map(item => <button type="button" key={item.id} role="tab" aria-selected={active === item.id} className={active === item.id ? 'active' : ''} onClick={() => onChange(item.id)}>{item.label}</button>)}</div>;
}
