import { useEffect, useState } from 'react';
import { Download, Power, RefreshCw, Save, ShieldAlert, TriangleAlert } from 'lucide-react';
import { get, patch, post } from '../lib/api';
import { formatDate } from '../lib/format';
import type { HostJob, HostPackageUpgrade, HostUpdatesPolicy, HostUpdatesStatus } from '../lib/types';
import { useToast } from './ToastProvider';
import { Badge, Button, Card, ConfirmAction, ErrorBanner, Field, Loading, Notice, SectionHeader, StatusBadge } from './ui';
import { Input } from './primitives/input';
import { LiveProgress } from './LiveProgress';
import { Switch } from './primitives/switch';

const weekdays = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const durations = [30, 60, 120, 180, 240, 360];
const endpoint = (serverID: string) => `control/servers/${encodeURIComponent(serverID)}/host/updates`;
const message = (reason: unknown) => reason instanceof Error ? reason.message : String(reason);
const jobLabels: Record<string, string> = { 'security-updates': 'Security updates', 'all-updates': 'All updates', 'disruptive-updates': 'Disruptive updates (maintenance window)', configure: 'Automatic updates setup', reboot: 'Reboot', 'docker-live-restore': 'Docker live-restore', 'docker-restart': 'Docker restart (maintenance window)', firewall: 'Firewall change', 'gateway-setup': 'Protected gateway setup', 'resource-tuning': 'Resource tuning' };
const securityRule = 'Only packages whose new version comes from the distribution\'s security repositories qualify (for example noble-security or Debian-Security). Docker, Caddy and PostgreSQL repositories have no security channel, so their updates never count as security updates.';

function windowLabel(policy: HostUpdatesPolicy) {
  const w = policy.reboot_window;
  const days = w.weekdays?.length ? w.weekdays.map(day => weekdays[day]).join(', ') : 'Every day';
  return `${days} at ${String(w.hour).padStart(2, '0')}:00 UTC for ${w.duration_minutes >= 60 ? `${w.duration_minutes / 60} h` : `${w.duration_minutes} min`}`;
}

