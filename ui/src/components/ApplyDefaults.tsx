import * as AlertDialog from '@radix-ui/react-alert-dialog';
import { AlertTriangle, Layers, LoaderCircle } from 'lucide-react';
import { type FormEvent, useState } from 'react';
import { get, post } from '../lib/api';
import { type DefaultsResponse, type DefaultsRuntime, defaultsRuntimeLabels, isDefaultsRuntime, runtimeProfile } from '../lib/defaults';
import { ActionForm, Button } from './ui';
import { useToast } from './ToastProvider';
import type { SettingsApplyResult } from '../lib/settingsApply';

// Reviews the application defaults of the application's runtime and copies the
// selected ones. The revision binds the request to the set shown here.
export function ApplyDefaultsButton({ appID, runtime, presentKeys, disabled, dirty, onApplied }: { appID: string; runtime?: string; presentKeys: string[]; disabled?: boolean; dirty?: boolean; onApplied?: (apply?: SettingsApplyResult) => void }) {
  const kind: DefaultsRuntime = isDefaultsRuntime(runtime) ? runtime : 'go';
  const label = defaultsRuntimeLabels[kind];
  const [open, setOpen] = useState(false);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [values, setValues] = useState<Record<string, string>>({});
  const [revision, setRevision] = useState('');
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [replace, setReplace] = useState(false);
  const notify = useToast();
  const present = new Set(presentKeys);
  const entries = Object.entries(values).filter(([, value]) => value !== '').sort(([a], [b]) => a.localeCompare(b));
  const load = async () => {
    setOpen(true); setLoading(true); setError(''); setReplace(false); setValues({}); setSelected(new Set());
    try {
      const profile = runtimeProfile(await get<DefaultsResponse>('control/application-defaults'), kind);
      const next = profile.values || {};
      setValues(next); setRevision(profile.revision || '');
      setSelected(new Set(Object.keys(next).filter(key => next[key] !== '' && !present.has(key))));
    } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setLoading(false); }
  };
  const toggle = (key: string, on: boolean) => setSelected(current => { const next = new Set(current); if (on) next.add(key); else next.delete(key); return next; });
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault(); if (busy) return;
    const keys = entries.map(([key]) => key).filter(key => selected.has(key));
    if (!keys.length) { setError('Select at least one default.'); return; }
    setBusy(true); setError('');
    try {
      const saved = await post<{ apply?: SettingsApplyResult } | undefined>(`control/apps/${encodeURIComponent(appID)}/apply-defaults`, { keys, replace, revision });
      setOpen(false);
      // The settings page reports what the save applied (a restart or deployment).
      if (onApplied) onApplied(saved?.apply ? { ...saved.apply, message: saved.apply.message.replace(/^Settings saved\./, `${label} defaults saved.`) } : undefined);
      else notify(`${label} defaults saved.`);
    } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setBusy(false); }
  };
  return <AlertDialog.Root open={open} onOpenChange={value => { if (!busy) setOpen(value); }}>
    <Button type="button" disabled={disabled || dirty} title={dirty ? 'Save or discard environment changes before applying application defaults.' : undefined} onClick={load}><Layers size={16} />Apply application defaults</Button>
    <AlertDialog.Portal><AlertDialog.Overlay className="dialog-overlay" /><AlertDialog.Content className="dialog apply-defaults-dialog"><ActionForm className="dialog-form" onSubmit={submit}>
      <AlertDialog.Title>Apply {label} defaults</AlertDialog.Title>
      <AlertDialog.Description>Choose the {label} application defaults to copy into {appID}. Variables that are already set are kept unless you choose to replace them.</AlertDialog.Description>
      {loading ? <p className="muted-copy"><LoaderCircle className="spin" size={16} /> Loading application defaults…</p> : <div className="apply-defaults-list">{entries.map(([key, value]) => <label className="check-row" key={key}><input type="checkbox" checked={selected.has(key)} onChange={event => toggle(key, event.target.checked)} aria-label={key} /><span><strong><code>{key}</code> = {value}</strong><small>{present.has(key) ? 'Already set · kept unless replacement is selected' : 'Not set'}</small></span></label>)}{!entries.length && !error && <p className="muted-copy">No {label} defaults are configured. Add them on the Application defaults page.</p>}</div>}
      {entries.length > 0 && <label className="check-row"><input type="checkbox" checked={replace} onChange={event => setReplace(event.target.checked)} /><span><strong>Replace selected variables that are already set</strong><small>Leave unchecked to keep every existing value, including empty ones.</small></span></label>}
      {error && <div className="dialog-error" role="alert"><AlertTriangle size={17} /><span>{error}</span></div>}
      <div className="dialog-actions"><AlertDialog.Cancel asChild><Button type="button" disabled={busy}>Cancel</Button></AlertDialog.Cancel><Button type="submit" variant="primary" disabled={busy || loading || !entries.length || !revision}>{busy && <LoaderCircle className="spin" size={16} />}Apply selected defaults</Button></div>
    </ActionForm></AlertDialog.Content></AlertDialog.Portal>
  </AlertDialog.Root>;
}
