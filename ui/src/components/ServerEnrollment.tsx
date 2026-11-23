import { type FormEvent, useEffect, useRef, useState } from 'react';
import { del, get, post } from '../lib/api';
import { goBack, indexHref, navigate, resourceHref } from '../lib/router';
import { useWorkspace } from '../state/WorkspaceContext';
import { Input } from './primitives/input';
import { useToast } from './ToastProvider';
import { ActionForm, Button, Card, CopyButton, ErrorBanner, Field, LogOutputHeader, Notice, SectionHeader, StatusBadge } from './ui';
import { Stack } from './layout';

/** A pending or finished connection of a new server (control/server-enrollments). */
export type Enrollment = { id: string; name: string; hostname: string; mode: 'ssh' | 'command'; state: 'waiting' | 'installing' | 'connecting' | 'connected' | 'failed'; message?: string; log?: string; host_key?: string; command?: string; expires?: string };

const stateLabel: Record<Enrollment['state'], string> = { waiting: 'Waiting', installing: 'Installing', connecting: 'Connecting', connected: 'Connected', failed: 'Failed' };
const stateStatus: Record<Enrollment['state'], string> = { waiting: 'waiting', installing: 'building', connecting: 'building', connected: 'healthy', failed: 'failed' };

/** The fields that name the new server; shared by both install modes. */
function IdentityFields() {
  return <><div className="form-grid two"><Field label="Server ID" hint="Permanent lowercase identifier."><Input name="id" required pattern="[a-z][a-z0-9-]{0,47}" placeholder="production-eu" spellCheck={false} /></Field><Field label="Display name"><Input name="name" required maxLength={100} placeholder="Production · Frankfurt" /></Field></div>
    <div className="form-grid two"><Field label="Hostname" hint="A DNS name for this server. Point it at the server; the HTTPS certificate is requested as soon as it resolves."><Input name="hostname" required placeholder="server1.example.com" spellCheck={false} /></Field><Field label="Contact email" hint="Optional. Given to the certificate authority for expiry notices."><Input name="email" type="email" placeholder="ops@example.com" autoComplete="email" /></Field></div></>;
}

/**
 * ServerEnrollment connects a server that has no agent yet. In SSH mode the
 * control panel logs in once and installs the agent itself; in command mode it
 * shows one command to run on the server. Either way the server installs the
 * release this panel can update, opens its web ports, and connects itself.
 */
