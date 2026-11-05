import { useEffect, useState } from 'react';
import { Download, Power, RefreshCw, Save, ShieldAlert } from 'lucide-react';
import { get, patch, post } from '../lib/api';
import { formatDate } from '../lib/format';
import type { HostJob, HostUpdatesPolicy, HostUpdatesStatus } from '../lib/types';
import { useToast } from './ToastProvider';
import { Badge, Button, Card, ConfirmAction, ErrorBanner, Field, Loading, Notice, SectionHeader, StatusBadge } from './ui';
import { Input } from './primitives/input';
import { Switch } from './primitives/switch';

const weekdays = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const durations = [30, 60, 120, 180, 240, 360];
const endpoint = (serverID: string) => `control/servers/${encodeURIComponent(serverID)}/host/updates`;
const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);
const jobLabels: Record<string, string> = { 'security-updates': 'Security updates', 'all-updates': 'All updates', configure: 'Automatic updates setup', reboot: 'Reboot', firewall: 'Firewall change' };

function windowLabel(policy: HostUpdatesPolicy) {
  const w = policy.reboot_window;
  const days = w.weekdays?.length ? w.weekdays.map(day => weekdays[day]).join(', ') : 'Every day';
  return `${days} at ${String(w.hour).padStart(2, '0')}:00 UTC for ${w.duration_minutes >= 60 ? `${w.duration_minutes / 60} h` : `${w.duration_minutes} min`}`;
}

/** Small badges for the server list: pending security updates and a required reboot. */
export function HostUpdateBadges({ serverID }: { serverID: string }) {
  const [status, setStatus] = useState<HostUpdatesStatus>();
  useEffect(() => {
    let active = true;
    get<HostUpdatesStatus>(endpoint(serverID)).then(value => { if (active) setStatus(value); }).catch(() => undefined);
    return () => { active = false; };
  }, [serverID]);
  if (!status?.supported || (!status.reboot_required && !status.security)) return null;
  return <div className="host-badges">{status.reboot_required && <Badge tone="warning">Reboot required</Badge>}{status.security > 0 && <Badge tone="warning">{status.security} security update{status.security === 1 ? '' : 's'}</Badge>}</div>;
}

function JobList({ jobs }: { jobs: HostJob[] }) {
  if (!jobs.length) return <p className="muted-copy">No host operations have run yet.</p>;
  return <ul className="host-jobs">{jobs.slice(0, 6).map(job => <li key={job.id}><div className="host-job-heading"><StatusBadge status={job.status === 'succeeded' ? 'done' : job.status} /><strong>{jobLabels[job.kind] || job.kind}</strong><small>{formatDate(job.started)} · {job.trigger}</small></div>{job.summary && <p className="muted-copy">{job.summary}</p>}{job.output && <details><summary>Output</summary><pre className="maintenance-report">{job.output}</pre></details>}</li>)}</ul>;
}