function DisruptiveList({ packages }: { packages: HostPackageUpgrade[] }) {
  return <ul className="host-disruptive">{packages.map(item => <li key={item.name}><code>{item.name}</code> {item.current ? `${item.current} → ` : 'new '}{item.candidate}{item.security && <Badge tone="warning">security</Badge>}<small>{item.effect}</small></li>)}</ul>;
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
  const run = async (scope: 'security' | 'all', include_disruptive = false) => { setStatus(await post<HostUpdatesStatus>(`${endpoint(serverID)}/run`, { scope, include_disruptive })); notify(scope === 'security' ? 'Installing security updates.' : include_disruptive ? 'Installing all pending updates, including disruptive ones.' : 'Installing pending updates; disruptive ones wait.'); };
  const reboot = async () => { setStatus(await post<HostUpdatesStatus>(`control/servers/${encodeURIComponent(serverID)}/host/reboot`, { confirm: true })); notify('Reboot requested. The server will be unavailable for a few minutes.'); };

  if (error && !status) return <Card className="form-card"><SectionHeader title="Operating system updates" /><ErrorBanner>{error}</ErrorBanner></Card>;
  if (!status || !policy) return <Card className="form-card"><SectionHeader title="Operating system updates" /><Loading label="Checking host updates…" /></Card>;
  if (!status.supported) return <Card className="form-card"><SectionHeader title="Operating system updates" /><Notice>{status.reason || 'Host updates are not managed on this server.'}</Notice></Card>;
  const busy = !!status.running;
  const disruptive = status.disruptive_packages || [];
  const deferred = status.policy.disruptive_in_window !== false;
  const windowNote = deferred && status.next_disruptive_window ? `They install in the maintenance window starting ${formatDate(status.next_disruptive_window)}, then the server reboots there if needed.` : 'They install only when you include them in a run below.';
  return <Card className="form-card" data-host-updates={serverID}>
    <SectionHeader title="Operating system updates" description={status.checked_at ? `Checked ${formatDate(status.checked_at)}. Package lists refresh daily on the server.` : undefined} actions={<Button size="sm" onClick={check} pending={checking} disabled={checking || busy}><RefreshCw size={15} />Check now</Button>} />
    {error && <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner>}
    {status.error && <Notice>{status.error}</Notice>}
    <div className="host-summary">
      <Badge tone={status.pending ? 'accent' : 'success'}>{status.pending ? `${status.pending} pending update${status.pending === 1 ? '' : 's'}` : 'Up to date'}</Badge>
      {status.security > 0 && <Badge tone="warning">{status.security} security</Badge>}
      {disruptive.length > 0 && <Badge tone="warning">{disruptive.length} disruptive</Badge>}
      {status.reboot_required && <Badge tone="warning">Reboot required</Badge>}
      {status.docker?.running && <Badge tone={status.docker.live_restore ? 'success' : 'warning'}>{status.docker.live_restore ? 'Docker live-restore on' : 'Docker live-restore off'}</Badge>}
      <Badge tone={status.unattended_enabled ? 'success' : 'neutral'}>{status.unattended_enabled ? 'Automatic security updates on' : status.unattended_installed ? 'Automatic updates off' : 'unattended-upgrades not installed'}</Badge>
      {busy && <LiveProgress compact label={`Running: ${jobLabels[status.running || ''] || status.running}`} startedAt={status.jobs?.find(job => job.status === 'running')?.started} />}
    </div>
    {status.reboot_required && <p className="muted-copy">{status.reboot_reason}{status.reboot_packages?.length ? ` (${status.reboot_packages.join(', ')})` : ''}.</p>}
    {status.reboot_deferred && <Notice>{status.reboot_deferred}</Notice>}
    {status.unattended_last_run && <p className="muted-copy">Unattended upgrades last ran {formatDate(status.unattended_last_run)}.</p>}
    {status.docker?.warning && <div className="alert alert-warning" role="status"><TriangleAlert size={16} /><span>{status.docker.warning}</span></div>}
    {disruptive.length > 0 && <div className="host-disruptive-block"><strong>Disruptive updates</strong><p className="muted-copy">These restart shared services or need a reboot. {windowNote}</p><DisruptiveList packages={disruptive} /></div>}
    {!!status.packages?.length && <details className="host-packages"><summary>Pending packages</summary><ul>{status.packages.map(item => <li key={item.name}><code>{item.name}</code> {item.current ? `${item.current} → ` : 'new '}{item.candidate}{item.security && <Badge tone="warning">security</Badge>}</li>)}</ul></details>}
    <div className="maintenance-actions">
      <ConfirmAction title="Install security updates now?" description={<>{securityRule} Disruptive security updates{disruptive.some(item => item.security) ? ` (${disruptive.filter(item => item.security).map(item => item.name).join(', ')})` : ''} are left for the maintenance window. A reboot may be required afterwards.</>} confirmLabel="Install security updates" onConfirm={() => run('security')}><Button variant="primary" disabled={busy}><ShieldAlert size={16} />Install security updates now</Button></ConfirmAction>
      <ConfirmAction title="Install all pending updates?" description={disruptive.length ? <>Every pending update is installed except these disruptive ones, which stay pending: {disruptive.map(item => item.name).join(', ')}. {windowNote}</> : 'Every pending package update is installed. None of them restarts shared services.'} confirmLabel="Install all updates" onConfirm={() => run('all')}><Button disabled={busy || !status.pending}><Download size={16} />Install all updates</Button></ConfirmAction>
      {disruptive.length > 0 && <ConfirmAction title="Install all updates, including disruptive ones?" description={`Installing these now interrupts hosted applications. ${disruptive.map(item => `${item.name} ${item.effect}`).join('. ')}. The run cannot be stopped once it starts.`} confirmLabel="Install including disruptive" destructive onConfirm={() => run('all', true)}><Button variant="danger" disabled={busy}><TriangleAlert size={16} />Include disruptive updates</Button></ConfirmAction>}
      <ConfirmAction title="Reboot this server now?" description="Every application on this server is unavailable until the host is back, usually a few minutes. The reboot is refused while deployments, backups or package updates are running." confirmLabel="Reboot now" destructive onConfirm={reboot}><Button variant="danger" disabled={busy}><Power size={16} />Reboot now</Button></ConfirmAction>
    </div>
    {!!status.components?.length && <div className="host-components">{status.components.map(item => <div key={item.name}><strong>{item.name}</strong><span>{item.source === 'absent' ? 'Not installed' : item.installed || 'Unknown version'}{item.upgrade_available && item.candidate ? ` → ${item.candidate}` : ''}</span><small>{item.source === 'binary' ? 'Standalone binary; package updates do not apply' : item.source === 'package' ? item.automatic === 'all' ? 'Updates install automatically' : item.automatic === 'security' ? 'Security updates install automatically' : 'Updates install manually' : ''}</small></div>)}</div>}
    <SectionHeader title="Update policy" description="Runs on the server itself, even while the control panel is offline." />
    <label className="check-row switch-row"><Switch aria-label="Automatic security updates" checked={policy.auto_security} onCheckedChange={auto_security => change({ auto_security, manage_package_sources: auto_security && policy.manage_package_sources })} /><span><strong>Automatic security updates</strong><small>Installs and enables unattended-upgrades for the distribution's security updates.</small></span></label>
    <label className="check-row switch-row"><Switch aria-label="Include official package repositories" checked={policy.manage_package_sources} disabled={!policy.auto_security} onCheckedChange={manage_package_sources => change({ manage_package_sources })} /><span><strong>Include official Caddy, PostgreSQL and Docker repositories</strong><small>{status.managed_origins?.length ? `Configured here: ${status.managed_origins.join('; ')}.` : 'None of these repositories is configured on this server.'}</small></span></label>
    <label className="check-row switch-row"><Switch aria-label="Apply disruptive updates in the window" checked={policy.disruptive_in_window !== false} onCheckedChange={disruptive_in_window => change({ disruptive_in_window })} /><span><strong>Apply disruptive updates in the maintenance window</strong><small>Docker, PostgreSQL, Redis, Caddy, kernel and systemd upgrades install only inside the window below, and automatic updates leave them alone. Off: they install only when requested.</small></span></label>
    <label className="check-row switch-row"><Switch aria-label="Reboot automatically" checked={policy.auto_reboot} onCheckedChange={auto_reboot => change({ auto_reboot })} /><span><strong>Reboot automatically when required</strong><small>Only inside the window below, at most once every six hours, and never while deployments or backups run.</small></span></label>
    <fieldset disabled={!policy.auto_reboot && policy.disruptive_in_window === false} className="host-window">
      <legend className="field-label">Maintenance window (UTC)</legend>
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
