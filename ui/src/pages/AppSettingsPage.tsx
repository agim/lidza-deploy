import { RetainedResources } from '../components/RetainedResources';
import { indexHref, navigate, resourceHref, withSticky } from '../lib/router';
import { NativeDataServices } from '../components/NativeDataServices';
import { AppPorts } from '../components/AppPorts';
import { RedirectDomains } from '../components/RedirectDomains';
import { RuntimeProfileField } from '../components/RuntimeProfileField';
import { ApplyDefaultsButton } from '../components/ApplyDefaults';
import { AppDatabasesBeforeDeploy } from '../components/BeforeDeployBackup';
import { MoveLockFieldset, useAppMoveLock } from '../components/MoveLock';
import { Input } from '../components/primitives/input';
import { Database, Eye, EyeOff, GitBranch, HardDrive, KeyRound, Plus, Save, X } from 'lucide-react';
import { type FormEvent, type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { APIError, get, patch, post } from '../lib/api';
import type { AppSettings, Deployment, DomainStatus, EnvironmentEntry } from '../lib/types';
import { restartOutcome, saveLabel, settingsChanges, settingsEffect, settingsSavedMessage, type SettingsApplyResult } from '../lib/settingsApply';
import { useToast } from '../components/ToastProvider';
import { AppDangerZone } from '../components/AppLifecycle';
import { ARCHIVE_RETENTION_DAYS } from '../lib/archive';
import { useWorkspace } from '../state/WorkspaceContext';
import type { Application } from '../lib/types';
import { ActionForm, Button, Card, ConfirmAction, ErrorBanner, Field, Loading, Notice, PromptDialog, SectionHeader } from '../components/ui';
import { Cluster } from '../components/layout';
import { CacheSettings } from './ResourceDetailPages';

// The application's settings, one tab each (docs/console-structure.md):
// Settings (source, build, ports, lifecycle, Danger zone), Environment,
// Domains, Database & cache and Backups. Each tab loads the settings itself
// and saves them with one PATCH; values another tab owns are sent unchanged.

type Action = 'keep' | 'set' | 'delete';
type Row = EnvironmentEntry & { rowID: string; action: Action; revealed?: boolean; saved: boolean };

/** A save's outcome shown under the form: what was applied, with the restart or deployment it started. */
type SaveNotice = { text: string; deployment?: string; link?: string };

/** useAppSettings loads an application's settings, saves them with optional environment changes and follows what the save applied. */
function useAppSettings(appID: string) {
  const [settings, setSettings] = useState<AppSettings>();
  const [loaded, setLoaded] = useState<AppSettings>();
  const [error, setError] = useState('');
  const [saving, setSaving] = useState(false);
  const [notice, setNotice] = useState<SaveNotice>();
  const [loadCount, setLoadCount] = useState(0);
  const [domainStatuses, setDomainStatuses] = useState<DomainStatus[]>([]);
  const notify = useToast();
  const move = useAppMoveLock(appID);
  const mounted = useRef(true);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  useEffect(() => {
    let active = true; let timer: ReturnType<typeof setInterval> | undefined;
    get<AppSettings>(`control/apps/${encodeURIComponent(appID)}/settings`).then(value => {
      if (!active) return;
      setSettings(value); setLoaded(value); setDomainStatuses(value.domain_statuses || []);
      if (value.additional_domains_supported) timer = setInterval(() => { get<AppSettings>(`control/apps/${encodeURIComponent(appID)}/settings`).then(next => { if (active) setDomainStatuses(next.domain_statuses || []); }).catch(() => {}); }, 10000);
    }).catch(reason => setError(reason instanceof Error ? reason.message : String(reason)));
    return () => { active = false; if (timer) clearInterval(timer); };
  }, [appID, loadCount]);
  const editable = Boolean(settings) && settings!.capabilities?.configure !== false && !move.locked;
  /** follow reports what a save applied and, for a restart, follows it until it finishes. */
  const follow = useCallback(async (result: SettingsApplyResult | undefined) => {
    const message = settingsSavedMessage(result);
    notify(message, Boolean(result?.error));
    const id = result?.deployment_id;
    const link = result?.applied === 'deploy' ? 'View deployment' : 'View restart';
    setNotice(result && result.applied !== 'immediate' ? { text: message, deployment: id, link } : undefined);
    if (!id || result?.applied !== 'restart') return;
    for (let attempt = 0; attempt < 400 && mounted.current; attempt++) {
      await new Promise(resolve => setTimeout(resolve, 1500));
      let current: Deployment;
      try { current = await get<Deployment>(`control/deployments/${encodeURIComponent(id)}`); } catch { continue; }
      if (['queued', 'building'].includes(current.status)) continue;
      const outcome = restartOutcome(current);
      notify(outcome, current.status !== 'live');
      if (mounted.current) setNotice({ text: outcome, deployment: id, link });
      return;
    }
  }, [notify]);
  const save = async (envChanges: Record<string, string | null> = {}, kindChanges: Record<string, string> = {}) => {
    if (!settings || settings.capabilities?.configure === false || move.locked) return;
    setSaving(true); setError(''); setNotice(undefined);
    try {
      const saved = await patch<{ apply?: SettingsApplyResult } | undefined>(`control/apps/${encodeURIComponent(appID)}/settings`, { branch: settings.branch, domain: settings.domain, ...(settings.stack_profiles_supported ? { runtime_profile: settings.runtime_profile || '' } : {}), ...(settings.additional_domains_supported ? { additional_domains: settings.additional_domains || [] } : {}), customer_hosts: Boolean(settings.customer_hosts), ...(settings.build_configuration_supported ? { build: settings.build || {} } : {}), ...(settings.runtime === 'astro' ? { astro: settings.astro || {} } : {}), backup_before_deploy: settings.backup_before_deploy !== false, env_changes: envChanges, env_kind_changes: kindChanges });
      setLoadCount(count => count + 1);
      void follow(saved?.apply);
    } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setSaving(false); }
  };
  return { settings, loaded, setSettings, error, setError, saving, save, follow, notice, editable, move, domainStatuses, reload: () => setLoadCount(count => count + 1) };
}
type SettingsState = ReturnType<typeof useAppSettings>;

