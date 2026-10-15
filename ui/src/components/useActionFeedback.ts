import { useEffect, useRef, useState } from 'react';
import { useToast } from './toast-context';

export const ACTION_FEEDBACK_MS = 1950;

// Only the indicator is held open. Requests start immediately and their results
// render immediately; fast responses still receive perceptible acknowledgement.
export function useActionFeedback() {
  const [pending, setPending] = useState(false);
  const active = useRef(false);
  const mounted = useRef(true);
  const notify = useToast();
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const run = async (operation: () => unknown): Promise<boolean> => {
    if (active.current) return false;
    active.current = true;
    const started = Date.now();
    setPending(true);
    let succeeded = true;
    try { await operation(); }
    catch (reason) {
      succeeded = false;
      if (mounted.current) notify(reason instanceof Error ? reason.message : String(reason), true);
    } finally {
      const remaining = ACTION_FEEDBACK_MS - (Date.now() - started);
      if (remaining > 0) await new Promise(resolve => setTimeout(resolve, remaining));
      active.current = false;
      if (mounted.current) setPending(false);
    }
    return succeeded;
  };
  return { pending, active, run };
}
