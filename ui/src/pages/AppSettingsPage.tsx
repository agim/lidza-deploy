import { RuntimeProfileField } from '../components/RuntimeProfileField';
import { Input } from '../components/primitives/input';
import { Database, Eye, EyeOff, GitBranch, HardDrive, KeyRound, Plus, Save, Trash2, X } from 'lucide-react';
import { type FormEvent, useEffect, useMemo, useState } from 'react';
import { APIError, get, patch, post } from '../lib/api';
import type { AppSettings, DomainStatus, EnvironmentEntry } from '../lib/types';
import { useToast } from '../components/ToastProvider';
import { ActionForm, Badge, Button, Card, ConfirmAction, ErrorBanner, Field, Loading, PageHeader, PromptDialog, SectionHeader, Tabs } from '../components/ui';

type Action = 'keep' | 'set' | 'delete';
type Row = EnvironmentEntry & { rowID: string; action: Action; revealed?: boolean; saved: boolean };

export function AppSettingsPage({ appID }: { appID: string }) {
  const [settings, setSettings] = useState<AppSettings>();
  const [rows, setRows] = useState<Row[]>([]);
  const [tab, setTab] = useState('general');
  const [error, setError] = useState('');
  const [saving, setSaving] = useState(false);
  const [branches, setBranches] = useState<string[]>([]);
 const [domainStatuses,setDomainStatuses] = useState<DomainStatus[]>([]);
  const [revealTarget, setRevealTarget] = useState<Row>();
  const [verificationCode, setVerificationCode] = useState('');
  const notify = useToast();
  useEffect(() => { let active = true; let timer: ReturnType<typeof setInterval>; get<AppSettings>(`control/apps/${encodeURIComponent(appID)}/settings`).then(value => { if (!active) return; setSettings(value); setDomainStatuses(value.domain_statuses||[]);
 if(value.additional_domains_supported) timer=setInterval(()=>{get<AppSettings>(`control/apps/${encodeURIComponent(appID)}/settings`).then(next=>{if(active)setDomainStatuses(next.domain_statuses||[])}).catch(()=>{});},10000);
 setRows((value.environment || (value.env_keys || []).map(key => ({ key }))).map((entry, index) => ({ ...entry, rowID: `${entry.key}-${index}`, action: 'keep', saved: true }))); }).catch(reason => setError(reason instanceof Error ? reason.message : String(reason))); return () => { active = false; clearInterval(timer); }; }, [appID]);
  const updateRow = (id: string, values: Partial<Row>) => setRows(current => current.map(row => row.rowID === id ? { ...row, ...values } : row));
  const addRow = () => setRows(current => [...current, { key: '', kind: 'credential', rowID: crypto.randomUUID(), action: 'set', saved: false }]);
  const reveal = async (row: Row) => {
    if (row.revealed) { updateRow(row.rowID, { value: '', revealed: false, action: 'keep' }); return; }
    try {
      const result = await post<{ value: string }>(`control/apps/${encodeURIComponent(appID)}/environment/${encodeURIComponent(row.key)}/reveal`);
      updateRow(row.rowID, { value: result.value, revealed: true });
    } catch (reason) {
      if (reason instanceof APIError && reason.code === 'step_up_required') {
        setVerificationCode('');
        setRevealTarget(row);
      } else setError(reason instanceof Error ? reason.message : String(reason));
    }
  };
  const verifyAndReveal = async (code: string) => {
    if (!revealTarget) return;
    await post('v1/auth/step-up', { code, purpose: 'credential.reveal', target: `${appID}/${revealTarget.key}` });
    const result = await post<{ value: string }>(`control/apps/${encodeURIComponent(appID)}/environment/${encodeURIComponent(revealTarget.key)}/reveal`);
    updateRow(revealTarget.rowID, { value: result.value, revealed: true });
  };
  const save = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault(); if (!settings) return; setSaving(true); setError('');
    try {
      const envChanges: Record<string, string | null> = {}, kindChanges: Record<string, string> = {}, names = new Set<string>();
      for (const row of rows) {
        const key = row.key.trim(); if (!key || row.action === 'keep') continue;
        if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) throw new Error(`${key || 'Variable'} is not a valid environment variable name.`);
        if (names.has(key)) throw new Error(`${key} appears more than once.`); names.add(key);
        envChanges[key] = row.action === 'delete' ? null : row.value || '';
        if (row.action !== 'delete' && row.kind && row.kind !== 'auto') kindChanges[key] = row.kind;
      }
      await patch(`control/apps/${encodeURIComponent(appID)}/settings`, { branch: settings.branch, domain: settings.domain, ...(settings.stack_profiles_supported ? {runtime_profile:settings.runtime_profile || ""}:{}), ...(settings.additional_domains_supported ? {additional_domains: settings.additional_domains || []} : {}), customer_hosts: Boolean(settings.customer_hosts), backup_before_deploy: settings.backup_before_deploy !== false, env_changes: envChanges, env_kind_changes: kindChanges });
      notify('Settings saved.');
      location.hash = 'applications';
    } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } finally { setSaving(false); }
  };
  const generateMasterKey = async () => {
    const entry = await post<EnvironmentEntry>(`control/apps/${encodeURIComponent(appID)}/master-key`);
    setSettings(current => current ? { ...current, env_keys: [...new Set([...(current.env_keys || []), entry.key])], environment: [...(current.environment || []).filter(row => row.key !== entry.key), entry], env_sources: { ...current.env_sources, [entry.key]: entry.source || 'built-in' } } : current);
    setRows(current => [...current.filter(row => row.key !== entry.key), { ...entry, rowID: crypto.randomUUID(), action: 'keep', saved: true }]);
    notify('Master key generated and saved. It will be used on the next deployment or reload.');
  };
  const hasMasterKey = Boolean(settings?.environment?.find(row => row.key === 'LIDZA_MASTER_KEY')?.length);
  const masterKeyDraft = rows.some(row => row.key === 'LIDZA_MASTER_KEY' && Boolean(row.value));
  const loadBranches = async () => { if (!settings) return; try { const values = await get<Array<{ name: string }>>(`control/github/branches?repository=${encodeURIComponent(settings.repository)}&installation=${settings.github_installation || 0}`); setBranches(values.map(value => value.name)); } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); } };
  const managed = useMemo(() => new Set([...Object.keys(settings?.database_bindings || {}), ...(settings?.cache?.managed ? ['CACHE_URL'] : []), ...(settings?.persistent_storage ? ['STORAGE_DIR'] : [])]), [settings]);
  if (!settings && !error) return <Loading overlay label="Loading application settings…" />;
  if (!settings) return <><PageHeader title={`${appID} settings`} back="#applications" /><ErrorBanner>{error}</ErrorBanner></>;
  return <><ActionForm id="settings-form" onSubmit={save}>
    <PageHeader eyebrow="Application settings" title={appID} description={`${settings.repository} · ${settings.server_id}`} back="#applications" actions={<div className="sticky-actions"><Button type="button" onClick={() => { location.hash = 'applications'; }}>Cancel</Button><Button type="submit" variant="primary" disabled={saving}>{saving ? 'Saving…' : <><Save size={16} />Save changes</>}</Button></div>} />
    {error && <ErrorBanner onDismiss={() => setError('')}>{error}</ErrorBanner>}
    <Tabs active={tab} onChange={setTab} items={[{ id: 'general', label: 'General' }, { id: 'environment', label: 'Environment' }, { id: 'resources', label: 'Data & storage' }]}>
    {tab === 'general' && <div className="settings-stack"><Card className="form-card"><SectionHeader title="Source & routing" description="Code source and the hostnames served by this application." /><div className="form-grid two"><Field label="Branch" hint="Branch changes apply to the next deployment."><div className="input-action"><Input name="branch" value={settings.branch} onChange={event => setSettings(current => current ? { ...current, branch: event.target.value } : current)} required list="settings-branches" /><Button type="button" size="sm" onClick={loadBranches}><GitBranch size={15} />Load</Button></div><datalist id="settings-branches">{branches.map(branch => <option key={branch}>{branch}</option>)}</datalist></Field><Field label="Application hostname" hint="Enter a hostname without a scheme or path."><Input name="domain" value={settings.domain} onChange={event => setSettings(current => current ? { ...current, domain: event.target.value } : current)} required /></Field></div><div className="settings-stack">{settings.stack_profiles_supported && <RuntimeProfileField value={settings.runtime_profile || ""} disabled={Boolean(settings.runtime_deployed && !settings.runtime_profile)} allowSingle={!settings.runtime_deployed || !settings.runtime_profile} onChange={event=>setSettings(current=>current ? {...current,runtime_profile:event.target.value}:current)} />}<SectionHeader title="Additional hostnames" description="Serve this app on more domains. Point each hostname’s DNS to this server; HTTPS is issued automatically. The primary hostname remains the app’s canonical URL." />{settings.additional_domains_supported ? <>{(settings.additional_domains || []).map((domain,index)=><Field key={index} label={`Additional hostname ${index+1}`}><div className="input-action"><Input name={`additional_domain_${index}`} aria-label={`Additional hostname ${index+1}`} value={domain} placeholder="www.example.com" required maxLength={253} autoCapitalize="none" spellCheck={false} onChange={event=>setSettings(current=>current ? {...current,additional_domains:(current.additional_domains||[]).map((item,i)=>i===index?event.target.value.toLowerCase().trim():item)}:current)} /><Button type="button" size="icon" aria-label={`Remove additional hostname ${index+1}`} onClick={()=>setSettings(current=>current?{...current,additional_domains:(current.additional_domains||[]).filter((_,i)=>i!==index)}:current)}><X size={16}/></Button></div><small className="field-hint [overflow-wrap:anywhere]">{domainStatuses.find(item=>item.domain===domain)?.state==='ready'?'HTTPS ready':domainStatuses.find(item=>item.domain===domain)?.message || (domainStatuses.some(item=>item.domain===domain)?'Waiting for DNS and HTTPS':'DNS and HTTPS checked after saving')}</small></Field>)}<Button type="button" disabled={(settings.additional_domains||[]).length>=20} onClick={()=>setSettings(current=>current?{...current,additional_domains:[...(current.additional_domains||[]),'']}:current)}><Plus size={16}/>Add hostname</Button></>:<p className="muted-copy">Update the hosting agent to manage additional hostnames.</p>}</div><label className="check-row"><Input name="customer_hosts" type="checkbox" checked={Boolean(settings.customer_hosts)} onChange={event => setSettings(current => current ? { ...current, customer_hosts: event.target.checked } : current)} /><span><strong>Serve approved customer hostnames</strong><small>Use only when the application validates hostnames through Līdza.</small></span></label></Card>
      <Card className="form-card"><SectionHeader title="Deployment safety" description="Protect application data before a release changes the running container." /><label className="check-row"><Input name="backup_before_deploy" type="checkbox" checked={settings.backup_before_deploy !== false} onChange={event => setSettings(current => current ? { ...current, backup_before_deploy: event.target.checked } : current)} /><span><strong>Back up attached databases before deployments and reloads</strong><small>A failed backup stops activation and leaves the healthy release serving.</small></span></label></Card></div>}
    {tab === 'environment' && <Card className="form-card env-editor" id="settings-env-editor"><SectionHeader title="Environment variables" description="Settings are visible when editing. Credentials remain masked and require fresh two-factor verification to reveal." actions={<div className="section-actions">{settings.master_key_generation_supported && !hasMasterKey && <ConfirmAction title="Generate application master key" description="This saves a new key for the next deployment or reload. If this installation already has encrypted credentials or data, cancel and import its original key instead." confirmLabel="Generate & save key" onConfirm={generateMasterKey}><Button type="button" disabled={saving || masterKeyDraft}><KeyRound size={16} />Generate master key</Button></ConfirmAction>}<Button type="button" onClick={addRow}><Plus size={16} />Add variable</Button></div>} />
      <div className="env-list">{rows.length === 0 && <p className="muted-copy">No application-specific variables are saved. Workspace defaults are copied only on the first deployment.</p>}{rows.map(row => { const saved = row.saved; const locked = managed.has(row.key); const credentialState = row.kind === 'credential' && saved ? `${row.preview || 'Stored securely'}${row.length ? ` · ${row.length} characters` : ''}` : ''; return <div className="env-row" key={row.rowID}>
        <Field label="Name" hint={settings.env_sources?.[row.key] === 'workspace' ? 'Workspace default' : settings.env_sources?.[row.key] === 'built-in' ? 'Built-in default' : saved ? 'Application setting' : 'New variable'}><Input className="env-name" name="application-environment-key" form="environment-editor-controls" value={row.key} readOnly={saved} autoComplete="off" spellCheck={false} placeholder="VARIABLE_NAME" onChange={event => updateRow(row.rowID, { key: event.target.value })} /></Field>
        <Field label="Type"><select className="env-kind" value={row.kind || 'auto'} disabled={locked} onChange={event => updateRow(row.rowID, { kind: event.target.value as Row['kind'], action: saved ? row.action : 'set' })}><option value="auto">Auto detect</option><option value="setting">Setting</option><option value="credential">Credential</option></select></Field>
        <Field label="Value"><div className="env-value-control"><Input className="env-value" name="application-environment-content" form="environment-editor-controls" type={row.kind === 'setting' || row.revealed ? 'text' : 'password'} value={row.value || ''} disabled={locked || row.action === 'delete'} autoComplete={row.kind === 'setting' ? 'off' : 'new-password'} spellCheck={false} placeholder={locked ? 'Managed by attached service' : credentialState || (saved ? 'Saved value stays unchanged' : 'Enter value')} title={locked ? 'Managed by the attached service' : credentialState || undefined} onChange={event => updateRow(row.rowID, { value: event.target.value, action: 'set' })} />{row.kind === 'credential' && saved && !locked && <Button className="env-reveal" type="button" size="icon" variant="ghost" aria-label={row.revealed ? 'Hide credential' : 'Reveal credential'} title={row.revealed ? 'Hide credential' : 'Reveal credential'} onClick={() => reveal(row)}>{row.revealed ? <EyeOff size={17} /> : <Eye size={17} />}</Button>}</div></Field>
        <Field label="Action"><select className="env-action" value={row.action} disabled={locked} onChange={event => updateRow(row.rowID, { action: event.target.value as Action, value: event.target.value === 'set' ? row.value : '' })}>{saved ? <><option value="keep">Keep saved value</option><option value="set">Replace value</option><option value="delete">Delete variable</option></> : <><option value="set">Set value</option><option value="delete">Discard row</option></>}</select></Field>
        {!saved && <Button type="button" size="icon" variant="ghost" aria-label="Remove row" onClick={() => setRows(current => current.filter(item => item.rowID !== row.rowID))}><X size={17} /></Button>}
      </div>; })}</div><div className="env-help"><KeyRound size={18} /><p>Credentials are encrypted on the hosting agent. Their saved values are never returned in normal page data.</p></div></Card>}
    {tab === 'resources' && <div className="resource-settings-grid"><Card className="resource-setting"><Database size={23} /><div><h2>Database</h2><p>{settings.database_bindings?.DATABASE_URL ? `Attached: ${settings.database_bindings.DATABASE_URL}` : settings.env_keys?.includes('DATABASE_URL') ? 'DATABASE_URL is set manually.' : 'No primary database attached.'}</p></div><Button type="button" onClick={() => { location.hash = `page/database/${encodeURIComponent(appID)}`; }}>Manage database</Button></Card><Card className="resource-setting"><HardDrive size={23} /><div><h2>Cache</h2><p>{settings.cache?.managed ? 'Private managed Valkey is attached.' : 'Automatic managed cache or external Redis/Valkey.'}</p></div><Button type="button" onClick={() => { location.hash = `page/cache/${encodeURIComponent(appID)}`; }}>Manage cache</Button></Card></div>}
    </Tabs>
  </ActionForm><PromptDialog open={Boolean(revealTarget)} title="Verify to reveal credential" description={<>Enter a fresh authenticator or recovery code to reveal <strong>{revealTarget?.key}</strong>. This verification is used only for this protected action.</>} label="Authenticator or recovery code" value={verificationCode} submitLabel="Verify & reveal" inputProps={{ type: 'password', autoComplete: 'one-time-code', spellCheck: false }} onValueChange={setVerificationCode} onOpenChange={open => { if (!open) { setRevealTarget(undefined); setVerificationCode(''); } }} onSubmit={verifyAndReveal} /></>;
}
