import { type FormEvent, useEffect, useRef, useState } from 'react';
import { KeyRound, Play } from 'lucide-react';
import { Input } from '../components/primitives/input';
import { ActionForm, Button, Card, CopyButton, ErrorBanner, Field, Loading, PageHeader, SectionHeader } from '../components/ui';
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
 const [output, setOutput] = useState('Locked.');
 const [jobID, setJobID] = useState(() => savedJob(serverID));
 const [submitting, setSubmitting] = useState(false);
 const submitLock = useRef(false);
 const [progress, setProgress] = useState('');
 const [error, setError] = useState('');
 const notify = useToast();
 const busy = submitting || Boolean(jobID);
 const endpoint = `control/servers/${encodeURIComponent(serverID)}/console`;
 useEffect(() => {
  setStatus(undefined); setError(''); setOutput('Locked.'); setJobID(savedJob(serverID));
  get<Access>(endpoint).then(setStatus).catch(reason => setError(message(reason)));
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
    setProgress(finished ? (job.error || 'Command finished.') : `Running · ${Math.floor(job.duration_seconds)} seconds`);
    if (finished) { if(job.error) setError(job.error); setJobID(''); return; }
    if (offset < job.output_size) delay = 50;
   } catch (reason) {
    if (cancelled) return;
    if (reason instanceof APIError && reason.status === 404) {
     setError(message(reason)); setProgress('Result unavailable. Check the server before rerunning.'); saveJob(serverID, ''); setJobID(''); return;
    }
    if (reason instanceof APIError && reason.status === 403) {
     setStatus(current => current && ({ ...current, unlocked: false }));
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
 const unlock = async (event: FormEvent<HTMLFormElement>) => {
  event.preventDefault(); const form = event.currentTarget;
  try {
   const result = await post<{ expiresAt: string }>('v1/auth/step-up', { code: String(new FormData(form).get('code') || ''), purpose: 'server.console', target: serverID });
   setStatus(current => ({ ...current, two_factor_enabled: true, unlocked: true })); setError(''); form.reset();
   notify(`Root console unlocked until ${new Date(result.expiresAt).toLocaleTimeString()}.`);
  } catch (reason) { setError(message(reason)); }
 };
 const run = async (event: FormEvent<HTMLFormElement>) => {
  event.preventDefault(); if (submitLock.current || busy || !status?.unlocked) return;
  submitLock.current = true; setSubmitting(true); setError('');
  const submitted = command, id = crypto.randomUUID();
  setCommand(''); setOutput(''); setProgress('Starting command…'); saveJob(serverID, id);
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
  return <><PageHeader eyebrow="Protected operation" title={`${server?.name || serverID} root console`} description="Just-in-time root access is scoped to this server and recorded in the audit log." back="#servers" />{error && <ErrorBanner>{error}</ErrorBanner>}<div className="console-grid"><Card className="form-card"><SectionHeader title="Security unlock" description={status?.two_factor_enabled ? 'Verify to unlock this server for 15 minutes. Verify again to extend access.' : 'Enable two-factor authentication before using the root console.'} />{status?.two_factor_enabled ? <ActionForm id="console-unlock" onSubmit={unlock}><Field label="Authenticator or recovery code"><Input name="code" inputMode="numeric" autoComplete="one-time-code" required /></Field><Button type="submit"><KeyRound size={16} />{status.unlocked ? 'Extend access' : 'Unlock for 15 minutes'}</Button></ActionForm> : <Button onClick={() => { location.hash = 'page/two-factor/account'; }}>Set up two-factor authentication</Button>}</Card><Card className="form-card"><SectionHeader title="Run command" description={`Maximum runtime: ${Math.round((status?.max_runtime_seconds ?? 600) / 60)} minutes. Output updates live and reconnects after connection loss. Output is capped at ${formatBytes(status?.max_output_bytes ?? 1048576)}.`} /><ActionForm id="console-command" onSubmit={run}><Field label="Command"><textarea rows={10} required spellCheck={false} value={command} onChange={event => setCommand(event.target.value)} placeholder={'docker ps\n\ndocker exec -i lidza-db-app sh -c \'exec psql …\''} /></Field><Button variant="primary" type="submit" disabled={!status?.unlocked || busy}><Play size={16} />{busy ? 'Running…' : 'Run command'}</Button></ActionForm><div className="console-output-heading"><h3>Output</h3><CopyButton value={output} label="Copy output" /></div><p role="status" className="muted-copy">{progress}</p><pre className="console-output">{output}</pre></Card></div></>;
}
