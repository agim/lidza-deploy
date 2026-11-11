import { Page } from './layout';
import { SectionBoundary } from './SectionBoundary';
import { Activity, AppWindow, Boxes, ChevronsUpDown, CircleHelp, CloudCog, DatabaseBackup, ChevronRight, ArrowUpRight, ArchiveRestore, LayoutGrid, LogOut, Menu, PackageOpen, RefreshCw, ScrollText, Server, ServerCog, Settings2, ShieldCheck, UserRound, Users, X } from 'lucide-react';
import { type ReactElement, type ReactNode, useEffect, useState } from 'react';
import type { Route } from '../lib/types';
import { type CollectionKey, breadcrumbs, indexHref, singletonHref } from '../lib/resources';
import { Breadcrumbs, useResourceName } from './resource';
import { heldRoleLabel, relativeTime } from '../lib/format';
import { HOME, withSticky } from '../lib/router';
import { usePreferences } from '../state/PreferencesContext';
import { useWorkspace } from '../state/WorkspaceContext';
import { ActionMenu, Button, ErrorBanner, IconButton, Loading, MenuItem, MenuLink } from './ui';
import { Sheet, SheetContent, SheetTitle, SheetDescription } from './primitives/sheet';
import { ActivityDrawer } from './ActivityDrawer';

const groups: Array<{ label: string; items: Array<{ page: CollectionKey; label: string; icon: typeof AppWindow }> }> = [
  { label: 'Applications', items: [
    { page: 'applications', label: 'Applications', icon: LayoutGrid },
    { page: 'deployments', label: 'Deployments', icon: PackageOpen },
    { page: 'errors', label: 'Errors', icon: Activity },
    { page: 'security', label: 'Security activity', icon: ShieldCheck },
  ] },
  { label: 'Infrastructure', items: [
    { page: 'servers', label: 'Servers', icon: Server },
    { page: 'databases', label: 'Databases', icon: DatabaseBackup },
    { page: 'remote-backups', label: 'Remote backups', icon: Boxes },
    { page: 'updates', label: 'Updates', icon: RefreshCw },
  ] },
  { label: 'Settings', items: [
    { page: 'integrations', label: 'Integrations', icon: CloudCog },
    { page: 'defaults', label: 'Application defaults', icon: Settings2 },
    { page: 'team', label: 'Team', icon: Users },
    { page: 'deleted-apps', label: 'Deleted apps', icon: ArchiveRestore },
    { page: 'control-panel', label: 'Control panel', icon: ServerCog },
    { page: 'audit', label: 'Audit log', icon: ScrollText },
    { page: 'help', label: 'Help', icon: CircleHelp },
  ] },
];

/** initial is the first letter of a name, for an avatar. */
function initial(value = '') {
  return (value.trim().match(/[\p{L}\p{N}]/u)?.[0] || '·').toUpperCase();
}

