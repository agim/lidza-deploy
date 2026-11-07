import { Play, RefreshCw, Square, Trash2 } from 'lucide-react';
import type { ReactNode } from 'react';
import { post } from '../lib/api';
import { ARCHIVE_RETENTION_DAYS } from '../lib/archive';
import { moveActive } from '../lib/move';
import type { Application, Deployment } from '../lib/types';
import { DeleteAppAction } from './DeleteAppDialog';
import { useToast } from './ToastProvider';
import { Button, Card, ConfirmAction, MenuItem, MenuLabel, MenuSeparator, SectionHeader } from './ui';

// Restart, stop/start and delete, the same for every runtime: the card's
// "Application" menu group and the settings page's Danger zone.

export type LifecycleAction = 'stop' | 'start' | 'restart';

/** Why the application cannot be stopped, started or restarted now; '' when it can. */
export function lifecycleBlocked(app: Application, deployment?: Pick<Deployment, 'status'>): string {
  if (app.retiring) return 'The application is being removed.';
  if (moveActive(app.move)) return 'This application is moving; finish or abort the move first.';
  if (['queued', 'building'].includes(deployment?.status || '')) return 'Wait for the running deployment to finish.';
  if (app.agent_error) return 'The server’s agent is unavailable.';
  if (app.native && !app.management_capabilities?.lifecycle) return app.management_capabilities?.lifecycle_reason || 'Update this server’s agent to stop, start and restart native applications.';
  return '';
}

/** Why the application cannot be deleted now; '' when it can. */
export function deleteBlocked(app: Application, deployment?: Pick<Deployment, 'status'>): string {
  if (app.retiring) return 'The application is being removed.';
  if (moveActive(app.move)) return 'This application is moving; finish or abort the move first.';
  if (['queued', 'building'].includes(deployment?.status || '')) return 'Wait for the running deployment to finish.';
  if (app.native && !app.management_capabilities?.remove) return 'Update this server’s agent to delete this application.';
  return '';
}

export const lifecycleCopy = {
  stop: { title: (id: string) => `Stop ${id}?`, confirm: 'Stop application', description: 'Every process stops and stays stopped after a reboot, and scheduled jobs pause. Visitors see a maintenance page; data is kept. Start it again at any time.' },
  restart: { title: (id: string) => `Restart ${id}?`, confirm: 'Restart application', description: 'Every running process restarts (a container application reloads its current release). Visitors may see errors for a few seconds; data is kept.' },
};

/** useLifecycle runs an action and reports it. */
export function useLifecycle(app: Application, onChanged: () => void | Promise<void>) {
  const notify = useToast();
  return async (action: LifecycleAction) => {
    if (new URLSearchParams(location.search).get('demo') !== '1') await post(`control/apps/${encodeURIComponent(app.id)}/${action}`);
    notify(action === 'stop' ? `${app.id} stopped. Visitors see a maintenance page.` : action === 'start' ? `${app.id} is starting.` : `${app.id} is restarting.`);
    await onChanged();
  };
}

function ConfirmMenuItem({ icon, label, blocked, title, description, confirmLabel, destructive, onConfirm, action }: { icon: ReactNode; label: string; blocked: string; title: string; description: string; confirmLabel: string; destructive?: boolean; onConfirm: () => Promise<void>; action: string }) {
  if (blocked) return <MenuItem disabled title={blocked} icon={icon}>{label}</MenuItem>;
  return <ConfirmAction title={title} description={description} confirmLabel={confirmLabel} destructive={destructive} onConfirm={onConfirm}><div className="menu-confirm-trigger menu-confirm-neutral" role="menuitem" tabIndex={0} data-lifecycle={action}>{icon}{label}</div></ConfirmAction>;
}

