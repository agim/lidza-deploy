import { type FormEvent, useEffect, useState } from 'react';
import { del, get, post, put } from '../lib/api';
import { Input } from './primitives/input';
import { useToast } from './ToastProvider';
import { ActionForm, Badge, Button, Card, ConfirmAction, EmptyState, ErrorBanner, Field, Loading, SectionHeader } from './ui';
import { Stack } from './layout';

/** An installed plugin and the keys a server trusts (control/servers/{id}/plugins). */
export type PluginRecord = { name: string; version: string; provides: string; summary?: string; hooks?: string[]; key_id?: string; installed?: string };
export type PluginsView = { plugins?: PluginRecord[]; keys?: string[]; operator_keys?: string[]; pinned?: number; arch?: string };
type PluginCheck = { id: string; title: string; ok: boolean; detail?: string; repairable?: boolean; caution?: boolean };

const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);
export const pluginsEndpoint = (serverID: string) => `control/servers/${encodeURIComponent(serverID)}/plugins`;

/**
 * ServerPlugins: the plugins of a server. A plugin is a separate, signed
 * program that manages one piece of application-specific software (a search
 * server, an analytics database); the agent runs it and knows nothing about
 * what is behind it. This tab trusts signing keys, installs a plugin from its
 * published release and shows what each plugin's own check says.
 */
export function ServerPlugins({ serverID }: { serverID: string }) {
  const notify = useToast();
  const [view, setView] = useState<PluginsView>();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState('');
  const [keys, setKeys] = useState('');
  const [checks, setChecks] = useState<Record<string, PluginCheck[]>>({});
  const accept = (next: PluginsView) => { setView(next); setKeys((next.operator_keys || []).join('\n')); };
  const load = () => get<PluginsView>(pluginsEndpoint(serverID)).then(next => { accept(next || {}); setError(''); }).catch(reason => { setView({}); setError(message(reason)); });
  useEffect(() => { setView(undefined); setChecks({}); void load(); }, [serverID]);
  const run = async (label: string, action: () => Promise<void>) => { setBusy(label); setError(''); try { await action(); } catch (reason) { setError(message(reason)); } finally { setBusy(''); } };
  const saveKeys = (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); void run('keys', async () => { accept(await put<PluginsView>(`${pluginsEndpoint(serverID)}/keys`, { keys: keys.split(/\s+/).filter(Boolean) })); notify('Trusted plugin keys saved.'); }); };
  const install = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = event.currentTarget; const values = Object.fromEntries(new FormData(form)) as Record<string, string>;
    void run('install', async () => { const record = await post<PluginRecord>(`${pluginsEndpoint(serverID)}/install`, { repository: values.repository, version: values.version }); notify(`Plugin ${record.name} ${record.version} installed.`); form.reset(); await load(); });
  };
  const check = (name: string) => run(`check:${name}`, async () => { const result = await post<{ answer?: { ok?: boolean; checks?: PluginCheck[]; messages?: string[] } }>(`${pluginsEndpoint(serverID)}/${encodeURIComponent(name)}/hooks/check`, {}); const list = result.answer?.checks || []; setChecks(current => ({ ...current, [name]: list.length ? list : [{ id: 'answer', title: name, ok: Boolean(result.answer?.ok), detail: (result.answer?.messages || []).join(' ') || (result.answer?.ok ? 'No problems reported.' : 'The plugin reported a problem without details.') }] })); });
  const remove = (name: string) => run(`remove:${name}`, async () => { await del(`${pluginsEndpoint(serverID)}/${encodeURIComponent(name)}`); notify(`Plugin ${name} removed.`); await load(); });
  if (!view) return <Loading />;
  const plugins = view.plugins || [];
  return <Stack gap="page" data-server-plugins={serverID}>
    {error && <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner>}
    <Card><SectionHeader title="Installed plugins" description="A plugin is a separate, signed program that manages one piece of application-specific software on this server. The agent runs it with limits and knows nothing about what is behind it." actions={<Badge>{plugins.length} installed</Badge>} />
      {plugins.length === 0 ? <EmptyState title="No plugins on this server" description="Install one from its published release below, after trusting its signing key." />
        : <div className="simple-list">{plugins.map(plugin => <div key={plugin.name} data-plugin={plugin.name}><div><strong>{plugin.name} {plugin.version}</strong><small>Provides {plugin.provides}{plugin.summary ? ` · ${plugin.summary}` : ''}{plugin.key_id ? ` · signed by ${plugin.key_id}` : ''}</small>
          {checks[plugin.name] && <ul className="plugin-checks" aria-label={`Checks of ${plugin.name}`}>{checks[plugin.name].map(item => <li key={item.id}><Badge tone={item.ok && !item.caution ? 'success' : item.ok || item.repairable ? 'warning' : 'danger'}>{item.ok && item.caution ? 'At risk' : item.ok ? 'OK' : item.repairable ? 'Repairable' : 'Problem'}</Badge> <strong>{item.title}</strong> <span>{item.detail}</span></li>)}</ul>}
        </div><span className="row-actions">{(plugin.hooks || []).includes('check') && <Button type="button" size="sm" disabled={busy !== ''} onClick={() => { void check(plugin.name); }}>{busy === `check:${plugin.name}` ? 'Checking…' : 'Check server'}</Button>}<ConfirmAction title={`Remove the ${plugin.name} plugin?`} description="Its program is removed from this server. What it manages, and its own data, stay as they are." confirmLabel="Remove plugin" destructive onConfirm={() => remove(plugin.name)}><Button type="button" size="sm" variant="danger" disabled={busy !== ''}>Remove</Button></ConfirmAction></span></div>)}</div>}
    </Card>
    <ActionForm onSubmit={install}><Card className="form-card"><SectionHeader title="Install a plugin" description={`From release v<version> of a public GitHub repository. The server verifies the release's signature with the keys it trusts and the archive against the signed manifest; this server is ${view.arch || 'of unknown architecture'}.`} />
      <div className="form-grid two"><Field label="Repository" hint="owner/name"><Input name="repository" required placeholder="example/deploy-plugin-search" spellCheck={false} /></Field><Field label="Version"><Input name="version" required placeholder="1.0" spellCheck={false} /></Field></div>
      <div className="sticky-actions"><Button type="submit" variant="primary" disabled={busy !== ''}>{busy === 'install' ? 'Installing…' : 'Install plugin'}</Button></div>
    </Card></ActionForm>
    <ActionForm onSubmit={saveKeys}><Card className="form-card"><SectionHeader title="Trusted signing keys" description={`A plugin runs with full access to this server, so it installs only when its release is signed by a key trusted here. ${view.pinned ? `${view.pinned} key${view.pinned === 1 ? ' is' : 's are'} built into the agent; ` : ''}add the public key its author publishes.`} actions={<Badge>{(view.keys || []).length} trusted</Badge>} />
      <Field label="Public keys you trust" hint="One per line, 64 hexadecimal characters each. Saving replaces the list."><textarea aria-label="Public keys you trust" rows={3} spellCheck={false} value={keys} onChange={event => setKeys(event.target.value)} /></Field>
      <div className="sticky-actions"><Button type="submit" disabled={busy !== ''}>{busy === 'keys' ? 'Saving…' : 'Save trusted keys'}</Button></div>
    </Card></ActionForm>
  </Stack>;
}
