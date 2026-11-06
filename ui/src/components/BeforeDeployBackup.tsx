import { useCallback, useEffect, useState } from 'react';
import { get, patch } from '../lib/api';
import { useToast } from './ToastProvider';
import { Button, Card, ErrorBanner, Field, Loading, SectionHeader } from './ui';

/**
 * Before-deploy backup policy of one database: what a deployment does about
 * its backup before changing anything. The operator chooses per database;
 * the default is a fresh verified backup every time, and a primary database
 * cannot choose scheduled backups only.
 */
export type BeforeDeployMode = 'always' | 'reuse' | 'never';
export interface BeforeDeployPolicy { mode?: BeforeDeployMode | ''; max_age_hours?: number }

export function beforeDeployLabel(policy?: BeforeDeployPolicy) {
  if (policy?.mode === 'reuse') return `Reuse a backup newer than ${policy.max_age_hours || 24} h`;
  if (policy?.mode === 'never') return 'Scheduled backups only';
  return 'Always a fresh backup';
}

export function BeforeDeployControl({ serverID, databaseID, policy, primary, disabled, onSaved }: {
  serverID: string; databaseID: string; policy?: BeforeDeployPolicy; primary: boolean; disabled?: boolean; onSaved?: () => Promise<void> | void;
}) {
  const [mode, setMode] = useState<BeforeDeployMode>(policy?.mode || 'always');
  const [hours, setHours] = useState(String(policy?.max_age_hours || 24));
  const [saving, setSaving] = useState(false);
  const notify = useToast();
  const save = async () => {
    const age = Number(hours);
    if (mode === 'reuse' && (!Number.isInteger(age) || age < 1 || age > 720)) { notify('Enter a maximum backup age between 1 and 720 hours.', true); return; }
    const body = mode === 'reuse' ? { mode, max_age_hours: age } : { mode };
    setSaving(true);
    try {
      await patch(`control/servers/${encodeURIComponent(serverID)}/databases/${encodeURIComponent(databaseID)}/backups/before-deploy`, body);
      await onSaved?.();
      notify(`Before-deploy backups saved for ${databaseID}.`);
    } catch (reason) {
      notify(reason instanceof Error ? reason.message : String(reason), true);
    } finally {
      setSaving(false);
    }
  };
  // Not a <form>: this control also sits inside the application settings form.
  return <div className="before-deploy" role="group" aria-label={`Before deploys for ${databaseID}`}>
    <div className="form-grid two">
      <Field label="Before deploys" hint={primary ? 'A primary database is always backed up before a deploy; it can reuse a recent verified backup.' : 'Scheduled backups continue whatever you choose here.'}>
        <select value={mode} disabled={disabled} onChange={event => setMode(event.target.value as BeforeDeployMode)}>
          <option value="always">Always take a fresh backup</option>
          <option value="reuse">Reuse a backup newer than…</option>
          <option value="never" disabled={primary}>{primary ? 'Scheduled backups only (not for a primary database)' : 'Scheduled backups only'}</option>
        </select>
      </Field>
      {mode === 'reuse' && <Field label="Maximum age (hours)" hint="A verified scheduled or manual backup newer than this counts as the pre-deployment backup; otherwise a fresh one is taken.">
        <input type="number" inputMode="numeric" min={1} max={720} step={1} required value={hours} disabled={disabled} onChange={event => setHours(event.target.value)} />
      </Field>}
    </div>
    <Button type="button" variant="primary" size="sm" disabled={saving || disabled} onClick={save}>{saving ? 'Saving…' : 'Save before-deploy policy'}</Button>
  </div>;
}

export interface BeforeDeployDatabase {
  app_id: string; server_id: string; ready?: boolean; retained?: boolean; operation?: string; engine?: string; primary?: boolean; before_deploy?: BeforeDeployPolicy;
  attachments?: Array<{ app_id: string; env_key?: string }>;
}

function databaseKind(database: BeforeDeployDatabase) {
  if (database.engine === 'clickhouse') return 'ClickHouse';
  if (database.engine === 'sqlite') return 'SQLite';
  return 'PostgreSQL';
}

/** The per-database "Before deploys" settings of one application. */
export function AppDatabasesBeforeDeploy({ appID }: { appID: string }) {
  const [databases, setDatabases] = useState<BeforeDeployDatabase[]>();
  const [error, setError] = useState('');
  const load = useCallback(async () => {
    try {
      const value = await get<{ databases?: BeforeDeployDatabase[] } | BeforeDeployDatabase[]>('control/databases');
      const list = Array.isArray(value) ? value : value.databases || [];
      setDatabases(list.filter(item => !item.retained && (item.app_id === appID || item.attachments?.some(attachment => attachment.app_id === appID))));
      setError('');
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
      setDatabases([]);
    }
  }, [appID]);
  useEffect(() => { void load(); }, [load]);
  return <Card className="form-card">
    <SectionHeader title="Before deploys" description="Choose, per database, what a deployment does before it changes anything: a fresh verified backup (the default), a recent verified backup, or scheduled backups only for secondary data." />
    {error && <ErrorBanner>{error}</ErrorBanner>}
    {!databases ? <Loading label="Loading databases…" /> : databases.length === 0 ? <p className="muted-copy">No managed database is attached to this application.</p> : <ul className="before-deploy-list">
      {databases.map(database => {
        const keys = (database.attachments || []).filter(attachment => attachment.app_id === appID).map(attachment => attachment.env_key).filter(Boolean);
        return <li key={`${database.server_id}-${database.app_id}`}>
          <div className="before-deploy-heading"><strong>{database.app_id}</strong><span>{database.primary ? 'Primary' : 'Secondary'} · {databaseKind(database)}{keys.length ? ` · ${keys.join(', ')}` : ''} · {database.server_id}</span></div>
          <BeforeDeployControl key={JSON.stringify(database.before_deploy)} serverID={database.server_id} databaseID={database.app_id} policy={database.before_deploy} primary={Boolean(database.primary)} disabled={Boolean(database.operation) || database.ready === false} onSaved={load} />
        </li>;
      })}
    </ul>}
  </Card>;
}
