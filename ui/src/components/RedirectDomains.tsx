import { Plus, Save, X } from 'lucide-react';
import { type KeyboardEvent, useEffect, useState } from 'react';
import { APIError, get, put } from '../lib/api';
import type { DomainStatus, RedirectDomain, RedirectDomainsView } from '../lib/types';
import { DnsRecordsTable, dnsAddressRecords } from './DnsRecords';
import { Badge, Button, Card, ErrorBanner, Field, Loading, Notice, SectionHeader } from './ui';
import { Input } from './primitives/input';
import { useToast } from './ToastProvider';

const statuses = [301, 302, 307, 308];
const statusLabels: Record<number, string> = { 301: '301 Moved permanently', 302: '302 Found (temporary)', 307: '307 Temporary redirect', 308: '308 Permanent redirect' };
const customTarget = '__custom__';

type Row = { id: string; host: string; target: string; custom: boolean; status: number; keep_path: boolean };

function toRows(list: RedirectDomain[], hostnames: string[]): Row[] {
  return list.map(item => ({ id: crypto.randomUUID(), host: item.host, target: item.target || '', custom: Boolean(item.target && !hostnames.includes(item.target)), status: item.status || 301, keep_path: item.keep_path !== false }));
}

function certificateLine(status: DomainStatus | undefined, ingress: string): string {
  if (status?.state === 'ready') return 'HTTPS ready';
  if (status?.message) return status.message;
  if (status?.state === 'requesting_ssl') return 'Requesting HTTPS certificate';
  if (status) return 'Waiting for DNS verification';
  if (ingress === 'caddy') return 'Caddy obtains the certificate once DNS points to this server.';
  if (ingress === 'caddyfile') return 'Not served on this server (see above).';
  return 'Certificate issued on demand once DNS points to this server.';
}

// Enter in a row must not submit the surrounding settings form.
const noSubmit = (event: KeyboardEvent) => { if (event.key === 'Enter') event.preventDefault(); };