/** SettingsForm: a tab's form with the shared loading, error and Save row. */
function SettingsForm({ id, state, children, submitLabel = 'Save changes', envKeys, onSubmit }: { id: string; state: SettingsState; children: ReactNode; submitLabel?: string; envKeys?: string[]; onSubmit?: () => unknown }) {
  const { settings, loaded, error, setError, saving, editable, move, save, notice } = state;
  const label = saveLabel(settingsEffect(settings?.apply_effects, settingsChanges(loaded, settings, envKeys)), submitLabel);
  if (!settings && !error) return <Loading label="Loading application settings…" />;
  if (!settings) return <ErrorBanner>{error}</ErrorBanner>;
  return <ActionForm id={id} className="settings-stack" onSubmit={async (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); await (onSubmit ? onSubmit() : save()); }}>
    {error && <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner>}
    {settings.capabilities?.reason && <p role="status">{settings.capabilities.reason}</p>}
    <MoveLockFieldset lock={move}>{children}</MoveLockFieldset>
    {notice && <Notice>{notice.text}{notice.deployment && <> <a href={withSticky(resourceHref('deployments', notice.deployment))}>{notice.link}</a></>}</Notice>}
    <Cluster><Button type="submit" variant="primary" disabled={saving || !editable} title={move.reason || undefined}><Save size={16} />{saving ? 'Saving…' : label}</Button></Cluster>
  </ActionForm>;
}

