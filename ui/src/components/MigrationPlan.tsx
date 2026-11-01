import { useState } from 'react';
import { get } from '../lib/api';
import { ErrorBanner } from './ui';

type Plan = {
  checked: string;
  commit?: string;
  domains: string[];
  persistent_files: boolean;
  cache_mode?: string;
  databases: Array<{ id: string; mode: string; shared_with: string[] }>;
  tasks: string[];
  blockers: string[];
  steps: string[];
};

export function MigrationPlan({ appID }: { appID: string }) {
  const [plan, setPlan] = useState<Plan>();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  return <details className="migration-plan" onToggle={async event => {
    if (!event.currentTarget.open) return;
    setBusy(true); setError(''); setPlan(undefined);
    try { setPlan(await get<Plan>(`control/apps/${encodeURIComponent(appID)}/migration-plan`)); }
    catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)); }
    finally { setBusy(false); }
  }}>
    <summary>Plan a move to another server</summary>
    <div className="settings-stack">
      <p>Review the source before preparing a destination. This check makes no server changes. Automated transfer is not available yet.</p>
      {busy && <p role="status">Checking application dependencies…</p>}
      {error && <ErrorBanner>{error}</ErrorBanner>}
      {plan && <>
        <p>Checked {new Date(plan.checked).toLocaleString()} · Commit {plan.commit?.slice(0, 12) || 'not deployed'}</p>
        <p>Domains: {plan.domains.join(', ')}. Persistent files: {plan.persistent_files ? 'transfer required' : 'no managed files recorded'}. Cache: {plan.cache_mode || 'none recorded'}.</p>
        {plan.databases.length > 0 && <ul>{plan.databases.map(db => <li key={db.id}>{db.id} ({db.mode}){db.shared_with.length ? ` · shared with ${db.shared_with.join(', ')}` : ''}</li>)}</ul>}
        {plan.tasks.length > 0 && <p>Workers and schedules to coordinate: {plan.tasks.join(', ')}.</p>}
        {plan.blockers.length > 0 && <><strong>Resolve before moving</strong><ul>{plan.blockers.map(item => <li key={item}>{item}</li>)}</ul></>}
        <strong>Cutover checklist</strong><ol>{plan.steps.map(item => <li key={item}>{item}</li>)}</ol>
      </>}
    </div>
  </details>;
}
