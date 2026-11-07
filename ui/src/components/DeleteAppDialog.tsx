import * as AlertDialog from '@radix-ui/react-alert-dialog';
import { type FormEvent, type ReactNode, useEffect, useId, useState } from 'react';
import { AlertTriangle, LoaderCircle } from 'lucide-react';
import { del } from '../lib/api';
import { deletionInventory, formatDay, restorableUntil } from '../lib/archive';
import { ActionForm, Button } from './ui';

/**
 * DeleteAppAction opens the deletion dialog of an application: it says
 * exactly what is kept for the restore window and what is deleted now,
 * shows the last day it can be restored, and needs the application id typed.
 */
export function DeleteAppAction({ app, children, onDeleted, now }: { app: { id: string; native?: unknown; runtime?: string; runtime_profile?: string; management_capabilities?: { adopted?: boolean } }; children: ReactNode; onDeleted: (archiveID?: string) => void | Promise<void>; now?: Date }) {
  const inputID = useId();
  const reasonID = useId();
  const [open, setOpen] = useState(false);
  const [confirm, setConfirm] = useState('');
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => { if (!open) { setConfirm(''); setReason(''); setError(''); setBusy(false); } }, [open]);
  const { kept, deleted } = deletionInventory(app);
  const until = formatDay(restorableUntil(now));
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (confirm.trim() !== app.id || busy) return;
    setBusy(true); setError('');
    try {
      const demo = new URLSearchParams(location.search).get('demo') === '1';
      const archive = demo ? undefined : await del<{ id?: string }>(`control/apps/${encodeURIComponent(app.id)}`, { confirm: app.id, reason: reason.trim() });
      setOpen(false);
      await onDeleted(archive?.id);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally { setBusy(false); }
  };
  return <AlertDialog.Root open={open} onOpenChange={value => { if (!busy) setOpen(value); }}>
    <AlertDialog.Trigger asChild>{children}</AlertDialog.Trigger>
    <AlertDialog.Portal><AlertDialog.Overlay className="dialog-overlay" /><AlertDialog.Content className="dialog dialog-wide" data-delete-app={app.id}>
      <ActionForm className="dialog-form" onSubmit={submit}>
        <AlertDialog.Title>Delete {app.id}?</AlertDialog.Title>
        <AlertDialog.Description>It stops serving at once and its hostnames become free for other applications. You can restore it from <strong>Deleted apps</strong> until <strong data-restorable-until>{until}</strong>; after that it is deleted permanently, including its databases.</AlertDialog.Description>
        <div className="delete-inventory">
          <section><h3>Kept until {until}</h3><ul>{kept.map(item => <li key={item}>{item}</li>)}</ul></section>
          <section><h3>Deleted now, cannot be undone</h3><ul>{deleted.map(item => <li key={item}>{item}</li>)}</ul></section>
        </div>
        <label className="field" htmlFor={reasonID}><span className="field-label">Reason (optional)</span><input id={reasonID} maxLength={1000} value={reason} onChange={event => setReason(event.target.value)} disabled={busy} /></label>
        <label className="field" htmlFor={inputID}><span className="field-label">Type <code>{app.id}</code> to delete it</span><input id={inputID} autoComplete="off" spellCheck={false} value={confirm} onChange={event => setConfirm(event.target.value)} disabled={busy} required /></label>
        {error && <div className="dialog-error" role="alert"><AlertTriangle size={17} /><span>{error}</span></div>}
        <div className="dialog-actions"><AlertDialog.Cancel asChild><Button type="button" disabled={busy}>Cancel</Button></AlertDialog.Cancel><Button type="submit" variant="danger" disabled={busy || confirm.trim() !== app.id}>{busy && <LoaderCircle className="spin" size={16} />}Delete application</Button></div>
      </ActionForm>
    </AlertDialog.Content></AlertDialog.Portal>
  </AlertDialog.Root>;
}