/** Settings tab: source and build, network ports, then the lifecycle Danger zone. */
export function AppSettingsTab({ appID }: { appID: string }) {
  const state = useAppSettings(appID);
  const { settings, setSettings, move, setError } = state;
  const [branches, setBranches] = useState<string[]>([]);
  const notify = useToast();
  const workspace = useWorkspaceIfPresent();
  const loadBranches = async () => { if (!settings) return; try { const values = await get<Array<{ name: string }>>(`control/github/branches?repository=${encodeURIComponent(settings.repository)}&installation=${settings.github_installation || 0}`); setBranches(values.map(value => value.name)); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } };
  return <div className="settings-stack">
    <SettingsForm id="settings-form" state={state}>{settings && <Card className="form-card"><SectionHeader title="Source & build" description="Where the code comes from and how it is built. Saving a change here deploys it." /><fieldset disabled={settings.capabilities?.configure === false}><div className="form-grid two">
      <Field label="Runtime"><Input value={settings.runtime === 'astro' ? 'Astro' : settings.runtime === 'rails' ? 'Ruby on Rails' : 'Lidza (Go)'} readOnly /></Field>
      {settings.build_configuration_supported && <Field label="Repository directory" hint="Saving a change deploys it."><Input value={settings.build?.source_directory || ''} onChange={event => setSettings(current => current ? { ...current, build: { ...current.build, source_directory: event.target.value } } : current)} placeholder="Repository root" /></Field>}
      {settings.runtime === 'astro' && <><Field label="Astro output"><select disabled={settings.capabilities?.runtime_mode === false} value={settings.astro?.mode || 'static'} onChange={event => setSettings(current => current ? { ...current, astro: { ...current.astro, mode: event.target.value as 'static' | 'node' } } : current)}><option value="static">Static website</option><option value="node">Standalone Node server</option></select></Field><Field label="Build output directory"><Input value={settings.astro?.output_directory || 'dist'} onChange={event => setSettings(current => current ? { ...current, astro: { ...current.astro, output_directory: event.target.value } } : current)} /></Field></>}
      <Field label="Branch" hint="Saving a new branch deploys it."><div className="input-action"><Input name="branch" value={settings.branch} onChange={event => setSettings(current => current ? { ...current, branch: event.target.value } : current)} required list="settings-branches" /><Button type="button" size="sm" onClick={loadBranches}><GitBranch size={15} />Load</Button></div><datalist id="settings-branches">{branches.map(branch => <option key={branch}>{branch}</option>)}</datalist></Field>
    </div>{settings.stack_profiles_supported && <RuntimeProfileField value={settings.runtime_profile || ''} disabled={Boolean(settings.runtime_deployed && !settings.runtime_profile)} allowSingle={!settings.runtime_deployed || !settings.runtime_profile} onChange={event => setSettings(current => current ? { ...current, runtime_profile: event.target.value } : current)} />}</fieldset></Card>}</SettingsForm>
    {settings?.public_ports_supported && <AppPorts appID={appID} />}
    {settings && <AppDangerZone app={dangerZoneApp(appID, settings, workspace?.apps?.find(item => item.id === appID), move.locked)} deployment={workspace?.latestDeployment?.(appID)} onChanged={async () => { state.reload(); await workspace?.refresh?.(); }} onDeleted={async () => { notify(`${appID} deleted. Restore it from Deleted apps within ${ARCHIVE_RETENTION_DAYS} days.`); navigate(indexHref('applications')); await workspace?.refresh?.(); }} />}
  </div>;
}

