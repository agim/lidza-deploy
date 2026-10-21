import * as AlertDialog from '@radix-ui/react-alert-dialog';
import { DropdownMenu as MenuRoot, DropdownMenuTrigger, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator } from './primitives/dropdown-menu';
import { AlertTriangle, Check, ChevronDown, Copy, LoaderCircle, MoreHorizontal, Search, X } from 'lucide-react';
import { Fragment, createContext, type ButtonHTMLAttributes, type FormEvent, type FormHTMLAttributes, type HTMLAttributes, type InputHTMLAttributes, type MouseEvent, type ReactNode, useContext, useEffect, useId, useState } from 'react';
import { Button as PrimitiveButton } from './primitives/button';
import { Card as PrimitiveCard } from './primitives/card';
import { Badge as PrimitiveBadge } from './primitives/badge';
import { Label as PrimitiveLabel } from './primitives/label';
import { Input } from './primitives/input';
import { Tabs as PrimitiveTabs, TabsList, TabsTrigger, TabsContent } from './primitives/tabs';
import { useActionFeedback } from './useActionFeedback';

export function cx(...values: Array<string | false | null | undefined>) { return values.filter(Boolean).join(' '); }

const FormPendingContext = createContext(false);

export function Button({ variant = 'secondary', size = 'md', className, children, onClick, disabled, pending = false, feedback = false, ...props }: Omit<ButtonHTMLAttributes<HTMLButtonElement>, 'onClick'> & { variant?: 'primary' | 'secondary' | 'ghost' | 'danger'; size?: 'sm' | 'md' | 'icon'; onClick?: (event: MouseEvent<HTMLButtonElement>) => unknown; pending?: boolean; feedback?: boolean }) {
  const action = useActionFeedback();
  const formPending = useContext(FormPendingContext);
  const busy = pending || action.pending || (props.type !== 'button' && formPending);
  const click = (event: MouseEvent<HTMLButtonElement>) => {
    if (busy || action.active.current || disabled) { event.preventDefault(); return; }
    try {
      const result = onClick?.(event);
      if (result && typeof (result as PromiseLike<unknown>).then === 'function') void action.run(() => result);
      else if (feedback) void action.run(() => undefined);
    } catch (reason) { void action.run(() => { throw reason; }); }
  };
  return <PrimitiveButton variant={variant === 'primary' ? 'default' : variant === 'danger' ? 'destructive' : variant === 'ghost' ? 'ghost' : 'outline'} size={size === 'md' ? 'default' : size} className={cx('button', `button-${variant}`, `button-${size}`, className)} {...props} disabled={disabled || busy} aria-busy={busy || undefined} data-pending={busy || undefined} onClick={click}>{children}{busy && <span className="button-progress" aria-hidden="true"><span /></span>}</PrimitiveButton>;
}

export function ActionForm({ onSubmit, children, ...props }: Omit<FormHTMLAttributes<HTMLFormElement>, 'onSubmit'> & { onSubmit: (event: FormEvent<HTMLFormElement>) => unknown }) {
  const action = useActionFeedback();
  return <FormPendingContext.Provider value={action.pending}><form {...props} aria-busy={action.pending || undefined} onSubmit={event => {
    event.preventDefault();
    if (!action.active.current) void action.run(() => onSubmit(event));
  }}>{children}</form></FormPendingContext.Provider>;
}

