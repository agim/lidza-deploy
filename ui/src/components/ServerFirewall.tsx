import { type FormEvent, useEffect, useState } from 'react';
import { CheckCircle2, ListChecks, Plus, RefreshCw, Save, ShieldCheck, Trash2, Undo2 } from 'lucide-react';
import { get, post, put } from '../lib/api';
import { formatDate } from '../lib/format';
import type { HostFirewallModel, HostFirewallPlan, HostFirewallRule, HostFirewallView } from '../lib/types';
import { useToast } from './ToastProvider';
import { ResourceList, type Column } from './ResourceList';
import { Badge, Button, Card, ErrorBanner, Field, IconButton, Loading, Notice, SectionHeader } from './ui';
import { Input } from './primitives/input';
import { Switch } from './primitives/switch';

type Phase = 'idle' | 'review' | 'applying' | 'confirming';
interface Row { rule: HostFirewallRule; index?: number }

const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);
const sleep = (ms: number) => new Promise(resolve => setTimeout(resolve, ms));
const sources = (value: string) => value.split(/[\s,]+/).map(item => item.trim()).filter(Boolean);
const sourceLabel = (source: string) => source === 'system' ? 'System' : source === 'user' ? 'Custom' : source.startsWith('app:') ? `App · ${source.slice(4)}` : source;

