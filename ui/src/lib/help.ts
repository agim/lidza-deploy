// The Help page's articles: short, task-oriented guides that name the console
// labels they refer to. Keep them free of real customer names, domains and
// addresses (help.test.ts checks); use example.com and documentation addresses.

export interface HelpArticle { id: string; title: string; body: string[]; keywords?: string[] }

export const helpArticles: HelpArticle[] = [
  { id: 'first-administrator', title: 'First administrator', keywords: ['admin', 'ADMIN_USERS'], body: ['In an application that uses the framework’s admin pages, the first account to sign in is an administrator. To admit more, set ADMIN_USERS to a comma-separated list of user IDs or email addresses, for example admin@example.com,ops@example.com.'] },
  { id: 'database-migrations', title: 'Database migrations', keywords: ['DB_MIGRATE', 'db:prepare', 'rails', 'schema'], body: ['New Lidza (Go) applications start with DB_MIGRATE=true, a workspace default under Application defaults, so they apply their schema before startup; existing applications keep their setting, including an explicit false. Ruby on Rails applications do not get DB_MIGRATE: the repository’s entrypoint prepares the database, and standard Rails entrypoints run db:prepare. Keep migrations backward compatible during zero-downtime activation.'] },
  { id: 'environment-variables', title: 'Environment variables', body: ['Use Setting for non-sensitive values and Credential for secrets. Saved credentials stay encrypted and masked.'] },
  { id: 'backups', title: 'Backups and recovery', keywords: ['restore', 'off-site', 's3'], body: [
    'Keep local and private off-site copies. Regularly test restoration into a separate database.',
    'Deleting an application deletes every backup of its databases, on the server and off-site, while the live databases stay restorable from Deleted apps.',
  ] },
  { id: 'backup-skip-tables', title: 'Leaving table data out of backups', keywords: ['exclude', 'logs', 'excluded tables'], body: [
    'To keep the rows of disposable tables (logs, caches, event streams) out of backups, open the database under Databases & backups and choose Choose tables in Backups skip data of these tables. Select the tables, then Save skipped tables.',
    'Their schema stays in every backup, so a restore recreates them empty. At least one table must keep its data. A move lists these tables and asks, per table, whether to copy its data or leave it behind.',
  ] },
  { id: 'dns-https', title: 'DNS and HTTPS', keywords: ['certificate', 'tls', 'a record', 'aaaa'], body: ['Point every A and AAAA record to the hosting server. The agent rechecks DNS and Caddy issues HTTPS when all records arrive.'] },
  { id: 'redirect-domains', title: 'Redirect domains', keywords: ['301', '302', '307', '308', 'redirect', 'old domain', 'www'], body: [
    'To add a redirect domain, open the application’s settings, General tab, and choose Add redirect domain under Redirect domains. Enter the Hostname (for example old.example.com), pick where it should Redirect to (the primary hostname, another hostname of the application or a Custom hostname…) and a Status: 301 or 308 for a permanent move, 302 or 307 for a temporary one. Then Save redirects.',
    'Keep path and query (on by default) sends old.example.com/a?b to the same path on the target; off, every request goes to the target’s home page. Point the hostname’s DNS to the server with the records shown under it; the certificate is issued once DNS arrives, and the line under the hostname says when HTTPS is ready.',
    'Redirects are answered by the web server, not the application, so they keep working while it is stopped or in maintenance. A hostname can belong to only one application, and a redirect cannot point at another redirect domain.',
  ] },
  { id: 'app-controls', title: 'Stopping, starting and restarting', keywords: ['stop', 'start', 'restart', 'danger zone', 'maintenance page'], body: [
    'To restart an application, open More on its card and choose Restart: every process restarts and visitors may see errors for a few seconds. Stop and Start are in the same menu and in the Danger zone at the bottom of the General settings.',
    'A stopped application stays stopped after a reboot, its scheduled jobs pause and visitors see a maintenance page; its data is kept. The card then offers Start instead of Deploy.',
    'The buttons are disabled, with the reason on hover, while a deployment is queued or building, while the application is moving or being removed, or when the server’s agent is unavailable or cannot control this application yet.',
  ] },
  { id: 'delete-app', title: 'Deleting an application', keywords: ['delete', 'remove', 'archive', 'danger zone', '90 days'], body: [
    'To delete an application, choose Delete in its card’s More menu or Delete application in the Danger zone, read the two lists in the dialog, and type the application id. A reason is optional. Deletion is refused while it deploys or moves.',
    'Kept for 90 days: its settings (environment, hostnames, redirect domains, scheduled tasks), its files and its databases, which stay in place with every login disabled. Deleted at once: the cache, every local and off-site backup, routes and certificates nothing else uses, its units or containers, scheduled tasks and the webhook. It stops serving immediately and its hostnames are free for other applications.',
  ] },
  { id: 'deleted-apps', title: 'Restoring a deleted application', keywords: ['restore', 'undelete', 'archive', 'purge', 'deleted apps'], body: [
    'To restore an application, open Workspace → Deleted apps and choose Restore. It is recreated from its settings on the same server with its files and databases, an empty cache, the hostnames no other application took meanwhile, and a new deployment of its branch. If another application took its id, the dialog asks for a new id (a native application keeps its own: delete or rename the other one first); a New primary domain can be given if its domain was taken. Backups start again with the next scheduled backup.',
    'Each row shows how many days are left. After 90 days the server deletes the application permanently, databases included, even while the control panel is offline; Delete now does the same at once. An application adopted from the server’s existing setup is restored with all its hostnames or not at all, and its deletion leaves the shared account and other applications untouched.',
  ] },
  { id: 'resource-tuning', title: 'Database and cache sizing', keywords: ['postgresql', 'redis', 'clickhouse', 'tuning', 'connections', 'memory', 'resources'], body: [
    'Each server sizes PostgreSQL, Redis and ClickHouse in proportion to its CPUs, memory, disk and number of applications. It recomputes at agent start, so after every reboot or resize, and hourly. See Resources on the server page; Review changes shows a dry run and Apply now applies it.',
    'Every application gets its own PostgreSQL connection cap from its observed peak and configured pool, never below the sessions in use; computed caps wait for a day of samples. Redis caches evict old keys, while queues and persistent data never evict and warn above 80% of their limit. Settings that need a PostgreSQL restart wait for the server’s maintenance window.',
    'Any value can be overridden; Save overrides, then review and apply. Pause automatic tuning keeps the targets visible without applying them. Move to its own Redis gives an application on the shared Redis a private instance; the old database is cleared only once it runs healthily there.',
  ] },
  { id: 'move-app', title: 'Moving an application to another server', keywords: ['move', 'migrate', 'server', 'replication', 'cutover', 'final sync'], body: [
    'To move an application, open Move to another server on its card, choose the Destination server and Review move. The plan lists databases, files, ports, warnings and estimates; resolve anything under Resolve before moving, confirm I have read these warnings when there are any, then Start move. The application stays live while the destination is prepared, its data and files copied and the destination deployed.',
    'With Database copy on Automatic, PostgreSQL databases replicate continuously through the control panel (or a private network both servers share), so no port needs to open between the servers; a database that cannot replicate is copied with a dump. Failed steps retry by themselves and can be resumed with Retry; Abort move removes the destination copy and keeps the source serving.',
    'The final sync locks the application, puts the source and the destination in maintenance, copies the latest changes and redeploys. With Final sync and finish automatically once DNS points to the destination, it runs and finishes by itself as soon as DNS and HTTPS work there; otherwise use Final sync and Finish move.',
  ] },
  { id: 'move-dns', title: 'DNS during a move', keywords: ['dns', 'cutover', 'nameserver', 'reverse dns', 'ptr', 'spf', 'mail', 'cdn'], body: [
    'Keep DNS unchanged until the move reaches Switch DNS. Then set the records it lists at your DNS provider and remove every other A and AAAA record of those hostnames. The check runs every minute at the zone’s own nameservers, so resolver caches do not delay it; I\'ve switched DNS — check again checks at once.',
    'An application that sends mail from the server itself (it publishes port 25, 465 or 587) also gets SPF records to update and a reverse DNS (PTR) value to set at the server provider. Reverse DNS is optional: the move never waits for it. When the environment shows that outgoing mail goes through a provider such as Mailgun, reverse DNS does not affect it and is not checked at all. For hostnames behind a proxy or CDN, tick These hostnames are served through a proxy or CDN and use Finish without DNS match.',
  ] },
  { id: 'panel-move', title: 'Moving the control panel', keywords: ['control panel', 'panel', 'move', 'standby', 'cutover'], body: [
    'To move the control panel to another server, open Workspace → Control panel, choose the Destination server and Review the move, then Start the move. The panel is installed there in standby and its state, database and certificate are copied while this panel keeps working.',
    'When it is Standing by, choose Cut over: this panel becomes read-only, copies its latest data and activates the destination; the move can no longer be aborted. Then change the DNS records it shows for the panel’s hostname. Once DNS and HTTPS answer on the destination, the old panel shows a notice and stops. Its address does not change.',
  ] },
  { id: 'activity', title: 'Activity', keywords: ['activity', 'progress', 'jobs', 'notifications', 'bell'], body: [
    'The bell in the header opens Activity: moves, deployments, updates and background jobs across your servers, grouped as Running, Waiting and Finished recently. Its badge counts what is running or waiting, and a dot means a recent operation failed.',
    'Select an entry for its details and Open to go to it. Clear removes finished entries from your list.',
  ] },
  { id: 'mail-tls', title: 'Mail ports and STARTTLS', keywords: ['mail', 'smtp', 'starttls', 'tls', 'public_tls', 'gmail', 'port 25'], body: [
    'Large senders such as Gmail and Microsoft require STARTTLS on port 25 and defer mail when the handshake fails. When a stack publishes a mail port (25, 465 or 587) but no service declares public_tls, its card and the deployment log warn about it.',
    'After each deployment, and every 15 minutes, the server also connects to the published mail ports and completes the TLS handshake with the primary hostname; a failure shows as a warning on the card. Warnings never block a deployment.',
  ] },
  { id: 'docker', title: 'Docker on demand', keywords: ['docker', 'container', 'install'], body: [
    'Servers that run only native applications do not need Docker. The first container application on a server installs Docker Engine from Docker’s official repository and starts it before its deployment, so there is nothing to prepare by hand.',
  ] },
  { id: 'protected-gateway', title: 'Protected gateway', keywords: ['gateway', 'gateway_transport', 'mtls'], body: [
    'An application whose stack declares gateway_transport needs a protected gateway between the web server and the agent. On servers whose Caddy is managed through its admin API, the first protected deployment sets it up automatically and lists Protected gateway setup under Recent host operations on the server page. Servers configured from a Caddyfile still need the reviewed manual setup.',
  ] },
  { id: 'host-updates', title: 'Server updates and maintenance window', keywords: ['updates', 'reboot', 'maintenance window', 'security'], body: [
    'Each server’s page has Operating system updates and an Update policy that runs on the server itself, even while the control panel is offline. Automatic security updates installs the distribution’s security updates unattended. With Apply disruptive updates in the maintenance window on, Docker, PostgreSQL, Redis, Caddy, kernel and systemd upgrades install only inside the window set by Start hour (UTC) and Window length; off, they install only when requested.',
    'With Reboot automatically on, the server reboots only inside that window, at most once every six hours and never while deployments or backups run. PostgreSQL settings that need a restart are applied in the same window.',
  ] },
];

/** searchHelp keeps the articles whose title, keywords or text contain every word of query. */
export function searchHelp(query: string, articles: HelpArticle[] = helpArticles): HelpArticle[] {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (!words.length) return articles;
  return articles.filter(article => {
    const text = [article.title, ...(article.keywords || []), ...article.body].join(' ').toLowerCase();
    return words.every(word => text.includes(word));
  });
}
