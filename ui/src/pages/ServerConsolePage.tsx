import { type FormEvent, useCallback, useEffect, useRef, useState } from 'react';
import { KeyRound, Play, Square } from 'lucide-react';
import { Input } from '../components/primitives/input';
import { ActionForm, Button, Card, CopyButton, ErrorBanner, Field, Loading, PageHeader, PromptDialog, SectionHeader } from '../components/ui';
import { APIError, api, get, post } from '../lib/api';
import { formatBytes } from '../lib/format';
import { useWorkspace } from '../state/WorkspaceContext';
import { useToast } from '../components/ToastProvider';

type Access = { two_factor_enabled: boolean; unlocked: boolean; max_runtime_seconds?: number; max_output_bytes?: number };
type Job = { id: string; state: string; output: string; next_offset: number; output_size: number; truncated: boolean; exit_code: number; duration_seconds: number; error?: string };
const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);
const storageKey = (serverID: string) => `lidza-console-job:${serverID}`;
const savedJob = (serverID: string) => { try { return sessionStorage.getItem(storageKey(serverID)) || ''; } catch { return ''; } };
const saveJob = (serverID: string, id: string) => { try { id ? sessionStorage.setItem(storageKey(serverID), id) : sessionStorage.removeItem(storageKey(serverID)); } catch { /* Console still works without browser storage. */ } };

