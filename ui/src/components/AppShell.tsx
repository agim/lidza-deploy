import { Activity, AppWindow, Boxes, CircleHelp, CloudCog, DatabaseBackup, ChevronDown, ChevronRight, ArrowUpRight, LayoutGrid, Menu, PackageOpen, RefreshCw, ScrollText, Server, Settings2, ShieldCheck, Users, X } from 'lucide-react';
import { type ReactNode, useEffect, useState } from 'react';
import type { PageKey, Route } from '../lib/types';
import { relativeTime } from '../lib/format';
import { useWorkspace } from '../state/WorkspaceContext';
import { IconButton, Loading } from './ui';
import { Sheet, SheetContent, SheetTitle, SheetDescription } from './primitives/sheet';
import { DropdownMenu, DropdownMenuTrigger, DropdownMenuContent, DropdownMenuItem } from './primitives/dropdown-menu';
import { useToast } from './ToastProvider';

const groups: Array<{ label: string; items: Array<{ page: PageKey; label: string; icon: typeof AppWindow }> }> = [
  { label: 'Applications', items: [
    { page: 'applications', label: 'Applications', icon: LayoutGrid },
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
  const { apps, loading, refreshing, updatedAt, error, unavailableServers, status } = useWorkspace();
  const notify = useToast();
  useEffect(() => setOpen(false), [route.page, route.detail, route.id]);
  const activePage = route.page === 'detail' ? (['server', 'containers', 'console', 'ssh', 'upgrade'].includes(route.detail || '') ? 'servers' : route.detail === 'database' ? 'databases' : 'applications') : route.page;
  const navigation = <><a className="brand" href="#applications" aria-label="Līdza Deploy home"><strong>līdza<ArrowUpRight size={20} strokeWidth={1.3} /></strong><span>deploy</span></a>
    <div className="workspace"><span className="workspace-avatar">L</span><div><strong>Your workspace</strong><span>Deployment control</span></div><ChevronDown size={13} /></div>
    <nav aria-label="Workspace navigation">{groups.map(group => <div className="nav-group" key={group.label}><p>{group.label}</p>{group.items.map(item => { const Icon = item.icon; const active = activePage === item.page; return <a key={item.page} href={`#${item.page}`} onClick={() => setOpen(false)} className={active ? 'active' : ''} aria-current={active ? 'page' : undefined}><Icon size={17} strokeWidth={1.7} /><span>{item.label}</span>{item.page === 'applications' && <span className="nav-count">{apps.length}</span>}</a>; })}</div>)}</nav>
    <div className="sidebar-bottom"><div className="agent-status"><span className={`status-dot ${error || unavailableServers.length ? 'warning-dot' : ''}`} />{loading ? 'Connecting to workspace' : error || unavailableServers.length ? 'Some connections need attention' : 'Workspace connected'}<ArrowUpRight size={13} /></div><div className="workspace-user"><span className="user-avatar">A</span><div><strong>{status.roles?.includes('admin') ? 'Workspace admin' : 'Workspace member'}</strong><span>Your infrastructure</span></div><ShieldCheck size={15} /></div></div></>;
  return <div className="app-shell">
    <aside className="sidebar desktop-sidebar">{navigation}</aside>
    <Sheet open={open} onOpenChange={setOpen}><SheetContent side="left" className="mobile-sidebar" showCloseButton={false}><SheetTitle className="sr-only">Workspace menu</SheetTitle><SheetDescription className="sr-only">Navigate applications, infrastructure and workspace settings.</SheetDescription><IconButton className="mobile-nav-close" label="Close menu" onClick={() => setOpen(false)}><X size={18} /></IconButton>{navigation}</SheetContent></Sheet>
    <div className="app-frame app-body">
      <header className="topbar"><div className="breadcrumbs topbar-left"><IconButton className="mobile-menu" label="Open menu" onClick={() => setOpen(true)}><Menu size={18} /></IconButton><span>Workspace</span><ChevronRight size={13} /><strong>{labelForRoute(route)}</strong></div><div className="topbar-right"><span className="updated live-indicator" role="status"><span className={refreshing ? 'online-dot pulse' : 'online-dot'} />{loading ? 'Connecting…' : updatedAt ? `Updated ${relativeTime(updatedAt.toISOString())}` : 'Connected'}</span><DropdownMenu><DropdownMenuTrigger asChild><button className="user-avatar small account-trigger" aria-label="Account menu">A</button></DropdownMenuTrigger><DropdownMenuContent align="end" className="account-dropdown"><DropdownMenuItem asChild><a href="#page/two-factor/account">Two-factor authentication</a></DropdownMenuItem><DropdownMenuItem onSelect={() => { void (async () => { try { const response = await fetch('/api/v1/auth/logout', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' }); if (!response.ok) throw new Error('Sign out failed. Please try again.'); location.href = '/login.html'; } catch (reason) { notify(String(reason), true); } })(); }}>Sign out</DropdownMenuItem></DropdownMenuContent></DropdownMenu></div></header>
      <main id="main-content">{loading ? <Loading overlay label="Loading your workspace…" /> : children}<footer className="page-footer"><span>Built with Līdza <span aria-hidden="true">·</span> Independent apps. One place to deploy.</span><span>shadcn/ui · Nova</span></footer></main>
    </div>
  </div>;
}

function labelForRoute(route: Route) {
  const labels: Partial<Record<PageKey, string>> = { applications: 'Applications', deployments: 'Deployments', errors: 'Errors', security: 'Security activity', servers: 'Servers', databases: 'Databases & backups', 'remote-backups': 'Remote backups', integrations: 'Integrations', defaults: 'Application defaults', updates: 'Updates', team: 'Team', audit: 'Audit log', help: 'Help', 'new-application': 'New application', detail: route.id || 'Details' };
  return labels[route.page] || 'Workspace';
}
