import { Activity, AppWindow, Boxes, CircleHelp, CloudCog, DatabaseBackup, History, LayoutDashboard, Menu, PackageOpen, RefreshCw, ScrollText, Server, Settings2, ShieldCheck, Users, X } from 'lucide-react';
import { type ReactNode, useEffect, useState } from 'react';
import type { PageKey, Route } from '../lib/types';
import { relativeTime } from '../lib/format';
import { useWorkspace } from '../state/WorkspaceContext';
import { Button, IconButton, cx } from './ui';

const groups: Array<{ label: string; items: Array<{ page: PageKey; label: string; icon: typeof AppWindow }> }> = [
  { label: 'Applications', items: [
    { page: 'applications', label: 'Applications', icon: LayoutDashboard },
    { page: 'deployments', label: 'Deployments', icon: PackageOpen },
    { page: 'errors', label: 'Errors', icon: Activity },
    { page: 'security', label: 'Security activity', icon: ShieldCheck },
  ] },
  { label: 'Infrastructure', items: [
    { page: 'servers', label: 'Servers', icon: Server },
    { page: 'databases', label: 'Databases & backups', icon: DatabaseBackup },
    { page: 'remote-backups', label: 'Remote backups', icon: Boxes },
    { page: 'updates', label: 'Updates', icon: RefreshCw },
  ] },
  { label: 'Workspace', items: [
    { page: 'integrations', label: 'Integrations', icon: CloudCog },
    { page: 'defaults', label: 'Application defaults', icon: Settings2 },
    { page: 'team', label: 'Team', icon: Users },
    { page: 'audit', label: 'Audit log', icon: ScrollText },
    { page: 'help', label: 'Help', icon: CircleHelp },
  ] },
];

export function AppShell({ route, children }: { route: Route; children: ReactNode }) {
  const [open, setOpen] = useState(false);
  const { apps, loading, refreshing, updatedAt } = useWorkspace();
  useEffect(() => setOpen(false), [route.page, route.detail, route.id]);
  useEffect(() => { document.body.style.overflow = open ? 'hidden' : ''; return () => { document.body.style.overflow = ''; }; }, [open]);
  const activePage = route.page === 'detail' ? (route.detail === 'server' || route.detail === 'containers' || route.detail === 'console' || route.detail === 'ssh' || route.detail === 'upgrade' ? 'servers' : route.detail === 'database' ? 'databases' : 'applications') : route.page;
  return <div className="app-shell">
    <div className={cx('mobile-scrim', open && 'open')} onClick={() => setOpen(false)} />
    <aside className={cx('sidebar', open && 'open')}>
      <div className="sidebar-brand"><a href="#applications" aria-label="Līdza Deploy home"><strong>līdza</strong><span>deploy</span></a><IconButton className="mobile-close" label="Close menu" onClick={() => setOpen(false)}><X size={20} /></IconButton></div>
      <div className="workspace-identity"><span className="workspace-mark">L</span><div><strong>Your workspace</strong><small>Deployment control</small></div></div>
      <nav aria-label="Workspace navigation">{groups.map(group => <div className="nav-group" key={group.label}><p>{group.label}</p>{group.items.map(item => { const Icon = item.icon; const active = activePage === item.page; return <a key={item.page} href={`#${item.page}`} className={active ? 'active' : ''} aria-current={active ? 'page' : undefined}><Icon size={18} strokeWidth={1.8} /><span>{item.label}</span>{item.page === 'applications' && <b>{apps.length}</b>}</a>; })}</div>)}</nav>
      <div className="sidebar-footer"><span className="online-dot" /> Līdza deployment control</div>
    </aside>
    <div className="app-frame">
      <header className="topbar"><div className="topbar-left"><IconButton className="mobile-menu" label="Open menu" onClick={() => setOpen(true)}><Menu size={21} /></IconButton><span>Workspace</span><span>/</span><strong>{labelForRoute(route)}</strong></div><div className="topbar-right"><span className="updated"><span className={refreshing ? 'online-dot pulse' : 'online-dot'} />{loading ? 'Connecting…' : `Updated ${relativeTime(updatedAt?.toISOString())}`}</span><details className="account-menu"><summary><span className="avatar">A</span><span>Account</span></summary><div><a href="#page/two-factor/account">Two-factor authentication</a><button onClick={async () => { await fetch('/api/v1/auth/logout', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' }); location.href = '/login.html'; }}>Sign out</button></div></details></div></header>
      <main id="main-content">{children}<footer><span>Built with Līdza</span><span>Independent apps. One place to deploy.</span></footer></main>
    </div>
  </div>;
}

function labelForRoute(route: Route) {
  const labels: Partial<Record<PageKey, string>> = { applications: 'Applications', deployments: 'Deployments', errors: 'Errors', security: 'Security activity', servers: 'Servers', databases: 'Databases & backups', 'remote-backups': 'Remote backups', integrations: 'Integrations', defaults: 'Application defaults', updates: 'Updates', team: 'Team', audit: 'Audit log', help: 'Help', 'new-application': 'New application', detail: route.id || 'Details' };
  return labels[route.page] || 'Workspace';
}
