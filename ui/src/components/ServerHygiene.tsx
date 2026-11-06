import { useEffect, useState } from 'react';
import { Gem, ScanSearch } from 'lucide-react';
import { get, post } from '../lib/api';
import { formatBytes, formatDate } from '../lib/format';
import { Badge, Button, Card, ConfirmAction, ErrorBanner, Loading, SectionHeader } from './ui';

export interface CleanupItem { kind: string; name: string; bytes: number; reason?: string }
export interface CleanupPreview { checked: string; items: CleanupItem[]; bytes: number; notes?: string[] }

const kindLabel: Record<string, string> = { image: 'Image', builder: 'Builder', volume: 'Volume', 'package-cache': 'Package cache', 'move-temp': 'Move transfer file', 'native-release': 'Old release', 'native-cache': 'Cache', 'native-scratch': 'Build scratch', toolchain: 'Toolchain', 'app-log': 'Rotated logs', 'core-dump': 'Core dumps' };

/** The dry run of the next cleanup: what it would remove now and the space it frees. */
export function CleanupPreviewCard({ serverID }: { serverID: string }) {
 const [preview, setPreview] = useState<CleanupPreview>();
 const [busy, setBusy] = useState(false);
 const [error, setError] = useState('');
 useEffect(() => { setPreview(undefined); setError(''); }, [serverID]);
 const check = async () => {
  setBusy(true);
  try { const value = await get<{ preview?: CleanupPreview }>(`control/servers/${encodeURIComponent(serverID)}/maintenance?preview=1`); setPreview(value.preview || { checked: new Date().toISOString(), items: [], bytes: 0 }); setError(''); }
  catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); }
  finally { setBusy(false); }
 };
 return <Card><SectionHeader title="What the next cleanup removes" description="A dry run on the server: old release images and native releases (the current and previous release of every application stay), stopped builders, anonymous volumes, oversized caches, rotated logs, core dumps, the package cache and files of interrupted move transfers." actions={<Button onClick={check} pending={busy}><ScanSearch size={16} />Check now</Button>} />
  {error && <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner>}
  {busy && !preview && <Loading label="Checking the server…" />}
  {preview && <>{preview.items.length ? <><p className="muted-copy">About <strong>{formatBytes(preview.bytes)}</strong> can be freed ({preview.items.length} item{preview.items.length === 1 ? '' : 's'}, checked {formatDate(preview.checked)}).</p><ul className="maintenance-preview" aria-label="Cleanup preview">{preview.items.map(item => <li key={item.kind + item.name}><Badge>{kindLabel[item.kind] || item.kind}</Badge> <code>{item.name}</code> <strong>{formatBytes(item.bytes)}</strong>{item.reason && <small> · {item.reason}</small>}</li>)}</ul></> : <p className="muted-copy">Nothing to remove beyond the build cache and journal cleanup.</p>}{preview.notes?.map(note => <p className="muted-copy" key={note}>{note}</p>)}</>}
 </Card>;
}

interface BundlerGroup { user: string; ruby: string; rubygems: string; default_bundler: string; apps: string[]; remove: string[]; keep?: Array<{ version: string; reason: string }> }
interface BundlerJob { status: string; summary?: string; output?: string; started: string }
interface BundlerStatus { groups: BundlerGroup[]; job?: BundlerJob }

/** Removes Bundler versions no release runs with, after every application's bundle check. */
export function BundlerCleanupCard({ serverID }: { serverID: string }) {
 const [status, setStatus] = useState<BundlerStatus>();
 const [error, setError] = useState('');
 const [reload, setReload] = useState(0);
 const path = `control/servers/${encodeURIComponent(serverID)}/host/ruby/bundlers`;
 useEffect(() => {
  let active = true, timer: ReturnType<typeof setTimeout>;
  const load = async () => {
   try { const value = await get<BundlerStatus>(path); if (!active) return; setStatus(value); setError(''); if (value.job?.status === 'running') timer = setTimeout(() => { void load(); }, 2000); }
   catch { if (active) setStatus(undefined); }
  };
  void load(); return () => { active = false; clearTimeout(timer); };
 }, [path, reload]);
 if (!status || (!status.groups.length && !status.job)) return null;
 const removable = status.groups.filter(group => group.remove.length);
 const running = status.job?.status === 'running';
 const listing = removable.map(group => `Ruby ${group.ruby} (${group.user}): Bundler ${group.remove.join(', ')}`).join('; ');
 const remove = async () => {
  try { setStatus(await post<BundlerStatus>(`${path}/remove`, { remove: removable.flatMap(group => group.remove.map(version => ({ user: group.user, ruby: group.ruby, version }))) })); setError(''); setReload(current => current + 1); }
  catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); throw reason; }
 };
 return <Card><SectionHeader title="Unused Bundler versions" description="Old Bundler versions beside a newer RubyGems print warnings on every command. Versions any kept release runs with, and RubyGems' default, always stay." actions={running ? <Badge tone="warning">Removing…</Badge> : undefined} />
  {error && <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner>}
  <ul className="maintenance-preview" aria-label="Bundler versions">{status.groups.map(group => <li key={group.user + group.ruby}><Gem size={15} /> Ruby <strong>{group.ruby}</strong> ({group.user}, RubyGems {group.rubygems}, default Bundler {group.default_bundler}) · {group.apps.join(', ')}{group.remove.length > 0 && <div>Removable: <code>{group.remove.join(', ')}</code></div>}{group.keep?.map(kept => <div key={kept.version}><small>Kept {kept.version}: {kept.reason}</small></div>)}</li>)}</ul>
  {removable.length > 0 && <div className="maintenance-actions"><ConfirmAction title="Remove unused Bundler versions?" description={`This removes ${listing}. Every application on these Rubies first resolves its bundle in a scratch copy of its current release; nothing is removed if one fails, and removed versions are installed again if an application stops resolving afterwards.`} confirmLabel="Remove versions" destructive onConfirm={remove}><Button variant="danger" disabled={running}><Gem size={16} />Remove unused Bundler versions</Button></ConfirmAction></div>}
  {status.job && !running && <><p className="muted-copy">Last removal {formatDate(status.job.started)}: {status.job.status === 'failed' ? 'failed — ' : ''}{status.job.summary}</p>{status.job.output && <pre className="maintenance-report">{status.job.output}</pre>}</>}
 </Card>;
}