/** signOut ends the session through the framework and opens the sign-in page. */
async function signOut(flush: () => Promise<unknown>) {
  await flush().catch(() => {});
  const response = await fetch('/api/v1/auth/logout', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
  if (!response.ok) throw new Error('Sign out failed. Please try again.');
  location.href = '/login.html';
}

/** AccountMenu holds the signed-in person's links: their account and signing out. */
function AccountMenu({ trigger, side }: { trigger: ReactElement; side?: 'top' | 'bottom' }) {
  const preferences = usePreferences();
  return <ActionMenu trigger={trigger} side={side} align={side === 'top' ? 'start' : 'end'}>
    <MenuLink href={withSticky(singletonHref('account'))} icon={<UserRound size={16} />}>Account</MenuLink>
    <MenuItem icon={<LogOut size={16} />} onSelect={() => signOut(preferences.flush)}>Sign out</MenuItem>
  </ActionMenu>;
}

/**
 * serverStatus names what the sidebar status measures: whether every
 * connected server's agent answered the last refresh.
 */
function serverStatus({ loading, error, servers, unavailable }: { loading: boolean; error: string; servers: Array<{ id: string; name: string }>; unavailable: string[] }) {
  if (loading) return { label: 'Checking servers…', warning: false, title: '' };
  if (unavailable.length) {
    const names = unavailable.map(id => servers.find(server => server.id === id)?.name || id);
    return { label: `${unavailable.length} ${unavailable.length === 1 ? 'server' : 'servers'} unreachable`, warning: true, title: `Unreachable: ${names.join(', ')}` };
  }
  if (error) return { label: 'Server status not refreshed', warning: true, title: error };
  if (!servers.length) return { label: 'No servers connected', warning: true, title: '' };
  return { label: 'All servers connected', warning: false, title: servers.map(server => server.name || server.id).join(', ') };
}

export function AppShell({ route, children }: { route: Route; children: ReactNode }) {
  const [open, setOpen] = useState(false);
  const { apps, servers, loading, refreshing, updatedAt, error, unavailableServers, status } = useWorkspace();
  const preferences = usePreferences();
  useEffect(() => setOpen(false), [route.view, route.collection, route.id, route.tab]);
  const name = useResourceName();
  const trail = breadcrumbs(route, name);
  const activePage = route.collection;
  const panel = status.hostname || 'Control panel';
  const person = status.user?.name || status.user?.email || 'Signed in';
  const role = heldRoleLabel(status.roles);
  const connection = serverStatus({ loading, error, servers, unavailable: unavailableServers });
  const navigation = <><a className="brand" href={withSticky(HOME)} aria-label="Līdza Deploy home"><strong>līdza<ArrowUpRight size={20} strokeWidth={1.3} /></strong><span>deploy</span></a>
    <a className="panel-identity" href={withSticky(singletonHref('control-panel'))} onClick={() => setOpen(false)}><span className="panel-avatar" aria-hidden="true">{initial(status.hostname || 'L')}</span><span className="sidebar-identity-text"><strong>{panel}</strong><span>{status.control_version ? `Release ${status.control_version}` : 'Control panel'}</span></span></a>
    <nav aria-label="Main navigation">{groups.map(group => <div className="nav-group" key={group.label}><p>{group.label}</p>{group.items.map(item => { const Icon = item.icon; const active = activePage === item.page; return <a key={item.page} href={withSticky(indexHref(item.page))} onClick={() => setOpen(false)} className={active ? 'active' : ''} aria-current={active ? 'page' : undefined}><Icon size={17} strokeWidth={1.7} /><span>{item.label}</span>{item.page === 'applications' && <span className="nav-count">{apps.length}</span>}</a>; })}</div>)}</nav>
    <div className="sidebar-bottom"><a className="agent-status" href={withSticky(indexHref('servers'))} onClick={() => setOpen(false)} title={connection.title || undefined}><span className={`status-dot ${connection.warning ? 'warning-dot' : ''}`} aria-hidden="true" />{connection.label}<ChevronRight size={13} aria-hidden="true" /></a>
      <AccountMenu side="top" trigger={<button type="button" className="sidebar-user" aria-label={`Account menu for ${person}`}><span className="user-avatar" aria-hidden="true">{initial(status.user?.name || status.user?.email || 'A')}</span><span className="sidebar-identity-text"><strong>{person}</strong><span>{[role, status.user?.name ? status.user?.email : ''].filter(Boolean).join(' · ') || 'Account'}</span></span><ChevronsUpDown size={14} aria-hidden="true" /></button>} /></div></>;
  return <div className="app-shell">
    <aside className="sidebar desktop-sidebar">{navigation}</aside>
    <Sheet open={open} onOpenChange={setOpen}><SheetContent side="left" className="mobile-sidebar" showCloseButton={false}><SheetTitle className="sr-only">Navigation menu</SheetTitle><SheetDescription className="sr-only">Navigate applications, infrastructure and settings of this control panel.</SheetDescription><IconButton className="mobile-nav-close" label="Close menu" onClick={() => setOpen(false)}><X size={18} /></IconButton>{navigation}</SheetContent></Sheet>
    <div className="app-frame app-body">
      <header className="topbar"><div className="topbar-left"><IconButton className="mobile-menu" label="Open menu" onClick={() => setOpen(true)}><Menu size={18} /></IconButton><Breadcrumbs trail={trail} /></div><div className="topbar-right"><span className="updated live-indicator" role="status"><span className={refreshing ? 'online-dot pulse' : 'online-dot'} />{loading ? 'Connecting…' : updatedAt ? `Updated ${relativeTime(updatedAt.toISOString())}` : 'Connected'}</span><ActivityDrawer /><AccountMenu trigger={<button type="button" className="user-avatar small account-trigger" aria-label={`Account menu for ${person}`}>{initial(status.user?.name || status.user?.email || 'A')}</button>} /></div></header>
      <main id="main-content">{preferences.error && <ErrorBanner>{preferences.error} <Button size="sm" onClick={() => { void preferences.flush().catch(() => {}); }}>Retry</Button></ErrorBanner>}{loading || !preferences.ready ? <Loading overlay label="Loading the control panel…" /> : <Page><SectionBoundary label={trail[trail.length - 1].label || 'This page'} resetKey={route}>{children}</SectionBoundary></Page>}<footer className="page-footer"><span>Built with Līdza <span aria-hidden="true">·</span> Independent apps. One place to deploy.</span><span>shadcn/ui · Nova</span></footer></main>
    </div>
  </div>;
}