export function ServerUpdates({ serverID }: { serverID: string }) {
  const notify = useToast();
  const [status, setStatus] = useState<HostUpdatesStatus>();
  const [policy, setPolicy] = useState<HostUpdatesPolicy>();
  const [error, setError] = useState('');
  const [checking, setChecking] = useState(false);
  useEffect(() => {
    let active = true; let timer: ReturnType<typeof setTimeout>;
    setStatus(undefined); setPolicy(undefined); setError('');
    const load = async (initial = false) => {
      try {
        const value = await get<HostUpdatesStatus>(endpoint(serverID));
        if (!active) return;
        setStatus(value); if (initial) setPolicy(value.policy); setError('');
        timer = setTimeout(() => { void load(); }, value.running ? 3000 : 20000);
      } catch (reason) { if (active) { setError(message(reason)); timer = setTimeout(() => { void load(initial); }, 20000); } }
    };
    void load(true);
    return () => { active = false; clearTimeout(timer); };
  }, [serverID]);
  const change = (value: Partial<HostUpdatesPolicy>) => setPolicy(current => current ? { ...current, ...value } : current);
  const changeWindow = (value: Partial<HostUpdatesPolicy['reboot_window']>) => setPolicy(current => current ? { ...current, reboot_window: { ...current.reboot_window, ...value } } : current);
  const toggleDay = (day: number) => { const days = policy?.reboot_window.weekdays || []; changeWindow({ weekdays: days.includes(day) ? days.filter(item => item !== day) : [...days, day].sort((x, y) => x - y) }); };
  const save = async () => { if (!policy) return; try { const value = await patch<HostUpdatesStatus>(endpoint(serverID), policy); setStatus(value); setPolicy(value.policy); setError(''); notify('Update policy saved.'); } catch (reason) { setError(message(reason)); } };
  const check = async () => { setChecking(true); try { setStatus(await get<HostUpdatesStatus>(`${endpoint(serverID)}?refresh=1`)); setError(''); } catch (reason) { setError(message(reason)); } finally { setChecking(false); } };
  const run = async (scope: 'security' | 'all') => { setStatus(await post<HostUpdatesStatus>(`${endpoint(serverID)}/run`, { scope })); notify(scope === 'security' ? 'Installing security updates.' : 'Installing all pending updates.'); };
  const reboot = async () => { setStatus(await post<HostUpdatesStatus>(`control/servers/${encodeURIComponent(serverID)}/host/reboot`, { confirm: true })); notify('Reboot requested. The server will be unavailable for a few minutes.'); };

  if (error && !status) return <Card className="form-card"><SectionHeader title="Operating system updates" /><ErrorBanner>{error}</ErrorBanner></Card>;
  if (!status || !policy) return <Card className="form-card"><SectionHeader title="Operating system updates" /><Loading label="Checking host updates…" /></Card>;
  if (!status.supported) return <Card className="form-card"><SectionHeader title="Operating system updates" /><Notice>{status.reason || 'Host updates are not managed on this server.'}</Notice></Card>;
  const busy = !!status.running;
  return <Card className="form-card" data-host-updates={serverID}>
    <SectionHeader title="Operating system updates" description={status.checked_at ? `Checked ${formatDate(status.checked_at)}. Package lists refresh daily on the server.` : undefined} actions={<Button size="sm" onClick={check} pending={checking} disabled={checking || busy}><RefreshCw size={15} />Check now</Button>} />
    {error && <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner>}
    {status.error && <Notice>{status.error}</Notice>}
    <div className="host-summary">
      <Badge tone={status.pending ? 'accent' : 'success'}>{status.pending ? `${status.pending} pending update${status.pending === 1 ? '' : 's'}` : 'Up to date'}</Badge>
      {status.security > 0 && <Badge tone="warning">{status.security} security</Badge>}
      {status.reboot_required && <Badge tone="warning">Reboot required</Badge>}
      <Badge tone={status.unattended_enabled ? 'success' : 'neutral'}>{status.unattended_enabled ? 'Automatic security updates on' : status.unattended_installed ? 'Automatic updates off' : 'unattended-upgrades not installed'}</Badge>
      {busy && <Badge tone="warning">Running: {jobLabels[status.running || ''] || status.running}</Badge>}
    </div>
    {status.reboot_required && <p className="muted-copy">{status.reboot_reason}{status.reboot_packages?.length ? ` (${status.reboot_packages.join(', ')})` : ''}.</p>}
    {status.reboot_deferred && <Notice>{status.reboot_deferred}</Notice>}
    {status.unattended_last_run && <p className="muted-copy">Unattended upgrades last ran {formatDate(status.unattended_last_run)}.</p>}
    {!!status.packages?.length && <details className="host-packages"><summary>Pending packages</summary><ul>{status.packages.map(item => <li key={item.name}><code>{item.name}</code> {item.current ? `${item.current} → ` : 'new '}{item.candidate}{item.security && <Badge tone="warning">security</Badge>}</li>)}</ul></details>}
    <div className="maintenance-actions">
      <ConfirmAction title="Install security updates now?" description="The server refreshes its package lists and installs pending security updates. Services are not restarted; a reboot may be required afterwards." confirmLabel="Install security updates" onConfirm={() => run('security')}><Button variant="primary" disabled={busy}><ShieldAlert size={16} />Install security updates now</Button></ConfirmAction>
      <ConfirmAction title="Install all pending updates?" description="Every pending package update is installed, including updates to Caddy, PostgreSQL, Redis or Docker when their repositories offer one. Updated services may restart briefly." confirmLabel="Install all updates" onConfirm={() => run('all')}><Button disabled={busy || !status.pending}><Download size={16} />Install all updates</Button></ConfirmAction>
      <ConfirmAction title="Reboot this server now?" description="Every application on this server is unavailable until the host is back, usually a few minutes. The reboot is refused while deployments, backups or package updates are running." confirmLabel="Reboot now" destructive onConfirm={reboot}><Button variant="danger" disabled={busy}><Power size={16} />Reboot now</Button></ConfirmAction>
    </div>
    {!!status.components?.length && <div className="host-components">{status.components.map(item => <div key={item.name}><strong>{item.name}</strong><span>{item.source === 'absent' ? 'Not installed' : item.installed || 'Unknown version'}{item.upgrade_available && item.candidate ? ` → ${item.candidate}` : ''}</span><small>{item.source === 'binary' ? 'Standalone binary; package updates do not apply' : item.source === 'package' ? item.automatic === 'all' ? 'Updates install automatically' : item.automatic === 'security' ? 'Security updates install automatically' : 'Updates install manually' : ''}</small></div>)}</div>}
    <SectionHeader title="Update policy" description="Runs on the server itself, even while the control panel is offline." />
    <label className="check-row switch-row"><Switch aria-label="Automatic security updates" checked={policy.auto_security} onCheckedChange={auto_security => change({ auto_security, manage_package_sources: auto_security && policy.manage_package_sources })} /><span><strong>Automatic security updates</strong><small>Installs and enables unattended-upgrades for the distribution's security updates.</small></span></label>
    <label className="check-row switch-row"><Switch aria-label="Include official package repositories" checked={policy.manage_package_sources} disabled={!policy.auto_security} onCheckedChange={manage_package_sources => change({ manage_package_sources })} /><span><strong>Include official Caddy, PostgreSQL and Docker repositories</strong><small>{status.managed_origins?.length ? `Configured here: ${status.managed_origins.join('; ')}.` : 'None of these repositories is configured on this server.'}</small></span></label>
    <label className="check-row switch-row"><Switch aria-label="Reboot automatically" checked={policy.auto_reboot} onCheckedChange={auto_reboot => change({ auto_reboot })} /><span><strong>Reboot automatically when required</strong><small>Only inside the window below, at most once every six hours, and never while deployments or backups run.</small></span></label>
    <fieldset disabled={!policy.auto_reboot} className="host-window">
      <legend className="field-label">Reboot window (UTC)</legend>
      <div className="host-weekdays">{weekdays.map((label, day) => <label key={label} className="check-row"><Input type="checkbox" checked={!!policy.reboot_window.weekdays?.includes(day)} onChange={() => toggleDay(day)} /><span>{label}</span></label>)}</div>
      <div className="form-grid two"><Field label="Start hour (UTC)"><select aria-label="Start hour (UTC)" value={policy.reboot_window.hour} onChange={event => changeWindow({ hour: Number(event.target.value) })}>{Array.from({ length: 24 }, (_, hour) => <option key={hour} value={hour}>{String(hour).padStart(2, '0')}:00</option>)}</select></Field><Field label="Window length"><select aria-label="Window length" value={policy.reboot_window.duration_minutes} onChange={event => changeWindow({ duration_minutes: Number(event.target.value) })}>{durations.map(minutes => <option key={minutes} value={minutes}>{minutes >= 60 ? `${minutes / 60} hour${minutes === 60 ? '' : 's'}` : `${minutes} minutes`}</option>)}</select></Field></div>
      <p className="muted-copy">{windowLabel(policy)}. No day selected means every day.</p>
    </fieldset>
    <div className="maintenance-actions"><Button variant="primary" onClick={save}><Save size={16} />Save update policy</Button></div>
    <p className="muted-copy">{status.policy.auto_reboot && status.next_reboot_window ? `Next reboot window: ${formatDate(status.next_reboot_window)}.` : 'Automatic reboots are off.'}{status.last_reboot ? ` Last reboot by the agent: ${formatDate(status.last_reboot)}.` : ''}</p>
    <SectionHeader title="Recent host operations" />
    <JobList jobs={status.jobs || []} />
  </Card>;
}
