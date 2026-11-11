import { usePreference } from '../state/PreferencesContext';
import { Input } from '../components/primitives/input';
import { Pause, Play, RefreshCw } from 'lucide-react';
import { useEffect, useRef, useState } from 'react';
import { get } from '../lib/api';
import type { Deployment } from '../lib/types';
import { useWorkspace } from '../state/WorkspaceContext';
import { Button, Card, ErrorBanner, Loading, LogOutputHeader, StatusBadge } from '../components/ui';
import { Cluster } from '../components/layout';

/** LiveLog: live output of a deployment (Log tab) or of a running application (Logs tab). */
export function LiveLog({ kind, id }: { kind: 'build-logs' | 'runtime-logs'; id: string }) {
  const { deployments } = useWorkspace();
  const initial = deployments.find(item => item.id === id);
  const [deployment, setDeployment] = useState<Deployment | undefined>(initial);
  const [output, setOutput] = useState(initial?.log || initial?.error || '');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(true);
  const [paused, setPaused] = useState(false);
  const [follow, setFollow] = usePreference<boolean>('deploy.logs.follow', true);
  const [refreshKey, setRefreshKey] = useState(0);
  const [manualRefreshing, setManualRefreshing] = useState(false);
  // Native applications list their services; the log can show one or all.
  const [units, setUnits] = useState<string[]>([]);
  const [unit, setUnit] = useState('');
  const outputRef = useRef<HTMLPreElement>(null);
  const terminal = kind === 'build-logs' && !!deployment && !['queued', 'building'].includes(deployment.status);

  useEffect(() => {
    let active = true;
    let timer: number | undefined;
    const schedule = (finished = false) => {
      if (active && !paused && !finished) timer = window.setTimeout(load, 1_500);
    };
    const load = async () => {
      try {
        if (kind === 'build-logs') {
          const current = await get<Deployment>(`control/deployments/${encodeURIComponent(id)}`);
          if (!active) return;
          setDeployment(current);
          setOutput(current.log || current.error || '');
          setError(''); setLoading(false);
          schedule(!['queued','building'].includes(current.status));
          return;
        } else {
          const value = await get<{ logs?: string; log?: string; units?: string[] }>(`control/apps/${encodeURIComponent(id)}/logs${unit ? `?unit=${encodeURIComponent(unit)}` : ''}`);
          if (!active) return;
          const text = value.logs ?? value.log;
          if (text !== undefined) setOutput(text);
          setUnits(Array.isArray(value.units) ? value.units : []);
          setError('');
        }
      } catch (reason) {
        if (!active) return;
        setError(`Live refresh failed: ${reason instanceof Error ? reason.message : String(reason)}. Retrying…`);
      } finally { if (active) setManualRefreshing(false); }
      if (active) setLoading(false);
      schedule(false);
    };
    load();
    return () => { active = false; if (timer) clearTimeout(timer); };
  }, [kind, id, paused, refreshKey, unit]);

  useEffect(() => {
    const element = outputRef.current;
    if (follow && element) element.scrollTop = element.scrollHeight;
  }, [output, follow]);

  const failure = kind === 'build-logs' ? deployment?.error : undefined;
  const visibleOutput = failure && output && !output.includes(failure) ? `${output}\n\nFailure: ${failure}` : output || (terminal ? 'No build output was recorded.' : kind === 'build-logs' ? 'Waiting for build output…' : 'Waiting for runtime output…');
  const stateLabel = kind === 'runtime-logs' ? `${paused ? 'Paused' : 'Live'} · runtime` : terminal ? `Finished · ${deployment?.status}` : paused ? `Paused · ${deployment?.status || 'waiting'}` : `Live · ${deployment?.status || 'waiting'}`;
  const status = kind === 'runtime-logs' ? loading ? 'connecting' : 'running' : deployment?.status || (loading ? 'connecting' : 'waiting');
  return <div className="settings-stack" data-live-log={kind}><Cluster><Button id="log-pause" onClick={() => setPaused(value => !value)} disabled={terminal}>{paused ? <Play size={15} /> : <Pause size={15} />}{paused ? 'Resume live output' : 'Pause live output'}</Button><Button feedback pending={manualRefreshing} onClick={() => { setManualRefreshing(true); setRefreshKey(value => value + 1); }}><RefreshCw size={15} />Refresh</Button></Cluster>{failure && <div id="deployment-failure"><ErrorBanner>{failure}</ErrorBanner></div>}{error && <div id="deployment-refresh-error"><ErrorBanner>{error}</ErrorBanner></div>}<Card className="log-status-card"><div><StatusBadge status={status} /><strong id="deployment-state" aria-live="polite">{stateLabel}</strong>{deployment && <span>{Math.round(deployment.duration_seconds || 0)} seconds</span>}</div>{kind === 'runtime-logs' && units.length > 1 && <label className="log-follow-control"><span>Process</span><select aria-label="Process" value={unit} onChange={event => { setOutput(''); setUnit(event.target.value); }}><option value="">All processes</option>{units.map(name => <option key={name} value={name}>{name}</option>)}</select></label>}<label className="log-follow-control"><Input id="log-follow" type="checkbox" checked={follow} onChange={event => setFollow(event.target.checked)} /><span>Follow latest output</span></label></Card><Card className="log-card"><LogOutputHeader title={kind === 'build-logs' ? 'Build log' : 'Runtime log'} value={visibleOutput} copyLabel={kind === 'build-logs' ? 'Copy build log' : 'Copy runtime log'} />{loading && !output ? <Loading label={`Connecting to live ${kind === 'build-logs' ? 'build' : 'runtime'} output…`} /> : <pre id="deployment-output" ref={outputRef} onScroll={event => { const element = event.currentTarget; if (follow && element.scrollHeight - element.scrollTop - element.clientHeight > 28) setFollow(false); }}>{visibleOutput}</pre>}</Card></div>;
}
