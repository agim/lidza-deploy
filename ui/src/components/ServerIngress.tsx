import { type FormEvent, useEffect, useState } from 'react';
import { FileCheck2, RefreshCw, Save, Undo2 } from 'lucide-react';
import { get, post, put } from '../lib/api';
import type { HostErrorPages, HostIngressStatus } from '../lib/types';
import { useToast } from './ToastProvider';
import { Badge, Button, Card, ErrorBanner, Field, Loading, Notice, SectionHeader } from './ui';
import { Input } from './primitives/input';

const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);
const hostOf = (admin?: string) => (admin || '').replace(/^https?:\/\//, '').replace(/\/$/, '');

// ingressLabel names the host's web ingress: an admin-API Caddy that created
// applications are published to, or the agent's own proxy.
export function ingressLabel(status: HostIngressStatus) {
  return status.mode === 'caddy' ? `Caddy (admin API on ${hostOf(status.caddy_admin) || '127.0.0.1:2019'})` : 'Līdza agent proxy';
}

const sourceLabel: Record<HostIngressStatus['source'], string> = { detected: 'Detected', setting: 'Set here', config: 'Agent configuration file' };

/** ErrorPagesFacts shows the managed error pages of a Caddy-fronted host. */
function ErrorPagesFacts({ pages, busy, onAdopt }: { pages: HostErrorPages; busy: boolean; onAdopt: () => void }) {
  return <div className="error-pages" data-error-pages>
    <dl className="move-facts">
      <dt>Error pages</dt><dd>{pages.ready ? <>Served from <code>{pages.dir}</code>{pages.caddy_user ? ` · readable by ${pages.caddy_user}` : ''}</> : pages.problem || 'Not written yet'}</dd>
      <dt>Error routes</dt><dd>{pages.managed} use the managed pages{pages.unmanaged_hosts ? ` · ${pages.unmanaged_hosts} hostname${pages.unmanaged_hosts === 1 ? '' : 's'} elsewhere` : ''}</dd>
    </dl>
    {pages.ready && pages.problem && <Notice>{pages.problem}</Notice>}
    {Boolean(pages.unmanaged?.length) && <div className="error-pages-unmanaged">
      <Notice>{pages.detail ? pages.detail.charAt(0).toUpperCase() + pages.detail.slice(1) + '.' : 'Some error pages are served from an unmanaged directory.'}</Notice>
      <ul className="muted-copy">{pages.unmanaged?.map(root => <li key={`${root.server}/${root.root}`}><code>{root.root}</code>{root.exists ? '' : ' (missing)'} · {root.hosts.length ? root.hosts.join(', ') : 'every hostname'}</li>)}</ul>
      <Button size="sm" variant="primary" disabled={busy || !pages.ready} onClick={onAdopt}><FileCheck2 size={15} />Use Līdza error pages</Button>
    </div>}
  </div>;
}

export function ServerIngress({ serverID }: { serverID: string }) {
  const notify = useToast();
  const base = `control/servers/${encodeURIComponent(serverID)}/host/ingress`;
  const [status, setStatus] = useState<HostIngressStatus>();
  const [admin, setAdmin] = useState('');
  const [server, setServer] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);

  const adopt = (value: HostIngressStatus) => { setStatus(value); setAdmin(value.override?.admin || ''); setServer(value.override?.server || ''); };
  const load = async (refresh = false) => { try { adopt(await get<HostIngressStatus>(refresh ? `${base}?refresh=1` : base)); setError(''); } catch (reason) { setError(message(reason)); } };
  useEffect(() => { setStatus(undefined); void load(); }, [serverID]);

  const save = async (caddy: { admin: string; server?: string } | null) => {
    setBusy(true);
    try { adopt(await put<HostIngressStatus>(base, { caddy })); setError(''); notify(caddy ? 'Created applications are published to this Caddy.' : 'Web ingress is detected automatically.'); }
    catch (reason) { setError(message(reason)); } finally { setBusy(false); }
  };
  const adoptPages = async () => {
    setBusy(true);
    try { adopt(await post<HostIngressStatus>(`${base}/error-pages`)); setError(''); notify('Error routes now serve the managed pages.'); }
    catch (reason) { setError(message(reason)); } finally { setBusy(false); }
  };
  const submit = (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); void save({ admin: admin.trim(), ...(server.trim() ? { server: server.trim() } : {}) }); };

  const header = <SectionHeader title="Web ingress" description="How created applications receive their hostnames and HTTPS certificates." actions={status && <Button size="sm" disabled={busy} onClick={() => { void load(true); }}><RefreshCw size={15} />Detect again</Button>} />;
  if (error && !status) return <Card className="form-card">{header}<ErrorBanner>{error}</ErrorBanner></Card>;
  if (!status) return <Card className="form-card">{header}<Loading label="Detecting the web ingress…" /></Card>;

  return <Card className="form-card" data-server-ingress={serverID}>
    {header}
    {error && <ErrorBanner>{error}</ErrorBanner>}
    <dl className="move-facts">
      <dt>Web ingress</dt><dd><strong>{ingressLabel(status)}</strong> <Badge tone={status.source === 'detected' ? 'neutral' : 'accent'}>{sourceLabel[status.source]}</Badge></dd>
      {status.mode === 'caddy' && status.caddy_server && <><dt>Caddy server</dt><dd><code>{status.caddy_server}</code></dd></>}
      {status.detail && <><dt>Detection</dt><dd>{status.detail}{status.web_ports?.length ? ` · listening on ${status.web_ports.join(', ')}` : ''}</dd></>}
    </dl>
    {status.mode === 'caddy'
      ? <p className="muted-copy">Each created application gets its own Caddy route to the agent proxy, and its hostnames are added to Caddy's TLS automation. Certificates are issued once DNS points here.</p>
      : <p className="muted-copy">The web server forwards every hostname to the agent proxy and issues certificates on demand once DNS points here.</p>}
    {status.mode === 'caddy' && status.error_pages && <ErrorPagesFacts pages={status.error_pages} busy={busy} onAdopt={() => { void adoptPages(); }} />}
    {status.configured
      ? <Notice>The agent configuration file sets <code>native.caddy</code>; change it on the server.</Notice>
      : <form className="form-grid two" onSubmit={submit} aria-label="Caddy admin API override">
        <Field label="Caddy admin API" hint="Loopback URL. Leave the override empty to detect the ingress automatically."><Input name="caddy_admin" type="url" required value={admin} onChange={event => setAdmin(event.target.value)} placeholder="http://127.0.0.1:2019" /></Field>
        <Field label="Caddy server name" hint="The HTTPS server in Caddy's configuration; default: the one listening on :443."><Input name="caddy_server" value={server} onChange={event => setServer(event.target.value)} pattern="[A-Za-z0-9_-]{1,64}" placeholder="srv0" /></Field>
        <div className="sticky-actions">
          {status.override && <Button type="button" disabled={busy} onClick={() => { void save(null); }}><Undo2 size={15} />Detect automatically</Button>}
          <Button type="submit" variant="primary" disabled={busy || !admin.trim()}><Save size={15} />{busy ? 'Checking…' : 'Use this Caddy'}</Button>
        </div>
      </form>}
  </Card>;
}