/** The card menu's "Application" group: Restart, Stop or Start, Delete. */
export function AppLifecycleMenu({ app, deployment, onChanged, onDeleted }: { app: Application; deployment?: Pick<Deployment, 'status'>; onChanged: () => void | Promise<void>; onDeleted: () => void | Promise<void> }) {
  const run = useLifecycle(app, onChanged);
  const blocked = lifecycleBlocked(app, deployment);
  const removal = deleteBlocked(app, deployment);
  return <>
    <MenuSeparator /><MenuLabel>Application</MenuLabel>
    <ConfirmMenuItem action="restart" icon={<RefreshCw size={16} />} label="Restart" blocked={blocked || (app.stopped ? 'The application is stopped; start it instead.' : '')} title={lifecycleCopy.restart.title(app.id)} description={lifecycleCopy.restart.description} confirmLabel={lifecycleCopy.restart.confirm} onConfirm={() => run('restart')} />
    {app.stopped
      ? <MenuItem disabled={!!blocked} title={blocked || undefined} icon={<Play size={16} />} onSelect={() => run('start')}>Start</MenuItem>
      : <ConfirmMenuItem action="stop" icon={<Square size={16} />} label="Stop" blocked={blocked} title={lifecycleCopy.stop.title(app.id)} description={lifecycleCopy.stop.description} confirmLabel={lifecycleCopy.stop.confirm} destructive onConfirm={() => run('stop')} />}
    {removal ? <MenuItem disabled title={removal} icon={<Trash2 size={16} />}>Delete</MenuItem> : <DeleteAppAction app={app} onDeleted={onDeleted}><div className="menu-confirm-trigger" role="menuitem" tabIndex={0} data-lifecycle="delete"><Trash2 size={16} />Delete</div></DeleteAppAction>}
  </>;
}

/** The Danger zone at the bottom of an application's General settings. */
export function AppDangerZone({ app, deployment, onChanged, onDeleted }: { app: Application; deployment?: Pick<Deployment, 'status'>; onChanged: () => void | Promise<void>; onDeleted: () => void | Promise<void> }) {
  const run = useLifecycle(app, onChanged);
  const blocked = lifecycleBlocked(app, deployment);
  const removal = deleteBlocked(app, deployment);
  return <Card className="form-card danger-zone" data-danger-zone={app.id}>
    <SectionHeader title="Danger zone" description="These actions affect visitors at once." />
    <div className="danger-zone-row">
      {app.stopped
        ? <><div><strong>Start this application</strong><small>It is stopped: visitors see a maintenance page. Starting it brings back its processes and scheduled jobs.</small>{blocked && <small role="note">{blocked}</small>}</div><Button variant="primary" disabled={!!blocked} title={blocked || undefined} onClick={() => run('start')}><Play size={16} />Start application</Button></>
        : <><div><strong>Stop this application</strong><small>{lifecycleCopy.stop.description}</small>{blocked && <small role="note">{blocked}</small>}</div><ConfirmAction title={lifecycleCopy.stop.title(app.id)} description={lifecycleCopy.stop.description} confirmLabel={lifecycleCopy.stop.confirm} destructive onConfirm={() => run('stop')}><Button variant="danger" disabled={!!blocked} title={blocked || undefined}><Square size={16} />Stop application</Button></ConfirmAction></>}
    </div>
    <div className="danger-zone-row">
      <div><strong>Delete this application</strong><small>It stops serving at once and its hostnames become free. It can be restored from Deleted apps for {ARCHIVE_RETENTION_DAYS} days.</small>{removal && <small role="note">{removal}</small>}</div>
      {removal ? <Button variant="danger" disabled title={removal}><Trash2 size={16} />Delete application</Button> : <DeleteAppAction app={app} onDeleted={onDeleted}><Button variant="danger"><Trash2 size={16} />Delete application</Button></DeleteAppAction>}
    </div>
  </Card>;
}

/** The card's primary action while an application is stopped. */
export function StartApplicationButton({ app, deployment, onChanged }: { app: Application; deployment?: Pick<Deployment, 'status'>; onChanged: () => void | Promise<void> }) {
  const run = useLifecycle(app, onChanged);
  const blocked = lifecycleBlocked(app, deployment);
  return <Button size="sm" variant="primary" data-action="start" data-id={app.id} disabled={!!blocked} title={blocked || undefined} onClick={() => run('start')}><Play size={14} />Start</Button>;
}