export function ServerConsolePage({ serverID }: { serverID: string }) {
 const { servers } = useWorkspace();
 const server = servers.find(item => item.id === serverID);
 const [status, setStatus] = useState<Access>();
 const [command, setCommand] = useState('');
 const [output, setOutput] = useState('No command output loaded.');
 const [jobID, setJobID] = useState(() => savedJob(serverID));
 const [submitting, setSubmitting] = useState(false);
 const [cancelTarget, setCancelTarget] = useState('');
 const [verificationCode, setVerificationCode] = useState('');
 const cancelRequested = useRef('');
 const [cancelling, setCancelling] = useState(false);
 const cancelLock = useRef('');
 const [cancelError,setCancelError] = useState('');
 const submitLock = useRef(false);
 const [progress, setProgress] = useState('');
 const [error, setError] = useState('');
 const notify = useToast();
 const busy = submitting || Boolean(jobID);
 const endpoint = `control/servers/${encodeURIComponent(serverID)}/console`;
 useEffect(() => {
  setStatus(undefined); setError(''); setOutput('No command output loaded.'); setJobID(savedJob(serverID));
  get<Access>(endpoint).then(value => { setStatus(value); if(savedJob(serverID)) { setProgress(value.unlocked ? 'Checking saved command…' : 'Saved command found. Unlock to check its status or cancel it.'); } }).catch(reason => setError(message(reason)));
 }, [endpoint, serverID]);
 useEffect(() => {
  if (!jobID || !status?.unlocked) return;
  let cancelled = false, offset = 0, text = '', failures = 0;
  let timer: ReturnType<typeof setTimeout>;
  const poll = async () => {
   let delay = 1000;
   try {
    const job = await api<Job>(`${endpoint}/jobs/${encodeURIComponent(jobID)}?offset=${offset}`, { signal: AbortSignal.timeout(15000) });
    if (cancelled) return;
    failures = 0; setError('');
    text += job.output; offset = job.next_offset;
    const finished = job.state !== 'running' && offset >= job.output_size;
    setOutput(text + (finished ? `${job.truncated ? '\n[output truncated at 8 MiB]' : ''}\n\n[exit ${job.exit_code} · ${job.duration_seconds.toFixed(2)}s]` : ''));
    setProgress(finished ? (job.state === 'cancelled' ? 'Command cancelled.' : job.error || 'Command finished.') : cancelRequested.current ? 'Stopping command…' : `Running · ${Math.floor(job.duration_seconds)} seconds`);
    if (finished) { cancelRequested.current=''; setCancelTarget(''); setCancelError(''); setCancelling(false); cancelLock.current=''; if(job.error) setError(job.error); setJobID(''); return; }
    if (offset < job.output_size) delay = 50;
   } catch (reason) {
    if (cancelled) return;
    if (reason instanceof APIError && reason.status === 404) {
     cancelRequested.current=''; setCancelling(false); cancelLock.current=''; setError(message(reason)); setProgress('Result unavailable. Check the server before rerunning.'); saveJob(serverID, ''); setJobID(''); return;
    }
    if (reason instanceof APIError && reason.status === 403) {
     setProgress('Unlock to check command status or cancel it.'); setStatus(current => current && ({ ...current, unlocked: false }));
     setError('Verify again to reconnect to this command.'); return;
    }
    setProgress('Connection interrupted; reconnecting to the same command…');
    delay = Math.min(10000, 1000 * ++failures);
   }
   if (!cancelled) timer = setTimeout(poll, delay);
  };
  void poll();
  return () => { cancelled = true; clearTimeout(timer); };
 }, [endpoint, jobID, serverID, status?.unlocked]);
 const requestCancellation = useCallback(async (target: string) => {
  if (!target || cancelLock.current) return;
  cancelLock.current=target; cancelRequested.current=target; setCancelling(true); setError(''); setCancelError(''); setProgress('Stopping command…');
  try {
   await api(`${endpoint}/jobs/${encodeURIComponent(target)}/cancel`, { method:'POST', body:'{}', signal:AbortSignal.timeout(15000) });
  } catch(reason) {
   if(cancelLock.current!==target) return;
   cancelRequested.current='';
   if(reason instanceof APIError && reason.status===403) {
    setStatus(current=>current && ({...current, unlocked:false})); setCancelTarget(target);
    setProgress('Verify to cancel this command.');
   } else setCancelError(`Cancellation was not confirmed: ${message(reason)}. You can retry Cancel.`);
  } finally {
   if(cancelLock.current===target) { cancelLock.current=''; setCancelling(false); }
  }
 }, [endpoint]);
 const cancelCommand = useCallback(() => {
  if (!jobID) return;
  if (status?.two_factor_enabled===false) { location.hash='page/two-factor/account'; return; }
  if (!status?.unlocked) { setCancelTarget(jobID); return; }
  void requestCancellation(jobID);
 }, [jobID,status?.two_factor_enabled,status?.unlocked,requestCancellation]);
 const unlockToCancel = async (code: string) => {
  const target=cancelTarget;
  await post('v1/auth/step-up',{code,purpose:'server.console',target:serverID});
  setStatus(current=>({...current,two_factor_enabled:true,unlocked:true})); setError(''); setCancelError('');
  await requestCancellation(target);
 };
 useEffect(() => {
  const interrupt = (event: KeyboardEvent) => {
   if (!event.ctrlKey || event.metaKey || event.altKey || event.shiftKey || event.key.toLowerCase()!=='c' || event.repeat || !jobID || cancelTarget) return;
   // Preserve normal copying when text is selected in either output or a field.
   if (window.getSelection()?.toString()) return;
   const field = document.activeElement;
   if ((field instanceof HTMLInputElement || field instanceof HTMLTextAreaElement) && field.selectionStart!==field.selectionEnd) return;
   event.preventDefault(); void cancelCommand();
  };
  document.addEventListener('keydown', interrupt);
  return () => document.removeEventListener('keydown', interrupt);
 }, [cancelCommand,jobID,cancelTarget]);
 const unlock = async (event: FormEvent<HTMLFormElement>) => {
  event.preventDefault(); const form = event.currentTarget;
  try {
   const result = await post<{ expiresAt: string }>('v1/auth/step-up', { code: String(new FormData(form).get('code') || ''), purpose: 'server.console', target: serverID });
   setStatus(current => ({ ...current, two_factor_enabled: true, unlocked: true })); setError(''); setCancelError(''); form.reset();
   notify(`Root console unlocked until ${new Date(result.expiresAt).toLocaleTimeString()}.`);
  } catch (reason) { setError(message(reason)); }
 };
 const run = async (event: FormEvent<HTMLFormElement>) => {
  event.preventDefault(); if (submitLock.current || busy || !status?.unlocked) return;
  submitLock.current = true; setSubmitting(true); setError(''); setCancelError('');
  const submitted = command, id = crypto.randomUUID();
  cancelRequested.current=''; setCommand(''); setOutput(''); setCancelling(false); cancelLock.current=''; setProgress('Starting command…'); saveJob(serverID, id);
  try {
   await api<Job>(`${endpoint}/jobs`, { method: 'POST', body: JSON.stringify({ id, command: submitted }), signal: AbortSignal.timeout(15000) }); setJobID(id);
  } catch (reason) {
   if (reason instanceof APIError && [400, 403, 404, 409].includes(reason.status)) {
    saveJob(serverID, ''); setCommand(submitted); setProgress('Command was not started.'); setError(message(reason));
   } else {
    setProgress('Acknowledgement lost; checking the same command…'); setJobID(id);
   }
  } finally { setSubmitting(false); submitLock.current = false; }
 };
 if (!status && !error) return <Loading overlay label="Checking console access…" />;
  return <><PageHeader eyebrow="Protected operation" title={`${server?.name || serverID} root console`} description="Just-in-time root access is scoped to this server and recorded in the audit log." back="#servers" />{(error || cancelError) && <ErrorBanner>{error || cancelError}</ErrorBanner>}<div className="console-grid"><Card className="form-card"><SectionHeader title="Security unlock" description={!status ? 'Verify your identity to restore console access.' : status.two_factor_enabled ? 'Verify to unlock this server for 15 minutes. Verify again to extend access.' : 'Enable two-factor authentication before using the root console.'} />{status?.two_factor_enabled !== false ? <ActionForm id="console-unlock" onSubmit={unlock}><Field label="Authenticator or recovery code"><Input name="code" inputMode="numeric" autoComplete="one-time-code" required /></Field><Button type="submit"><KeyRound size={16} />{status?.unlocked ? 'Extend access' : 'Unlock for 15 minutes'}</Button></ActionForm> : <Button onClick={() => { location.hash = 'page/two-factor/account'; }}>Set up two-factor authentication</Button>}</Card><Card className="form-card"><SectionHeader title="Run command" description={`Maximum runtime: ${Math.round((status?.max_runtime_seconds ?? 600) / 60)} minutes. Output updates live and reconnects after connection loss. Output is capped at ${formatBytes(status?.max_output_bytes ?? 1048576)}.`} /><ActionForm id="console-command" onSubmit={run}><Field label="Command"><textarea rows={10} required spellCheck={false} value={command} onChange={event => setCommand(event.target.value)} placeholder={'docker ps\n\ndocker exec -i lidza-db-app sh -c \'exec psql …\''} /></Field>{!jobID && <Button variant="primary" type="submit" disabled={!status?.unlocked || busy}><Play size={16} />{submitting ? 'Starting…' : 'Run command'}</Button>}{jobID && <Button type="button" variant="secondary" onClick={cancelCommand} disabled={cancelling} pending={cancelling}><Square size={16} />{cancelling ? 'Sending interrupt…' : status?.two_factor_enabled===false ? 'Set up MFA to cancel' : !status?.unlocked ? 'Unlock to cancel' : cancelRequested.current ? 'Retry cancel (Ctrl+C)' : 'Cancel command (Ctrl+C)'}</Button>}</ActionForm><div className="console-output-heading"><h3>Output</h3><CopyButton value={output} label="Copy output" /></div><p role="status" className="muted-copy">{progress}</p><pre className="console-output">{output}</pre></Card></div><PromptDialog open={Boolean(cancelTarget)} title="Unlock to cancel command" description="Verify your authenticator or recovery code to interrupt this saved command. Its current status will also be checked." label="Authenticator or recovery code" value={verificationCode} submitLabel="Verify & cancel command" inputProps={{type:'password',autoComplete:'one-time-code',spellCheck:false}} onValueChange={setVerificationCode} onOpenChange={open=>{if(!open){setCancelTarget('');setVerificationCode('');}}} onSubmit={unlockToCancel} /></>;
}
