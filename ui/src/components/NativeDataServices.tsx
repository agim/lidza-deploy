import { Database, HardDrive } from 'lucide-react';
import { useEffect, useState } from 'react';
import { get } from '../lib/api';
import { formatBytes, formatDate } from '../lib/format';
import { detailHref, href, navigate } from '../lib/router';
import { Button, Card, Loading } from './ui';

type Backups = { backups?: number; last_backup?: string; backup_error?: string; measure_error?: string; size_bytes?: number; ready?: boolean; resource?: string };
export type NativeDataView = {
  app_id: string;
  postgres?: Backups & { engine: string; version?: number; database: string; role?: string; port?: number };
  redis?: { engine: string; unit: string; active: string; ready?: boolean; used_memory_bytes?: number; max_memory_bytes?: number; persistence?: string; backup?: string; measure_error?: string };
  clickhouse?: Backups & { engine: string; version?: string; database: string; tables?: number; rows?: number };
};

/**
 * Data & storage of a native application: the PostgreSQL, Redis and
 * ClickHouse provisioned on the server for it. An application whose data
 * lives elsewhere (for example an adopted one) has none provisioned.
 */
export function NativeDataServices({ appID }: { appID: string }) {
  const [view, setView] = useState<NativeDataView>();
  const [missing, setMissing] = useState('');
  const [loading, setLoading] = useState(true);
  useEffect(() => {
    let active = true;
    setLoading(true);
    get<NativeDataView>(`control/native-apps/${encodeURIComponent(appID)}/data`)
      .then(value => { if (active) { setView(value); setMissing(''); } })
      .catch(reason => { if (active) { setView(undefined); setMissing(reason instanceof Error ? reason.message : String(reason)); } })
      .finally(() => { if (active) setLoading(false); });
    return () => { active = false; };
  }, [appID]);
  if (loading) return <Loading label="Loading data services…" />;
  const none = !view?.postgres && !view?.redis && !view?.clickhouse;
  if (none) return <Card className="resource-setting"><Database size={23} /><div><h2>Data services</h2><p>No database or cache was provisioned on the server for this application. Databases it uses that the server backs up are listed under Databases & backups.</p>{missing && <p className="muted-copy">{missing}</p>}</div><Button type="button" onClick={() => { navigate(href('databases')); }}>Databases & backups</Button></Card>;
  const database = (title: string, value: Backups & { engine: string; database: string }, detail: string) => <Card className="resource-setting"><Database size={23} /><div><h2>{title}</h2><p>{detail}{value.size_bytes ? ` · ${formatBytes(value.size_bytes)}` : ''}</p><p>{value.ready ? 'Ready' : 'Not ready'} · {value.backups ? `${value.backups} backup${value.backups === 1 ? '' : 's'}${value.last_backup ? `, latest ${formatDate(value.last_backup)}` : ''}` : 'No backups yet'}</p>{value.backup_error && <p className="text-danger">{value.backup_error}</p>}{value.measure_error && <p className="muted-copy">{value.measure_error}</p>}</div>{value.resource && <Button type="button" onClick={() => { navigate(detailHref('database', value.resource!)); }}>Manage backups</Button>}</Card>;
  return <div className="resource-settings-grid">
    {view?.postgres && database('PostgreSQL', view.postgres, `Database ${view.postgres.database}${view.postgres.version ? ` · PostgreSQL ${view.postgres.version}` : ''} on the server`)}
    {view?.clickhouse && database('ClickHouse', view.clickhouse, `Database ${view.clickhouse.database}${typeof view.clickhouse.tables === 'number' ? ` · ${view.clickhouse.tables} tables` : ''}`)}
    {view?.redis && <Card className="resource-setting"><HardDrive size={23} /><div><h2>Redis</h2><p>Service {view.redis.unit} · {view.redis.active}</p><p>{typeof view.redis.used_memory_bytes === 'number' ? `${formatBytes(view.redis.used_memory_bytes)} used` : 'Memory not measured'}{view.redis.max_memory_bytes ? ` of ${formatBytes(view.redis.max_memory_bytes)}` : ''}{view.redis.persistence ? ` · persistence ${view.redis.persistence}` : ''}</p>{view.redis.measure_error && <p className="muted-copy">{view.redis.measure_error}</p>}</div></Card>}
  </div>;
}