/** RedirectDomains edits the hostnames that redirect to this application (or elsewhere), with the DNS records each one needs. */
export function RedirectDomains({ appID, disabled = false }: { appID: string; disabled?: boolean }) {
  const [view, setView] = useState<RedirectDomainsView>();
  const [rows, setRows] = useState<Row[]>([]);
  const [error, setError] = useState('');
  const [unsupported, setUnsupported] = useState(false);
  const [saving, setSaving] = useState(false);
  const notify = useToast();
  const path = `control/apps/${encodeURIComponent(appID)}/redirects`;
  useEffect(() => {
    let active = true;
    get<RedirectDomainsView>(path).then(value => { if (!active) return; setView(value); setRows(toRows(value.redirects || [], value.hostnames || [])); })
      .catch(reason => { if (!active) return; if (reason instanceof APIError && reason.status === 404) setUnsupported(true); else setError(reason instanceof Error ? reason.message : String(reason)); });
    return () => { active = false; };
  }, [path]);
  const update = (id: string, values: Partial<Row>) => setRows(current => current.map(row => row.id === id ? { ...row, ...values } : row));
  const hostnames = view?.hostnames || [];
  const primary = hostnames[0] || '';
  const save = async () => {
    setSaving(true); setError('');
    try {
      const redirects: RedirectDomain[] = rows.filter(row => row.host.trim()).map(row => ({ host: row.host.trim().toLowerCase(), ...(row.target.trim() ? { target: row.target.trim().toLowerCase() } : {}), status: row.status, keep_path: row.keep_path }));
      const next = await put<RedirectDomainsView>(path, { redirects });
      setView(next); setRows(toRows(next.redirects || [], next.hostnames || []));
      notify(next.note || 'Redirect domains saved.');
    } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setSaving(false); }
  };
  const header = <SectionHeader title="Redirect domains" description="Hostnames that answer with a redirect instead of the application: retired sites, old names, a bare domain. They keep redirecting while the application is stopped or in maintenance." actions={view && <Button type="button" variant="primary" disabled={disabled || saving} onClick={save}><Save size={16} />{saving ? 'Saving…' : 'Save redirects'}</Button>} />;
  if (unsupported) return <Card className="form-card">{header}<p className="muted-copy">Update the hosting agent to manage redirect domains.</p></Card>;
  if (!view && !error) return <Card className="form-card">{header}<Loading label="Loading redirect domains…" /></Card>;
  return <Card className="form-card redirect-domains">{header}
    {error && <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner>}
    {view?.ingress === 'caddyfile' && <Notice>{view.detail}</Notice>}
    {view && (view.warnings || []).filter(warning => warning !== view.detail).map(warning => <Notice key={warning}>{warning}</Notice>)}
    <fieldset disabled={disabled} className="settings-stack">
      {rows.length === 0 && <p className="muted-copy">No redirect domains.</p>}
      {rows.map((row, index) => {
        const host = row.host.trim().toLowerCase();
        const target = row.target.trim().toLowerCase() || primary;
        const external = Boolean(row.target.trim()) && !hostnames.includes(row.target.trim().toLowerCase());
        const status = view?.statuses?.find(item => item.domain === host);
        return <div className="redirect-row" key={row.id} role="group" aria-label={`Redirect domain ${index + 1}`}>
          <div className="form-grid two">
            <Field label="Hostname"><div className="input-action"><Input aria-label={`Redirect hostname ${index + 1}`} value={row.host} placeholder="old.example.com" maxLength={253} autoCapitalize="none" spellCheck={false} onKeyDown={noSubmit} onChange={event => update(row.id, { host: event.target.value.toLowerCase().trim() })} /><Button type="button" size="icon" aria-label={`Remove redirect domain ${index + 1}`} onClick={() => setRows(current => current.filter(item => item.id !== row.id))}><X size={16} /></Button></div></Field>
            <Field label="Redirect to" hint={external ? <Badge tone="warning">External hostname: this application does not serve it</Badge> : undefined}>
              <select aria-label={`Redirect target ${index + 1}`} value={row.custom ? customTarget : row.target} onChange={event => event.target.value === customTarget ? update(row.id, { custom: true, target: '' }) : update(row.id, { custom: false, target: event.target.value })}>
                <option value="">{primary ? `${primary} (primary)` : 'Primary hostname'}</option>
                {hostnames.slice(1).map(name => <option key={name} value={name}>{name}</option>)}
                <option value={customTarget}>Custom hostname…</option>
              </select>
              {row.custom && <Input aria-label={`Custom redirect target ${index + 1}`} value={row.target} placeholder="www.example.org" maxLength={253} autoCapitalize="none" spellCheck={false} onKeyDown={noSubmit} onChange={event => update(row.id, { target: event.target.value.toLowerCase().trim() })} />}
            </Field>
            <Field label="Status"><select aria-label={`Redirect status ${index + 1}`} value={row.status} onChange={event => update(row.id, { status: Number(event.target.value) })}>{statuses.map(code => <option key={code} value={code}>{statusLabels[code]}</option>)}</select></Field>
            <label className="check-row"><Input type="checkbox" aria-label={`Keep path ${index + 1}`} checked={row.keep_path} onChange={event => update(row.id, { keep_path: event.target.checked })} /><span><strong>Keep path and query</strong><small>{host || 'old.example.com'}/a?b → https://{target || 'target'}{row.keep_path ? '/a?b' : '/'}</small></span></label>
          </div>
          {host && (view?.addresses || []).length > 0 && <div className="redirect-dns"><small className="field-hint">Point {host} to this server:</small><DnsRecordsTable label={`DNS records for ${host}`} records={dnsAddressRecords(host, view?.addresses || [])} /></div>}
          {host && <small className="field-hint [overflow-wrap:anywhere]" role="status">{certificateLine(status, view?.ingress || '')}</small>}
        </div>;
      })}
      <div><Button type="button" disabled={rows.length >= 50} onClick={() => setRows(current => [...current, { id: crypto.randomUUID(), host: '', target: '', custom: false, status: 301, keep_path: true }])}><Plus size={16} />Add redirect domain</Button></div>
    </fieldset>
  </Card>;
}
