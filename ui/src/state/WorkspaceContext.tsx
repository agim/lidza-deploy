import { createContext, type ReactNode, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react';
import { get } from '../lib/api';
import type { Application, ControlStatus, Deployment, Server, WorkspaceData } from '../lib/types';

interface WorkspaceContextValue extends WorkspaceData {
  loading: boolean;
  refreshing: boolean;
  error: string;
  updatedAt?: Date;
  refresh: (foreground?: boolean) => Promise<void>;
  latestDeployment: (appID: string) => Deployment | undefined;
}

const WorkspaceContext = createContext<WorkspaceContextValue | null>(null);

const demoApps: Application[] = [
  { id: 'customer-portal', repository: 'acme/customer-portal', branch: 'main', domain: 'portal.example.com', server_id: 'eu-1', auto_deploy: true, current: { commit: 'a6b92fe' }, domain_status: { state: 'ready' } },
  { id: 'content-studio', repository: 'acme/content-studio', branch: 'release', domain: 'studio.example.com', server_id: 'eu-1', auto_deploy: true, current: { commit: '8f129bc' }, domain_status: { state: 'ready' } },
  { id: 'documentation', repository: 'acme/docs', branch: 'main', domain: 'docs.example.com', server_id: 'us-1', auto_deploy: false, stopped: true, domain_status: { state: 'ready' } },
];
const demoServers: Server[] = [{ id: 'eu-1', name: 'Frankfurt production', url: 'https://agent-eu.example.com' }, { id: 'us-1', name: 'Virginia staging', url: 'https://agent-us.example.com' }];
const demoDeployments: Deployment[] = demoApps.map((app, index) => ({ id: `demo-${index}`, app_id: app.id, status: index === 2 ? 'failed' : 'live', commit: app.current?.commit, created: new Date(Date.now() - (index + 1) * 3600000).toISOString(), duration_seconds: 42 + index * 8, error: index === 2 ? 'Readiness check failed' : '' }));

export function WorkspaceProvider({ children }: { children: ReactNode }) {
  const demo = new URLSearchParams(location.search).get('demo') === '1';
  const [data, setData] = useState<WorkspaceData>({ apps: demo ? demoApps : [], servers: demo ? demoServers : [], deployments: demo ? demoDeployments : [], status: demo ? { roles: ['admin'], github_app_connected: true, github_app_configured: true, github_public_https: true, two_factor_enabled: true } : {}, unavailableServers: [] });
  const [loading, setLoading] = useState(!demo);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState('');
  const [updatedAt, setUpdatedAt] = useState<Date>();
  const busy = useRef(false);

  const refresh = useCallback(async (foreground = false) => {
    if (demo || busy.current || document.hidden) return;
    busy.current = true;
    if (foreground) setLoading(true); else setRefreshing(true);
    const results = await Promise.allSettled([
      get<Application[]>('control/apps'), get<Server[]>('control/servers'),
      get<{ deployments?: Deployment[]; unavailable_servers?: string[]; server_errors?: Record<string,string> } | Deployment[]>('control/deployments'),
      get<ControlStatus>('control/status'),
    ]);
    setData(current => {
      const apps = results[0].status === 'fulfilled' ? results[0].value : current.apps;
      const servers = results[1].status === 'fulfilled' ? results[1].value : current.servers;
      const raw = results[2].status === 'fulfilled' ? results[2].value : null;
      const deployments = Array.isArray(raw) ? raw : raw?.deployments || current.deployments;
      const unavailableServers = Array.isArray(raw) ? [] : raw?.unavailable_servers || current.unavailableServers;
      const status = results[3].status === 'fulfilled' ? results[3].value : current.status;
      return { apps: [...apps].sort((a, b) => a.id.localeCompare(b.id)), servers: [...servers].sort((a, b) => a.id.localeCompare(b.id)), deployments, status, unavailableServers };
    });
    const failures = results.filter(result => result.status === 'rejected') as PromiseRejectedResult[];
    const history = results[2].status==='fulfilled' && !Array.isArray(results[2].value) ? results[2].value.unavailable_servers || [] : [];
    const reasons = results[2].status==='fulfilled' && !Array.isArray(results[2].value) ? Object.values(results[2].value.server_errors || {}).join('; ') : '';
    setError(failures.length ? `Some workspace data could not be refreshed: ${failures.map(result => result.reason instanceof Error ? result.reason.message : String(result.reason)).join('; ')}` : history.length ? `Deployment history unavailable for ${history.join(', ')}${reasons ? `: ${reasons}` : ''}. Accepted deployments may still be running; check the hosting agent.` : '');
    if (!failures.length) setUpdatedAt(new Date());
    setLoading(false); setRefreshing(false); busy.current = false;
  }, [demo]);

  useEffect(() => { refresh(true); const timer = window.setInterval(() => refresh(false), 10_000); return () => clearInterval(timer); }, [refresh]);
  const latestDeployment = useCallback((appID: string) => data.deployments.filter(item => item.app_id === appID).sort((a, b) => +new Date(b.created) - +new Date(a.created))[0], [data.deployments]);
  const value = useMemo(() => ({ ...data, loading, refreshing, error, updatedAt, refresh, latestDeployment }), [data, loading, refreshing, error, updatedAt, refresh, latestDeployment]);
  return <WorkspaceContext.Provider value={value}>{children}</WorkspaceContext.Provider>;
}

export function useWorkspace() {
  const context = useContext(WorkspaceContext);
  if (!context) throw new Error('WorkspaceProvider is missing');
  return context;
}
