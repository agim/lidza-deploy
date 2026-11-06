import { ArrowRightLeft, ArrowUpRight, Bell, CheckCircle2, ChevronDown, Clock3, Cog, Loader2, PackageOpen, RefreshCw, X, XCircle } from 'lucide-react';
import { useEffect, useState } from 'react';
import { activeCount, elapsed, groupActivity, progressLabel, progressPercent, useActivity, type ActivityItem } from '../lib/activity';
import { relativeTime } from '../lib/format';
import { withSticky } from '../lib/router';
import { Sheet, SheetContent, SheetDescription, SheetTitle } from './primitives/sheet';
import { Button, IconButton, ProgressBar } from './ui';

const kindIcons = { move: ArrowRightLeft, deployment: PackageOpen, update: RefreshCw, job: Cog } as Record<string, typeof Cog>;

function StateIcon({ state }: { state: ActivityItem['state'] }) {
  if (state === 'running') return <Loader2 size={13} className="ops-state-icon spin" aria-label="Running" />;
  if (state === 'waiting') return <Clock3 size={13} className="ops-state-icon" aria-label="Waiting" />;
  if (state === 'failed') return <XCircle size={13} className="ops-state-icon" aria-label="Failed" />;
  return <CheckCircle2 size={13} className="ops-state-icon" aria-label="Succeeded" />;
}

function ActivityRow({ item, now, onOpen, onDismiss }: { item: ActivityItem; now: number; onOpen: () => void; onDismiss?: () => void }) {
  const [expanded, setExpanded] = useState(false);
  const Icon = kindIcons[item.kind] || Cog;
  const finished = item.state === 'succeeded' || item.state === 'failed';
  const time = finished ? relativeTime(item.updated) : elapsed(item.started || item.updated, now);
  const line = item.state === 'waiting' ? item.waiting_on || item.message : item.message;
  const detailID = `ops-detail-${item.id.replace(/[^A-Za-z0-9_-]/g, '-')}`;
  return <li className={`ops-item ops-${item.state}`} data-testid="ops-item">
    <div className="ops-row">
      <button type="button" className="ops-summary" aria-expanded={expanded} aria-controls={detailID} onClick={() => setExpanded(value => !value)}>
        <span className="ops-kind"><Icon size={16} strokeWidth={1.7} /></span>
        <span className="ops-main">
          <span className="ops-title">{item.title}</span>
          <span className="ops-meta"><StateIcon state={item.state} />{item.server_name && <span className="ops-server">{item.server_name}</span>}{time && <span>{finished ? time : `${time} elapsed`}</span>}</span>
          {line && <span className="ops-message">{line}</span>}
          {item.progress && item.progress.total > 0 && !finished && <span className="ops-progress"><ProgressBar value={progressPercent(item.progress)} tone={item.state === 'waiting' ? 'warning' : 'accent'} /><small>{progressLabel(item.progress)}</small></span>}
        </span>
        <ChevronDown size={14} className="ops-chevron" aria-hidden="true" />
      </button>
      {onDismiss && <IconButton className="ops-dismiss" label={`Dismiss ${item.title}`} onClick={onDismiss}><X size={14} /></IconButton>}
    </div>
    {expanded && <div className="ops-detail" id={detailID}>
      {item.waiting_on && item.state !== 'waiting' && <p><strong>Waiting on</strong> {item.waiting_on}</p>}
      {item.details.length > 0 && <ul>{item.details.map((detail, index) => <li key={index}>{detail}</li>)}</ul>}
      {item.started && <p className="ops-times">Started {new Date(item.started).toLocaleString()}{finished ? ` · finished ${relativeTime(item.updated)}` : ` · updated ${relativeTime(item.updated)}`}</p>}
      {item.link && <a className="ops-open" href={withSticky(item.link)} onClick={onOpen}>Open<ArrowUpRight size={13} /></a>}
    </div>}
  </li>;
}

function Group({ label, items, now, onOpen, onDismiss }: { label: string; items: ActivityItem[]; now: number; onOpen: () => void; onDismiss?: (ids: string[]) => void }) {
  if (!items.length) return null;
  return <section className="ops-group" aria-label={label}>
    <header><h3>{label}<span>{items.length}</span></h3>{onDismiss && <Button size="sm" variant="ghost" onClick={() => onDismiss(items.map(item => item.id))}>Clear</Button>}</header>
    <ul>{items.map(item => <ActivityRow key={item.id} item={item} now={now} onOpen={onOpen} onDismiss={onDismiss ? () => onDismiss([item.id]) : undefined} />)}</ul>
  </section>;
}

/** ActivityDrawer is the header's activity button and the drawer it opens. */
export function ActivityDrawer() {
  const [open, setOpen] = useState(false);
  const { items, error, loaded, dismissed, dismiss } = useActivity(open);
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!open) return;
    setNow(Date.now());
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [open]);
  const groups = groupActivity(items, dismissed);
  const count = activeCount(items);
  const failed = groups.finished.some(item => item.state === 'failed');
  const label = count ? `Activity, ${count} in progress` : failed ? 'Activity, a recent operation failed' : 'Activity';
  const close = () => setOpen(false);
  return <>
    <button type="button" className={`ops-trigger${groups.running.length ? ' ops-busy' : ''}`} aria-label={label} title={label} onClick={() => setOpen(true)}>
      <Bell size={17} strokeWidth={1.7} />
      {count > 0 ? <span className="ops-count" data-testid="ops-count">{count > 99 ? '99+' : count}</span> : failed && <span className="ops-alert" aria-hidden="true" />}
    </button>
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetContent side="right" className="ops-drawer" showCloseButton={false}>
        <div className="ops-head">
          <div><SheetTitle>Activity</SheetTitle><SheetDescription>Moves, deployments, updates and background jobs across your servers.</SheetDescription></div>
          <IconButton label="Close activity" onClick={close}><X size={18} /></IconButton>
        </div>
        <div className="ops-body">
          {error && <p className="ops-error" role="alert">Activity could not be refreshed: {error}</p>}
          <Group label="Running" items={groups.running} now={now} onOpen={close} />
          <Group label="Waiting" items={groups.waiting} now={now} onOpen={close} />
          <Group label="Finished recently" items={groups.finished} now={now} onOpen={close} onDismiss={dismiss} />
          {loaded && !groups.running.length && !groups.waiting.length && !groups.finished.length && <div className="ops-empty"><CheckCircle2 size={22} strokeWidth={1.5} /><strong>Nothing in progress</strong><span>Moves, deployments and updates appear here while they run.</span></div>}
        </div>
      </SheetContent>
    </Sheet>
  </>;
}