export function IconButton({ label, children, ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { label: string }) {
  return <Button size="icon" variant="ghost" aria-label={label} title={label} {...props}>{children}</Button>;
}

export function Card({ className, ...props }: HTMLAttributes<HTMLElement>) { return <PrimitiveCard className={cx('card', className)} {...props} />; }
export function CardHeader({ className, ...props }: HTMLAttributes<HTMLDivElement>) { return <div className={cx('card-header', className)} {...props} />; }
export function CardBody({ className, ...props }: HTMLAttributes<HTMLDivElement>) { return <div className={cx('card-body', className)} {...props} />; }

export function Badge({ tone = 'neutral', children }: { tone?: 'success' | 'warning' | 'danger' | 'neutral' | 'accent'; children: ReactNode }) {
  return <PrimitiveBadge variant="outline" className={cx('badge', `badge-${tone}`)}>{children}</PrimitiveBadge>;
}

export function StatusBadge({ status = 'unknown' }: { status?: string }) {
  const lower = status.toLowerCase();
  const tone = ['live', 'ready', 'done', 'running', 'healthy'].includes(lower) ? 'success' : ['failed', 'error', 'unhealthy'].includes(lower) ? 'danger' : ['building', 'queued', 'pending', 'retrying', 'waiting', 'setup_required'].includes(lower) ? 'warning' : 'neutral';
  return <Badge tone={tone}><span className="status-dot" />{status === 'setup_required' ? 'Setup required' : status || 'unknown'}</Badge>;
}

export function Field({ label, hint, error, className, children }: { label: string; hint?: ReactNode; error?: string; className?: string; children: ReactNode }) {
  return <PrimitiveLabel className={cx('field', className)}><span className="field-label">{label}</span>{children}{hint && <span className="field-hint">{hint}</span>}{error && <span className="field-error">{error}</span>}</PrimitiveLabel>;
}

export function SearchField(props: InputHTMLAttributes<HTMLInputElement>) {
  return <label className="search-field"><Search size={17} aria-hidden="true" /><span className="sr-only">Search</span><Input type="search" {...props} /></label>;
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

const DropdownMenu = { Root: MenuRoot, Trigger: DropdownMenuTrigger, Content: DropdownMenuContent, Item: DropdownMenuItem, Label: DropdownMenuLabel, Separator: DropdownMenuSeparator, Portal: Fragment };

const CloseMenuContext = createContext(() => undefined as void);

export function ActionMenu({ label = 'More actions', children, compact = false }: { label?: string; children: ReactNode; compact?: boolean }) {
  const [open, setOpen] = useState(false);
  return <CloseMenuContext.Provider value={() => setOpen(false)}><DropdownMenu.Root open={open} onOpenChange={setOpen}><DropdownMenu.Trigger asChild><Button variant={compact ? "ghost" : "secondary"} size={compact ? "icon" : "sm"} aria-label={label}><MoreHorizontal size={16} />{!compact && <>{label}<ChevronDown size={14} /></>}</Button></DropdownMenu.Trigger><DropdownMenu.Portal><DropdownMenu.Content className="menu-content" align="end" sideOffset={7}>{children}</DropdownMenu.Content></DropdownMenu.Portal></DropdownMenu.Root></CloseMenuContext.Provider>;
}

export function MenuItem({ icon, danger, children, onSelect }: { icon?: ReactNode; danger?: boolean; children: ReactNode; onSelect?: () => unknown }) {
  const action = useActionFeedback();
  const close = useContext(CloseMenuContext);
  return <DropdownMenu.Item className={cx('menu-item', danger && 'menu-danger')} disabled={action.pending} aria-busy={action.pending || undefined} onSelect={event => {
    if (action.active.current) { event.preventDefault(); return; }
    try {
      const result = onSelect?.();
      if (result && typeof (result as PromiseLike<unknown>).then === 'function') {
        event.preventDefault();
        void action.run(() => result).then(succeeded => { if (succeeded) close(); });
      }
    } catch (reason) { event.preventDefault(); void action.run(() => { throw reason; }); }
  }}>{action.pending ? <LoaderCircle className="spin" size={16} aria-hidden="true" /> : icon}{children}</DropdownMenu.Item>;
}

export function MenuLink({ href, icon, children }: { href: string; icon?: ReactNode; children: ReactNode }) {
  return <DropdownMenu.Item asChild className="menu-item"><a href={href}>{icon}{children}</a></DropdownMenu.Item>;
}

export function MenuLabel({ children }: { children: ReactNode }) { return <DropdownMenu.Label className="menu-label">{children}</DropdownMenu.Label>; }
export function MenuSeparator() { return <DropdownMenu.Separator className="menu-separator" />; }

export function ConfirmAction({ title, description, confirmLabel, destructive = false, children, onConfirm }: { title: string; description: ReactNode; confirmLabel: string; destructive?: boolean; children: ReactNode; onConfirm: () => void | Promise<void> }) {
  const [open, setOpen] = useState(false);
  const action = useActionFeedback();
  const confirm = async () => { if (await action.run(onConfirm)) setOpen(false); };
  return <AlertDialog.Root open={open} onOpenChange={value => { if (!action.pending) setOpen(value); }}><AlertDialog.Trigger asChild>{children}</AlertDialog.Trigger><AlertDialog.Portal><AlertDialog.Overlay className="dialog-overlay" /><AlertDialog.Content className="dialog"><AlertDialog.Title>{title}</AlertDialog.Title><AlertDialog.Description>{description}</AlertDialog.Description><div className="dialog-actions"><AlertDialog.Cancel asChild><Button type="button" disabled={action.pending}>Cancel</Button></AlertDialog.Cancel><Button type="button" variant={destructive ? 'danger' : 'primary'} onClick={confirm} pending={action.pending}>{confirmLabel}</Button></div></AlertDialog.Content></AlertDialog.Portal></AlertDialog.Root>;
}

export function PromptDialog({ open, title, description, label, value, submitLabel, inputProps, onValueChange, onOpenChange, onSubmit }: {
  open: boolean;
  title: string;
  description: ReactNode;
  label: string;
  value: string;
  submitLabel: string;
  inputProps?: Omit<InputHTMLAttributes<HTMLInputElement>, 'value' | 'onChange'>;
  onValueChange: (value: string) => void;
  onOpenChange: (open: boolean) => void;
  onSubmit: (value: string) => void | Promise<void>;
}) {
  const inputID = useId();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => { if (!open) { setBusy(false); setError(''); } }, [open]);
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const nextValue = value.trim();
    if (!nextValue || busy) return;
    setBusy(true);
    setError('');
    try {
      await onSubmit(nextValue);
      onOpenChange(false);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setBusy(false);
    }
  };
  return <AlertDialog.Root open={open} onOpenChange={onOpenChange}><AlertDialog.Portal><AlertDialog.Overlay className="dialog-overlay" /><AlertDialog.Content className="dialog"><ActionForm className="dialog-form" onSubmit={submit}><AlertDialog.Title>{title}</AlertDialog.Title><AlertDialog.Description>{description}</AlertDialog.Description><label className="field" htmlFor={inputID}><span className="field-label">{label}</span><input {...inputProps} id={inputID} value={value} onChange={event => onValueChange(event.target.value)} autoFocus required disabled={busy} /></label>{error && <div className="dialog-error" role="alert"><AlertTriangle size={17} /><span>{error}</span></div>}<div className="dialog-actions"><AlertDialog.Cancel asChild><Button type="button" disabled={busy}>Cancel</Button></AlertDialog.Cancel><Button type="submit" variant="primary" disabled={busy || !value.trim()}>{busy && <LoaderCircle className="spin" size={16} />}{submitLabel}</Button></div></ActionForm></AlertDialog.Content></AlertDialog.Portal></AlertDialog.Root>;
}