/** Domains tab: the primary and additional hostnames, customer hostnames, then redirect domains. */
export function AppDomainsTab({ appID }: { appID: string }) {
  const state = useAppSettings(appID);
  const { settings, setSettings, domainStatuses, move } = state;
  const primary = domainStatuses.find(item => item.domain === settings?.domain);
  return <div className="settings-stack">
    <SettingsForm id="domains-form" state={state} submitLabel="Save hostnames">{settings && <Card className="form-card"><SectionHeader title="Hostnames" description="Point each hostname’s DNS to this server; HTTPS is issued automatically. The primary hostname is the app’s canonical URL." /><fieldset disabled={settings.capabilities?.configure === false}>
      <Field label="Application hostname" hint="Enter a hostname without a scheme or path."><Input disabled={settings.capabilities?.routing === false} name="domain" value={settings.domain} onChange={event => setSettings(current => current ? { ...current, domain: event.target.value } : current)} required />{primary && <small className="field-hint" role="status">{primary.state === 'ready' ? 'HTTPS ready' : primary.message || (primary.state === 'requesting_ssl' ? 'Requesting HTTPS certificate' : 'Waiting for DNS verification')}{primary.checked && <> · Checked {new Date(primary.checked).toLocaleString()}</>}</small>}</Field>
      {settings.additional_domains_supported ? <>{(settings.additional_domains || []).map((domain, index) => <Field key={index} label={`Additional hostname ${index + 1}`}><div className="input-action"><Input disabled={settings.capabilities?.routing === false} name={`additional_domain_${index}`} aria-label={`Additional hostname ${index + 1}`} value={domain} placeholder="www.example.com" required maxLength={253} autoCapitalize="none" spellCheck={false} onChange={event => setSettings(current => current ? { ...current, additional_domains: (current.additional_domains || []).map((item, i) => i === index ? event.target.value.toLowerCase().trim() : item) } : current)} /><Button type="button" size="icon" disabled={settings.capabilities?.routing === false} aria-label={`Remove additional hostname ${index + 1}`} onClick={() => setSettings(current => current ? { ...current, additional_domains: (current.additional_domains || []).filter((_, i) => i !== index) } : current)}><X size={16} /></Button></div><small className="field-hint [overflow-wrap:anywhere]">{domainStatuses.find(item => item.domain === domain)?.state === 'ready' ? 'HTTPS ready' : domainStatuses.find(item => item.domain === domain)?.message || (domainStatuses.some(item => item.domain === domain) ? 'Waiting for DNS and HTTPS' : 'DNS and HTTPS checked after saving')}</small></Field>)}<Cluster><Button type="button" disabled={settings.capabilities?.routing === false || (settings.additional_domains || []).length >= 20} onClick={() => setSettings(current => current ? { ...current, additional_domains: [...(current.additional_domains || []), ''] } : current)}><Plus size={16} />Add hostname</Button></Cluster></> : <p className="muted-copy">Update the hosting agent to manage additional hostnames.</p>}
      {(!settings.runtime || settings.runtime === 'go') && <label className="check-row"><Input name="customer_hosts" type="checkbox" checked={Boolean(settings.customer_hosts)} onChange={event => setSettings(current => current ? { ...current, customer_hosts: event.target.checked } : current)} /><span><strong>Serve approved customer hostnames</strong><small>Use only when the application validates hostnames through Līdza.</small></span></label>}
    </fieldset></Card>}</SettingsForm>
    {settings && <RedirectDomains appID={appID} disabled={settings.capabilities?.configure === false || move.locked} />}
  </div>;
}

/** Backups tab: deployment safety, the before-deploy policy of each database, and where their backups are. */
export function AppBackupsTab({ appID }: { appID: string }) {
  const state = useAppSettings(appID);
  const { settings, setSettings, move } = state;
  const [databases, setDatabases] = useState<Array<{ app_id: string; server_id: string; engine?: string; backups?: unknown[]; attachments?: Array<{ app_id: string }> }>>();
  useEffect(() => { get<{ databases?: typeof databases } | NonNullable<typeof databases>>('control/databases').then(value => setDatabases((Array.isArray(value) ? value : value.databases || []).filter(item => item.app_id === appID || item.attachments?.some(attachment => attachment.app_id === appID)))).catch(() => setDatabases([])); }, [appID]);
  return <div className="settings-stack">
    {settings && !settings.native && <SettingsForm id="backups-form" state={state}><Card className="form-card"><SectionHeader title="Deployment safety" description="Protect application data before activating a release." /><label className="check-row"><Input disabled={settings.capabilities?.configure === false} name="backup_before_deploy" type="checkbox" checked={settings.backup_before_deploy !== false} onChange={event => setSettings(current => current ? { ...current, backup_before_deploy: event.target.checked } : current)} /><span><strong>Back up attached databases before deployments and reloads</strong><small>A failed backup stops activation and leaves the healthy release serving.</small></span></label></Card></SettingsForm>}
    {settings?.native && <Card className="form-card"><SectionHeader title="Deployment safety" description="Existing database backups and schedules remain under the current host setup." /></Card>}
    <MoveLockFieldset lock={move}><AppDatabasesBeforeDeploy appID={appID} /></MoveLockFieldset>
    <Card><SectionHeader title="Backup history" description="Backups belong to each database; open one to download, restore or change its schedule." />{!databases ? <Loading label="Loading databases…" /> : databases.length ? <ul className="plain-list link-list">{databases.map(database => <li key={`${database.server_id}/${database.app_id}`}><a href={withSticky(resourceHref('databases', database.app_id, 'backups'))}>{database.app_id}</a><span className="muted-copy">{database.engine === 'clickhouse' ? 'ClickHouse' : database.engine === 'sqlite' ? 'SQLite' : 'PostgreSQL'} · {database.backups?.length || 0} backup{database.backups?.length === 1 ? '' : 's'}</span></li>)}</ul> : <p className="muted-copy">No database is attached to {appID}.</p>}</Card>
  </div>;
}

