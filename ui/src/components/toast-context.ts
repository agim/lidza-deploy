import { createContext, useContext } from 'react';

export const ToastContext = createContext<(message: string, error?: boolean) => void>(() => undefined);
export function useToast() { return useContext(ToastContext); }
