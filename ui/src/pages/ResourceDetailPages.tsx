import { Input } from '../components/primitives/input';
import { Database, HardDrive, Save, UploadCloud } from 'lucide-react';
import { type FormEvent, useEffect, useState } from 'react';
import { get, post } from '../lib/api';
import { useToast } from '../components/ToastProvider';
import { type AppMoveLock, MoveLockFieldset } from '../components/MoveLock';
import { SummaryList } from '../components/SummaryList';
import { ActionForm, Button, Card, ErrorBanner, Field, Loading, SectionHeader } from '../components/ui';
import { Cluster } from '../components/layout';

/** CacheSettings: an application's cache, automatic private Valkey or an external Redis-compatible service (Database & cache tab). */
export function CacheSettings({ appID, disabled, lock }: { appID: string; disabled?: boolean; lock: AppMoveLock }) {
  const [data, setData] = useState<{ mode?: string; configured?: boolean }>(), [mode, setMode] = useState('automatic'), [error, setError] = useState(''); const notify = useToast();
  useEffect(() => { get<{ mode?: string; configured?: boolean }>(`control/apps/${encodeURIComponent(appID)}/cache`).then(value => { setData(value); setMode(value.mode === 'external' ? 'external' : 'automatic'); }).catch(reason => setError(reason instanceof Error ? reason.message : String(reason))); }, [appID]);
  const submit = async (event: FormEvent<HTMLFormElement>) => { if (lock.locked) return; const values = Object.fromEntries(new FormData(event.currentTarget)); try { await post(`control/apps/${encodeURIComponent(appID)}/cache`, { mode, ...(mode === 'external' ? { url: values.url } : {}) }); notify('Cache configuration saved.'); setError(''); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } };
  if (!data && !error) return <Loading label="Loading cache settings…" />;
  return <Card className="form-card" data-cache-settings={appID}><SectionHeader title="Cache" description="Choose automatic private Valkey or an existing Redis-compatible service." />{error && <ErrorBanner>{error}</ErrorBanner>}
    {data && <ActionForm id="cache-form" onSubmit={submit}><MoveLockFieldset lock={lock}><fieldset disabled={disabled}><div className="choice-grid"><label className={mode === 'automatic' ? 'selected' : ''}><Input type="radio" value="automatic" checked={mode === 'automatic'} onChange={() => setMode('automatic')} /><HardDrive size={21} /><span><strong>Automatic private cache</strong><small>Provision authenticated Valkey on the app’s server and retain its volume.</small></span></label><label className={mode === 'external' ? 'selected' : ''}><Input type="radio" value="external" checked={mode === 'external'} onChange={() => setMode('external')} /><Database size={21} /><span><strong>External Redis / Valkey</strong><small>Use an existing TLS-protected service reachable by the app server.</small></span></label></div>{mode === 'external' && <Field label="CACHE_URL" hint={data.configured ? 'A credential is already saved. Leave blank only if the API permits keeping it.' : 'This connection string is encrypted.'}><Input name="url" type="password" autoComplete="new-password" placeholder={data.configured ? 'Saved credential' : 'rediss://…'} required={!data.configured} /></Field>}</fieldset></MoveLockFieldset><Cluster><Button type="submit" variant="primary" disabled={disabled || lock.locked} title={lock.reason || undefined}><Save size={16} />Save cache settings</Button></Cluster></ActionForm>}
  </Card>;
}

/** ServerSSHAccess: the operator keys of a server (SSH access tab). */
export function ServerSSHAccess({ serverID }: { serverID: string }) {
  const [data, setData] = useState<{ keys?: Array<{ id?: string; name?: string; fingerprint?: string; public_key?: string }> }>(), [error, setError] = useState('');
  useEffect(() => { get<typeof data>(`control/servers/${encodeURIComponent(serverID)}/ssh-access`).then(setData).catch(reason => setError(reason instanceof Error ? reason.message : String(reason))); }, [serverID]);
  if (!data && !error) return <Loading label="Loading SSH access…" />;
  return <Card className="form-card">{error && <ErrorBanner>{error}</ErrorBanner>}<SectionHeader title="Authorized operator keys" description="Temporary, auditable operator keys for this server. Access is disabled unless explicitly enabled; remove keys right after use." /><div className="simple-list">{(data?.keys || []).map((key, index) => <div key={key.id || index}><div><strong>{key.name || `Key ${index + 1}`}</strong><code>{key.fingerprint || key.public_key}</code></div></div>)}{!data?.keys?.length && <p className="muted-copy">No operator keys are authorized.</p>}</div></Card>;
}

/** ServerAgentUpgrade: the installed agent release and a safe upgrade (Agent tab). */
export function ServerAgentUpgrade({ serverID }: { serverID: string }) {
  const [data, setData] = useState<{ current?: string; latest?: string; channel?: string; state?: string; message?: string; supported?: boolean }>(), [error, setError] = useState(''); const notify = useToast();
  useEffect(() => { get<typeof data>(`control/servers/${encodeURIComponent(serverID)}/upgrade`).then(setData).catch(reason => setError(reason instanceof Error ? reason.message : String(reason))); }, [serverID]);
  if (!data && !error) return <Loading label="Loading the agent release…" />;
  const latest = data?.latest || (data?.channel === 'private' ? 'none newer' : 'unknown');
  return <Card className="form-card" data-agent-upgrade={serverID}><SectionHeader title="Agent release" description="Checksums and startup are verified. A failed startup restores the previous binaries; running applications keep serving." />{error && <ErrorBanner>{error}</ErrorBanner>}
    <SummaryList aria-label="Upgrade state" items={[{ label: 'Installed', value: data?.current || 'unknown' }, { label: data?.channel === 'private' ? 'Latest private release' : 'Latest stable', value: latest }, { label: 'State', value: <strong>{data?.state || 'unknown'}</strong> }, data?.message && { label: 'Message', value: data.message }]} />
    <Cluster><Button variant="primary" disabled={!data?.supported || !data?.latest} onClick={async () => { try { await post(`control/servers/${encodeURIComponent(serverID)}/upgrade`, { version: data!.latest }); notify(`Agent upgrade to ${data!.latest} queued.`); setError(''); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } }}><UploadCloud size={16} />Upgrade agent</Button></Cluster>
  </Card>;
}