export function ServerEnrollment({ mode }: { mode: 'ssh' | 'command' }) {
  const { refresh } = useWorkspace(); const notify = useToast();
  const [enrollment, setEnrollment] = useState<Enrollment>(); const [error, setError] = useState(''); const [busy, setBusy] = useState(false);
  const [auth, setAuth] = useState<'password' | 'key'>('password');
  const finished = useRef(false);
  const active = enrollment && enrollment.state !== 'connected' && enrollment.state !== 'failed';

  useEffect(() => {
    if (!enrollment || !active) return;
    let stopped = false;
    const timer = setInterval(() => {
      get<Enrollment>(`control/server-enrollments/${encodeURIComponent(enrollment.id)}`).then(next => { if (!stopped) setEnrollment(next); }).catch(() => { /* keeps the last state; the next poll retries */ });
    }, 2000);
    return () => { stopped = true; clearInterval(timer); };
  }, [enrollment?.id, active]);

  useEffect(() => {
    if (enrollment?.state !== 'connected' || finished.current) return;
    finished.current = true;
    notify('Server connected.');
    void refresh().then(() => navigate(resourceHref('servers', enrollment.id)));
  }, [enrollment?.state]);

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault(); setBusy(true); setError('');
    try {
      const values = Object.fromEntries(new FormData(event.currentTarget)) as Record<string, string>;
      const body: Record<string, unknown> = { id: values.id, name: values.name, hostname: values.hostname, email: values.email || '' };
      if (mode === 'ssh') body.ssh = { user: values.ssh_user || 'root', port: Number(values.ssh_port) || 22, address: values.ssh_address || '', host_key: values.ssh_host_key || '', ...(auth === 'password' ? { password: values.ssh_password } : { private_key: values.ssh_key, passphrase: values.ssh_passphrase || '' }) };
      finished.current = false;
      setEnrollment(await post<Enrollment>('control/server-enrollments', body));
    } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setBusy(false); }
  };
  const cancel = async () => { if (enrollment) await del(`control/server-enrollments/${encodeURIComponent(enrollment.id)}`).catch(() => undefined); setEnrollment(undefined); };

  if (enrollment) return <Stack gap="section">
    {enrollment.state === 'failed' && <ErrorBanner>{enrollment.message || 'The server could not be connected.'}</ErrorBanner>}
    <Card className="form-card narrow-form" data-enrollment={enrollment.state}><SectionHeader title={`Connecting ${enrollment.name}`} description={enrollment.hostname} actions={<StatusBadge status={stateStatus[enrollment.state]} />} />
      <Stack gap="md">
        <p role="status"><strong>{stateLabel[enrollment.state]}.</strong> {enrollment.state !== 'failed' && enrollment.message}</p>
        {enrollment.command && <><Field label="Run this on the new server" hint="Paste it into a terminal on that server. It works once and expires in an hour."><textarea readOnly rows={3} spellCheck={false} aria-label="Install command" value={enrollment.command} onFocus={event => event.currentTarget.select()} /></Field><CopyButton value={enrollment.command} label="Copy command" /></>}
        {enrollment.host_key && <p className="muted-copy">Server host key: <code>{enrollment.host_key}</code></p>}
        {enrollment.log && <><LogOutputHeader title="Installer output" value={enrollment.log} copyLabel="Copy output" /><pre className="console-output" role="region" aria-label="Installer output" tabIndex={0}>{enrollment.log}</pre></>}
        <div className="sticky-actions">{active ? <Button type="button" onClick={() => { void cancel(); }}>Cancel</Button> : enrollment.state === 'failed' && <Button type="button" variant="primary" onClick={() => setEnrollment(undefined)}>Start again</Button>}</div>
      </Stack>
    </Card>
  </Stack>;

  return <ActionForm id="server-enrollment-form" onSubmit={submit}><Stack gap="section">
    {error && <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner>}
    <Card className="form-card narrow-form"><SectionHeader title="New server" description={mode === 'ssh' ? 'The control panel logs in once, installs the hosting agent, opens the web ports and connects the server. Nothing is typed on the server.' : 'The control panel gives you one command to run on the server. It installs the hosting agent and connects the server by itself.'} /><IdentityFields /></Card>
    {mode === 'ssh' && <Card className="form-card narrow-form"><SectionHeader title="SSH login" description="Used for this one installation and never stored." />
      <div className="form-grid two"><Field label="SSH user" hint="root, or a user that may use sudo."><Input name="ssh_user" defaultValue="root" required spellCheck={false} /></Field><Field label="SSH port"><Input name="ssh_port" type="number" min={1} max={65535} defaultValue={22} required /></Field></div>
      <Field label="Sign in with"><select aria-label="Sign in with" value={auth} onChange={event => setAuth(event.target.value as 'password' | 'key')}><option value="password">Password</option><option value="key">Private key</option></select></Field>
      {auth === 'password' ? <Field label="SSH password"><Input name="ssh_password" type="password" required autoComplete="off" /></Field>
        : <><Field label="Private key" hint="The whole key, including its BEGIN and END lines."><textarea name="ssh_key" rows={6} required spellCheck={false} aria-label="Private key" /></Field><Field label="Key passphrase" hint="Only if the key has one."><Input name="ssh_passphrase" type="password" autoComplete="off" /></Field></>}
      <div className="form-grid two"><Field label="SSH address" hint="Optional. The server's IP address, if the hostname does not point at it yet."><Input name="ssh_address" placeholder="203.0.113.10" spellCheck={false} /></Field><Field label="Host key fingerprint" hint="Optional. If given, the server must present it."><Input name="ssh_host_key" placeholder="SHA256:…" spellCheck={false} /></Field></div>
    </Card>}
    {mode === 'command' && <Notice>The command needs root on the server and works once. The server downloads the agent release from {`this control panel's update source`}.</Notice>}
    <div className="sticky-actions"><Button type="button" onClick={() => { goBack(indexHref('servers')); }}>Cancel</Button><Button type="submit" variant="primary" disabled={busy}>{busy ? 'Starting…' : mode === 'ssh' ? 'Install and connect' : 'Create install command'}</Button></div>
  </Stack></ActionForm>;
}
