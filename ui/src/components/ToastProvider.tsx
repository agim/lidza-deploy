import { type ReactNode, useCallback, useMemo, useState } from 'react';
import { CheckCircle2, X } from 'lucide-react';
import { IconButton } from './ui';
import { ToastContext } from './toast-context';
export { useToast } from './toast-context';

type Toast = { id: number; message: string; error?: boolean };

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const notify = useCallback((message: string, error = false) => {
    const id = Date.now() + Math.random();
    setToasts(current => [...current, { id, message, error }]);
    if (!error) setTimeout(() => setToasts(current => current.filter(item => item.id !== id)), 6000);
  }, []);
  const value = useMemo(() => notify, [notify]);
  return <ToastContext.Provider value={value}>{children}<div className="toast-region" aria-live="polite">{toasts.map(toast => <div className={toast.error ? 'toast toast-error' : 'toast'} key={toast.id}>{toast.error ? null : <CheckCircle2 size={18} />}<span>{toast.message}</span><IconButton label="Dismiss" onClick={() => setToasts(current => current.filter(item => item.id !== toast.id))}><X size={16} /></IconButton></div>)}</div></ToastContext.Provider>;
}