export function ServerFirewall({ serverID }: { serverID: string }) {
  const notify = useToast();
  const base = `control/servers/${encodeURIComponent(serverID)}/host/firewall`;
  const [view, setView] = useState<HostFirewallView>();
  const [draft, setDraft] = useState<HostFirewallModel>();
  const [sshFrom, setSSHFrom] = useState('');
  const [plan, setPlan] = useState<HostFirewallPlan>();
  const [phase, setPhase] = useState<Phase>('idle');
  const [pendingUntil, setPendingUntil] = useState(0);
  const [now, setNow] = useState(Date.now());
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);

  const adopt = (value: HostFirewallView) => { setView(value); setDraft(value.model); setSSHFrom((value.model.ssh_from || []).join(', ')); if (value.pending_seconds) setPendingUntil(Date.now() + value.pending_seconds * 1000); };
  const load = async () => { try { adopt(await get<HostFirewallView>(base)); setError(''); } catch (reason) { setError(message(reason)); } };
  useEffect(() => { setView(undefined); setDraft(undefined); setPlan(undefined); setPhase('idle'); void load(); }, [serverID]);
  useEffect(() => { if (!pendingUntil) return; const timer = setInterval(() => setNow(Date.now()), 1000); return () => clearInterval(timer); }, [pendingUntil]);

  if (error && !view) return <Card className="form-card"><SectionHeader title="Host firewall" /><ErrorBanner>{error}</ErrorBanner></Card>;
  if (!view || !draft) return <Card className="form-card"><SectionHeader title="Host firewall" /><Loading label="Reading the host firewall…" /></Card>;
  if (!view.supported) return <Card className="form-card"><SectionHeader title="Host firewall" /><Notice>{view.reason || 'The host firewall is not managed on this server.'}</Notice></Card>;

  const model: HostFirewallModel = { ...draft, ssh_from: sources(sshFrom) };
  const dirty = JSON.stringify(model) !== JSON.stringify({ ...view.model, ssh_from: view.model.ssh_from || [] });
  const secondsLeft = pendingUntil ? Math.max(0, Math.round((pendingUntil - now) / 1000)) : 0;
  const pending = !!view.pending_seconds || phase === 'confirming';
  const locked = busy || phase === 'applying' || phase === 'confirming' || pending || !!view.applying;

  const save = async () => { const value = await put<HostFirewallView>(base, model); adopt(value); return value; };
  const review = async () => {
    setBusy(true);
    try { if (dirty) await save(); setPlan(await get<HostFirewallPlan>(`${base}/plan`)); setPhase('review'); setError(''); }
    catch (reason) { setError(message(reason)); } finally { setBusy(false); }
  };
  const confirm = async () => {
    const value = await post<HostFirewallView>(`${base}/confirm`, {});
    adopt(value); setPendingUntil(0); setPhase('idle'); setPlan(undefined);
    notify('Firewall change confirmed.');
  };
  const apply = async () => {
    if (!plan) return;
    setPhase('applying'); setError('');
    try {
      let value = await post<HostFirewallView>(`${base}/apply`, { fingerprint: plan.fingerprint });
      const started = Date.now();
      // Wait for the agent to finish applying, then prove the panel still
      // reaches it before confirming; otherwise the agent rolls back by itself.
      while (Date.now() - started < 11 * 60_000) {
        if (!value.applying && value.pending_seconds) break;
        if (!value.applying && !value.pending_seconds) { adopt(value); setPhase('idle'); setPlan(undefined); if (value.last_error) setError(value.last_error); else notify('Firewall is already up to date.'); return; }
        await sleep(2000);
        try { value = await get<HostFirewallView>(base); } catch { /* the agent may be briefly unreachable while rules change */ }
      }
      adopt(value); setPhase('confirming');
      const deadline = Date.now() + (value.pending_seconds || 0) * 1000;
      while (Date.now() < deadline) {
        try { await get<HostFirewallView>(base); await confirm(); return; }
        catch { await sleep(3000); }
      }
      setPhase('idle'); setPlan(undefined);
      setError('The server did not answer after the change. The agent restores the previous firewall automatically.');
      await load();
    } catch (reason) { setPhase('idle'); setError(message(reason)); await load(); }
  };
  const addRule = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = event.currentTarget; const values = new FormData(form);
    const rule: HostFirewallRule = { port: String(values.get('port') || '').trim(), proto: (values.get('proto') || 'tcp') as HostFirewallRule['proto'], from: sources(String(values.get('from') || '')), comment: String(values.get('comment') || '').trim(), source: 'user' };
    if (!rule.port) return;
    setDraft(current => current ? { ...current, rules: [...current.rules, rule] } : current);
    form.reset();
  };
  const remove = (index: number) => setDraft(current => current ? { ...current, rules: current.rules.filter((_, i) => i !== index) } : current);

  const rows: Row[] = [...view.rules.filter(rule => rule.source === 'system').map(rule => ({ rule })), ...draft.rules.map((rule, index) => ({ rule, index })), ...view.rules.filter(rule => rule.source.startsWith('app:')).map(rule => ({ rule }))];
  const columns: Column<Row>[] = [
    { id: 'port', label: 'Port', render: ({ rule }) => <code>{rule.port}</code> },
    { id: 'proto', label: 'Protocol', render: ({ rule }) => rule.proto === 'any' ? 'TCP + UDP' : rule.proto.toUpperCase() },
    { id: 'from', label: 'From', render: ({ rule }) => rule.from?.length ? <span className="firewall-sources">{rule.from.map(item => <code key={item}>{item}</code>)}</span> : <span className="muted-copy">Anywhere</span> },
    { id: 'comment', label: 'Comment', render: ({ rule }) => rule.comment || <span className="muted-copy">—</span> },
    { id: 'source', label: 'Source', render: ({ rule }) => <Badge tone={rule.source === 'system' ? 'accent' : 'neutral'}>{sourceLabel(rule.source)}{rule.enforced === 'docker' ? ' · Docker' : ''}</Badge> },
    { id: 'actions', label: 'Actions', render: ({ index }) => index === undefined ? <span className="muted-copy">Managed</span> : <IconButton label="Remove rule" disabled={locked} onClick={() => remove(index)}><Trash2 size={15} /></IconButton> },
  ];

  return <Card className="form-card" data-host-firewall={serverID}>
    <SectionHeader title="Host firewall" description="Inbound traffic to this server is denied unless a rule below allows it." actions={<Button size="sm" onClick={load} disabled={locked}><RefreshCw size={15} />Refresh</Button>} />
    {error && <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner>}
    {view.last_error && !error && <Notice>{view.last_error}</Notice>}
    <div className="host-summary">
      <Badge tone={view.active ? 'success' : 'warning'}>{!view.installed ? 'ufw not installed' : view.active ? 'Firewall active' : 'Firewall inactive'}</Badge>
      {view.default_incoming && <Badge>Default inbound: {view.default_incoming}</Badge>}
      {view.last_applied && <Badge>Applied {formatDate(view.last_applied)}</Badge>}
      {view.applying && <Badge tone="warning">Applying…</Badge>}
    </div>
    {pending && !view.applying && <div className="firewall-countdown" role="status"><ShieldCheck size={18} /><span><strong>Waiting for confirmation{secondsLeft ? ` · ${secondsLeft}s` : ''}</strong><small>If the change is not confirmed, the server restores its previous firewall automatically.</small></span>{phase !== 'confirming' && <Button size="sm" variant="primary" onClick={() => { confirm().catch(reason => { setError(message(reason)); void load(); }); }}><CheckCircle2 size={15} />Confirm change</Button>}</div>}
    <label className="check-row switch-row"><Switch aria-label="Enforce the host firewall" checked={draft.enabled} disabled={locked} onCheckedChange={enabled => setDraft({ ...draft, enabled })} /><span><strong>Enforce the host firewall</strong><small>SSH, HTTP and HTTPS always stay open. Existing rules on the server were imported unchanged.</small></span></label>
    <Field label="SSH allowed from" hint="Addresses or CIDR networks, separated by commas. Leave empty to allow SSH from anywhere."><Input value={sshFrom} disabled={locked} placeholder="Anywhere" onChange={event => setSSHFrom(event.target.value)} /></Field>
    <ResourceList rows={rows} columns={columns} rowKey={row => `${row.rule.source}-${row.rule.port}-${row.rule.proto}-${row.index ?? ''}-${(row.rule.from || []).join(',')}`} />
    <form className="firewall-add" onSubmit={addRule}>
      <Field label="Port"><Input name="port" required pattern="[0-9]{1,5}(:[0-9]{1,5})?" placeholder="5432" disabled={locked} /></Field>
      <Field label="Protocol"><select name="proto" aria-label="Protocol" defaultValue="tcp" disabled={locked}><option value="tcp">TCP</option><option value="udp">UDP</option><option value="any">TCP + UDP</option></select></Field>
      <Field label="From"><Input name="from" placeholder="Anywhere, or 203.0.113.10, 10.0.0.0/8" disabled={locked} /></Field>
      <Field label="Comment"><Input name="comment" maxLength={80} placeholder="Replica access" disabled={locked} /></Field>
      <Button type="submit" disabled={locked}><Plus size={15} />Add rule</Button>
    </form>
    {!!view.kept?.length && <details className="host-packages"><summary>Rules kept exactly as they are ({view.kept.length})</summary><ul>{view.kept.map(line => <li key={line}><code>{line}</code></li>)}</ul></details>}
    {view.notes?.map(note => <p className="muted-copy" key={note}>{note}</p>)}
    <div className="maintenance-actions">
      <Button onClick={() => { save().then(() => notify('Firewall rules saved. Review and apply them to take effect.')).catch(reason => setError(message(reason))); }} disabled={!dirty || locked}><Save size={16} />Save draft</Button>
      <Button variant="primary" onClick={review} pending={busy} disabled={locked}><ListChecks size={16} />Review changes</Button>
      {dirty && <Button variant="ghost" onClick={() => adopt(view)} disabled={locked}><Undo2 size={16} />Discard edits</Button>}
    </div>
    {plan && (phase === 'review' || phase === 'applying') && <div className="firewall-plan">
      <SectionHeader title="Planned changes" description={plan.changes.length ? `${plan.changes.length} command${plan.changes.length === 1 ? '' : 's'} will run on the server.` : 'The server already matches these rules.'} />
      {plan.warnings?.map(warning => <Notice key={warning}>{warning}</Notice>)}
      {!!plan.changes.length && <ol className="firewall-commands">{plan.changes.map((change, index) => <li key={index}><Badge tone={change.action === 'delete' || change.action === 'disable' ? 'danger' : change.action === 'add' ? 'success' : 'warning'}>{change.action}</Badge><code>{change.command}</code></li>)}</ol>}
      {!!plan.unmanaged?.length && <details className="host-packages"><summary>Rules on the server not managed here ({plan.unmanaged.length}); they stay in place</summary><ul>{plan.unmanaged.map(line => <li key={line}><code>{line}</code></li>)}</ul></details>}
      <div className="maintenance-actions">
        <Button variant="primary" onClick={apply} pending={phase === 'applying'} disabled={phase === 'applying' || !plan.changes.length}><ShieldCheck size={16} />Apply firewall</Button>
        <Button onClick={() => { setPlan(undefined); setPhase('idle'); }} disabled={phase === 'applying'}>Cancel</Button>
      </div>
    </div>}
  </Card>;
}
