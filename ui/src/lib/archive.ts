// Deleted applications: what deletion keeps and deletes, and the archive
// records the panel lists (GET /api/control/archives).

export const ARCHIVE_RETENTION_DAYS = 90;

export interface ArchiveItem { kind: string; name: string; size_bytes?: number; measured?: boolean; count?: number; detail?: string }
export interface ArchiveEvent { at: string; message: string }
export interface ArchiveRow {
  id: string; app_id: string; kind: 'container' | 'stack' | 'native'; runtime?: string; repository?: string; branch?: string;
  deleted_at: string; deleted_by?: string; reason?: string; expires_at: string; phase: 'deleting' | 'archived' | 'restoring' | 'purging'; error?: string;
  hostnames: string[]; commit?: string; kept: ArchiveItem[]; deleted: ArchiveItem[]; kept_bytes: number; size_pending?: boolean; log?: ArchiveEvent[];
  id_available: boolean; server_id: string; server_name: string; days_left: number; webhook?: string; move_copies?: string; unavailable?: boolean;
}
export interface ArchivePurge { archive_id: string; app_id: string; at: string; trigger: 'expiry' | 'manual'; by?: string; deleted: ArchiveItem[]; server_id: string; server_name: string }
export interface ArchivesResponse { archives: ArchiveRow[]; purged: ArchivePurge[]; retention_days: number; unavailable_servers: string[] }
export interface RestoreResponse {
  result: { app_id: string; hostnames: string[]; skipped?: Record<string, string>; commit?: string };
  webhook?: string; deploy_error?: string; deployment?: { id: string };
}

/** restorableUntil is the last day a deletion made at now can be restored. */
export function restorableUntil(now = new Date(), days = ARCHIVE_RETENTION_DAYS) {
  return new Date(now.getTime() + days * 24 * 60 * 60 * 1000);
}

export function formatDay(value: Date | string) {
  const date = typeof value === 'string' ? new Date(value) : value;
  return date.toLocaleDateString(undefined, { year: 'numeric', month: 'long', day: 'numeric' });
}

/** deletionInventory says, per runtime, what deleting an application keeps and deletes. */
export function deletionInventory(app: { native?: unknown; runtime?: string; runtime_profile?: string; management_capabilities?: { adopted?: boolean } }) {
  if (app.native && app.management_capabilities?.adopted) return adoptedInventory;
  const native = Boolean(app.native);
  const kept = [
    'Settings: environment, database bindings, hostnames, redirect domains and scheduled tasks',
    native ? 'Application files (moved to a folder only root can open)' : 'Files: the storage volume and stack volumes',
    native ? 'Host PostgreSQL and ClickHouse databases, with their logins disabled' : 'Databases and ClickHouse, with their containers stopped so nothing can log in',
    'S3 application storage, untouched',
  ];
  const deleted = [
    native ? 'Redis instance and its data' : 'Cache (Valkey container and its data)',
    'Every backup of its databases, on the server and off-site (S3)',
    'Caddy route, redirect routes and TLS certificates nothing else uses; the hostnames become free at once',
    native ? 'Systemd units, the system user and its crontab' : 'Containers, images, published ports and their firewall openings',
    'Scheduled tasks, workers, the gateway identity, the GitHub webhook and stored console errors',
    'Copies of its data that moves left on other servers',
  ];
  return { kept, deleted };
}

/** What deleting a native application adopted from the existing host setup keeps and deletes. */
const adoptedInventory = {
  kept: [
    'Settings: environment, database bindings, hostnames, redirect domains and scheduled tasks',
    'Application directory (moved to a folder only root can open); the shared deployment account, its home and other applications stay as they are',
    'Host PostgreSQL databases: a role only this application uses loses its login; a role shared with another application only loses access to this database',
    'S3 application storage, untouched',
  ],
  deleted: [
    'Its Redis database on the shared Redis, when no other application uses the same database number (a shared one is kept)',
    'Every backup of its databases, on the server and off-site (S3)',
    'Its web server routes, redirect routes and TLS certificates nothing else uses; the hostnames become free at once',
    'Its systemd units (stopped, disabled, unit files removed) and its crontab schedule; the deployment account itself is unchanged',
    'Scheduled tasks, the GitHub webhook and stored console errors',
    'Copies of its data that moves left on other servers',
  ],
};

export function itemLabel(item: ArchiveItem) {
  const labels: Record<string, string> = { settings: 'Settings', volume: 'Volume', directory: 'Files', database: 'Database', clickhouse: 'ClickHouse', cache: 'Cache', backups: 'Backups', route: 'Route', redirects: 'Redirects', certificate: 'Certificate', certificates: 'Certificates', ports: 'Ports', 'move-copy': 'Move copy', releases: 'Releases', tasks: 'Tasks', units: 'Units', account: 'Account', 's3-objects': 'S3 objects' };
  return labels[item.kind] || item.kind;
}
