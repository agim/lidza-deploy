import { type ReactNode, useCallback } from 'react';
import { toast } from 'sonner';
import { Toaster } from './primitives/sonner';
import { ToastContext } from './toast-context';
export { useToast } from './toast-context';

export function ToastProvider({ children }: { children: ReactNode }) {
  const notify = useCallback((message: string, error = false) => {
    if (error) toast.error(message, { duration: Infinity });
    else toast.success(message, { duration: 6000 });
  }, []);
  return <ToastContext.Provider value={notify}>{children}<Toaster closeButton position="bottom-right" /></ToastContext.Provider>;
}