/** Database & cache tab: the attached database and cache (container apps) or the server's data services (native apps), then retained data. */
export function AppDataTab({ appID }: { appID: string }) {
  const { settings, error, move } = useAppSettings(appID);
  if (!settings && !error) return <Loading label="Loading data services…" />;
  if (!settings) return <ErrorBanner>{error}</ErrorBanner>;
  if (settings.native) return <div className="settings-stack"><NativeDataServices appID={appID} /><RetainedResources appID={appID} /></div>;
  const attached = settings.database_bindings?.DATABASE_URL;
  return <div className="settings-stack">
    <Card className="resource-setting"><Database size={23} /><div><h2>Database</h2><p>{attached ? `Attached: ${attached}` : settings.env_keys?.includes('DATABASE_URL') ? 'DATABASE_URL is set manually.' : 'No primary database attached.'}</p></div>{attached && <a className="button button-secondary button-md" href={withSticky(resourceHref('databases', attached))}>Open database</a>}</Card>
    {settings.capabilities?.resources === false
      ? <Card className="resource-setting"><HardDrive size={23} /><div><h2>Cache</h2><p>{settings.env_keys?.includes('REDIS_URL') ? 'Uses the REDIS_URL set in Environment.' : 'Set REDIS_URL in Environment to use an existing Redis or Valkey.'}</p></div></Card>
      : <CacheSettings appID={appID} disabled={settings.capabilities?.configure === false || move.locked} lock={move} />}
    <RetainedResources appID={appID} />
  </div>;
}