export function CopyButton({ value, label = 'Copy' }: { value: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => { if (!copied) return; const timer = setTimeout(() => setCopied(false), 1600); return () => clearTimeout(timer); }, [copied]);
  return <span className="copy-control"><Button type="button" size="sm" onClick={async () => { setError(''); try { await navigator.clipboard.writeText(value); setCopied(true); } catch { setCopied(false); setError('Could not copy. Select the text and copy it manually.'); } }}>{copied ? <Check size={15} /> : <Copy size={15} />}{copied ? 'Copied' : label}</Button>{error && <span className="field-error" role="alert">{error}</span>}</span>;
}

export function LogOutputHeader({ title, value, copyLabel }: { title: string; value: string; copyLabel: string }) {
  return <div className="log-output-heading"><h3>{title}</h3><CopyButton value={value} label={copyLabel} /></div>;
}

export function ProgressBar({ value, tone = 'accent' }: { value: number; tone?: 'accent' | 'danger' | 'warning' }) {
  const safe = Math.max(0, Math.min(100, value || 0));
  return <div className="progress" aria-label={`${safe.toFixed(0)} percent`}><span className={`progress-${tone}`} style={{ width: `${safe}%` }} /></div>;
}

export function Tabs({ items, active, onChange, children }: { children: ReactNode; items: Array<{ id: string; label: string }>; active: string; onChange: (id: string) => void }) {
  return <PrimitiveTabs value={active} onValueChange={onChange}><TabsList variant="line" className="tabs">{items.map(item => <TabsTrigger key={item.id} value={item.id} className={active === item.id ? 'active' : ''}>{item.label}</TabsTrigger>)}</TabsList><TabsContent value={active}>{children}</TabsContent></PrimitiveTabs>;
}