/** Environment tab: the application's variables; credentials stay masked until verified. */
export function AppEnvironmentTab({ appID }: { appID: string }) {
  const state = useAppSettings(appID);
  const { settings, setSettings, saving, save } = state;
  const [rows, setRows] = useState<Row[]>([]);
  const [revealTarget, setRevealTarget] = useState<Row>();
  const [verificationCode, setVerificationCode] = useState('');
  const notify = useToast();
  useEffect(() => { if (settings) setRows((settings.environment || (settings.env_keys || []).map(key => ({ key }))).map((entry, index) => ({ ...entry, rowID: `${entry.key}-${index}`, action: 'keep', saved: true }))); }, [settings]);
  const updateRow = (id: string, values: Partial<Row>) => setRows(current => current.map(row => row.rowID === id ? { ...row, ...values } : row));
  const addRow = () => setRows(current => [...current, { key: '', kind: 'credential', rowID: crypto.randomUUID(), action: 'set', saved: false }]);
  const reveal = async (row: Row) => {
    if (row.revealed) { updateRow(row.rowID, { value: '', revealed: false, action: 'keep' }); return; }
    try {
      const result = await post<{ value: string }>(`control/apps/${encodeURIComponent(appID)}/environment/${encodeURIComponent(row.key)}/reveal`);
      updateRow(row.rowID, { value: result.value, revealed: true });
    } catch (reason) {
      if (reason instanceof APIError && reason.code === 'step_up_required') { setVerificationCode(''); setRevealTarget(row); }
      else state.setError(reason instanceof Error ? reason.message : String(reason));
    }
  };
  const verifyAndReveal = async (code: string) => {
    if (!revealTarget) return;
    await post('v1/auth/step-up', { code, purpose: 'credential.reveal', target: `${appID}/${revealTarget.key}` });
    const result = await post<{ value: string }>(`control/apps/${encodeURIComponent(appID)}/environment/${encodeURIComponent(revealTarget.key)}/reveal`);
    updateRow(revealTarget.rowID, { value: result.value, revealed: true });
  };
  const submit = async () => {
    const envChanges: Record<string, string | null> = {}, kindChanges: Record<string, string> = {}, names = new Set<string>();
    try {
      for (const row of rows) {
        const key = row.key.trim(); if (!key || row.action === 'keep') continue;
        if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) throw new Error(`${key || 'Variable'} is not a valid environment variable name.`);
        if (names.has(key)) throw new Error(`${key} appears more than once.`); names.add(key);
        envChanges[key] = row.action === 'delete' ? null : row.value || '';
        if (row.action !== 'delete' && row.kind && row.kind !== 'auto') kindChanges[key] = row.kind;
      }
    } catch (reason) { state.setError(reason instanceof Error ? reason.message : String(reason)); return; }
    await save(envChanges, kindChanges);
  };
  const generateMasterKey = async () => {
    const entry = await post<EnvironmentEntry & { apply?: SettingsApplyResult }>(`control/apps/${encodeURIComponent(appID)}/master-key`);
    setSettings(current => current ? { ...current, env_keys: [...new Set([...(current.env_keys || []), entry.key])], environment: [...(current.environment || []).filter(row => row.key !== entry.key), entry], env_sources: { ...current.env_sources, [entry.key]: entry.source || 'built-in' } } : current);
    void state.follow(entry.apply ? { ...entry.apply, message: entry.apply.message.replace(/^Settings saved\./, 'Master key generated and saved.') } : { applied: 'immediate', message: 'Master key generated and saved.' });
  };
  const pendingKeys = rows.filter(row => row.action !== 'keep' && row.key.trim()).map(row => row.key.trim());
  const hasMasterKey = Boolean(settings?.environment?.find(row => row.key === 'LIDZA_MASTER_KEY')?.length);
  const masterKeyDraft = rows.some(row => row.key === 'LIDZA_MASTER_KEY' && Boolean(row.value));
  const managed = useMemo(() => new Set([...(settings?.readonly_env_keys || []), ...Object.keys(settings?.database_bindings || {}), ...(settings?.cache?.managed ? ['CACHE_URL'] : []), ...(settings?.persistent_storage ? ['STORAGE_DIR'] : [])]), [settings]);
  return <>
    <SettingsForm id="environment-form" state={state} submitLabel="Save variables" envKeys={pendingKeys} onSubmit={submit}>{settings && <Card className="form-card env-editor" id="settings-env-editor"><SectionHeader title="Environment variables" description={settings.native ? 'Saving restarts the application with its variables (a static site deploys them). Credentials remain masked; hosting variables are managed by the server.' : 'Saving restarts the application with its variables. Settings are visible when editing; credentials remain masked and require fresh two-factor verification to reveal.'} actions={<div className="section-actions"><ApplyDefaultsButton appID={appID} runtime={settings.defaults_runtime} presentKeys={settings.env_keys || []} disabled={saving || settings.capabilities?.configure === false || settings.capabilities?.environment === false} dirty={rows.some(row => row.action !== 'keep')} onApplied={apply => { state.reload(); void state.follow(apply); }} />{settings.master_key_generation_supported && !hasMasterKey && <ConfirmAction title="Generate application master key" description="This saves a new key and restarts the application with it. If this installation already has encrypted credentials or data, cancel and import its original key instead." confirmLabel="Generate & save key" onConfirm={generateMasterKey}><Button type="button" disabled={saving || masterKeyDraft}><KeyRound size={16} />Generate master key</Button></ConfirmAction>}<Button type="button" disabled={settings.capabilities?.environment === false} onClick={addRow}><Plus size={16} />Add variable</Button></div>} />
      <fieldset disabled={settings.capabilities?.environment === false}><div className="env-list">{rows.length === 0 && <p className="muted-copy">No application-specific variables are saved. Application defaults are copied only on the first deployment.</p>}{rows.map(row => { const saved = row.saved; const locked = managed.has(row.key); const credentialState = row.kind === 'credential' && saved ? `${row.preview || 'Stored securely'}${row.length ? ` · ${row.length} characters` : ''}` : ''; return <div className="env-row" key={row.rowID}>
        <Field label="Name" hint={settings.env_sources?.[row.key] === 'workspace' ? 'Application default' : settings.env_sources?.[row.key] === 'built-in' ? 'Built-in default' : saved ? 'Application setting' : 'New variable'}><Input className="env-name" name="application-environment-key" form="environment-editor-controls" value={row.key} readOnly={saved} autoComplete="off" spellCheck={false} placeholder="VARIABLE_NAME" onChange={event => updateRow(row.rowID, { key: event.target.value })} /></Field>
        <Field label="Type"><select className="env-kind" value={row.kind || 'auto'} disabled={locked} onChange={event => updateRow(row.rowID, { kind: event.target.value as Row['kind'], action: saved ? row.action : 'set' })}><option value="auto">Auto detect</option><option value="setting">Setting</option><option value="credential">Credential</option></select></Field>
        <Field label="Value"><div className="env-value-control"><Input className="env-value" name="application-environment-content" form="environment-editor-controls" type={row.kind === 'setting' || row.revealed ? 'text' : 'password'} value={row.value || ''} disabled={locked || row.action === 'delete'} autoComplete={row.kind === 'setting' ? 'off' : 'new-password'} spellCheck={false} placeholder={locked ? 'Managed by hosting configuration' : credentialState || (saved ? 'Saved value stays unchanged' : 'Enter value')} title={locked ? 'Managed by hosting configuration' : credentialState || undefined} onChange={event => updateRow(row.rowID, { value: event.target.value, action: 'set' })} />{settings.capabilities?.environment !== false && row.kind === 'credential' && saved && !locked && <Button className="env-reveal" type="button" size="icon" variant="ghost" aria-label={row.revealed ? 'Hide credential' : 'Reveal credential'} title={row.revealed ? 'Hide credential' : 'Reveal credential'} onClick={() => reveal(row)}>{row.revealed ? <EyeOff size={17} /> : <Eye size={17} />}</Button>}</div></Field>
        <Field label="Action"><select className="env-action" value={row.action} disabled={locked} onChange={event => updateRow(row.rowID, { action: event.target.value as Action, value: event.target.value === 'set' ? row.value : '' })}>{saved ? <><option value="keep">Keep saved value</option><option value="set">Replace value</option><option value="delete">Delete variable</option></> : <><option value="set">Set value</option><option value="delete">Discard row</option></>}</select></Field>
        {!saved && <Button type="button" size="icon" variant="ghost" aria-label="Remove row" onClick={() => setRows(current => current.filter(item => item.rowID !== row.rowID))}><X size={17} /></Button>}
      </div>; })}</div></fieldset><div className="env-help"><KeyRound size={18} /><p>{settings.capabilities?.environment === false ? 'Settings come from the existing host configuration. Credentials remain masked.' : 'Credentials are encrypted on the hosting agent. Their saved values are never returned in normal page data.'}</p></div></Card>}</SettingsForm>
    {/* The environment rows name this form as their owner so they stay out of the settings form's submission; it exists so password fields have a form, and Enter in a row never submits. */}
    <form id="environment-editor-controls" hidden aria-hidden="true" onSubmit={event => event.preventDefault()} />
    <PromptDialog open={Boolean(revealTarget)} title="Verify to reveal credential" description={<>Enter a fresh authenticator or recovery code to reveal <strong>{revealTarget?.key}</strong>. This verification is used only for this protected action.</>} label="Authenticator or recovery code" value={verificationCode} submitLabel="Verify & reveal" inputProps={{ type: 'password', autoComplete: 'one-time-code', spellCheck: false }} onValueChange={setVerificationCode} onOpenChange={open => { if (!open) { setRevealTarget(undefined); setVerificationCode(''); } }} onSubmit={verifyAndReveal} />
  </>;
}

/** dangerZoneApp is the application the Danger zone acts on: the panel's record (with its move), its state from the settings. */
function dangerZoneApp(appID: string, settings: AppSettings, listed: Application | undefined, locked: boolean): Application {
  const base: Application = listed || { ...settings, id: appID };
  return { ...base, stopped: settings.stopped ?? base.stopped, native: base.native || (settings.native as Application['native']), management_capabilities: base.management_capabilities || (settings.native ? settings.capabilities : undefined),
    move: base.move || (locked ? { id: '', state: 'syncing', source_server: '', target_server: '', locked: true } : undefined) };
}

/** useWorkspaceIfPresent is the workspace, or null where the page renders without one. */
function useWorkspaceIfPresent() {
  try { return useWorkspace(); } catch { return null; }
}
