# Stop, start and restart an application

Container, stack and native applications (created, adopted or static) share the
same controls. Open **More** on the application's card: the **Application** group
has **Restart**, **Stop**/**Start** and **Delete**. Stop/Start and Delete are also
in the **Danger zone** at the end of the General settings. Admins and deployers
can use them; viewers cannot.

The buttons are disabled, with the reason on hover, while a deployment is
queued or building, while the application is moving or being removed, or when
the server's agent is unavailable or cannot control the application yet.

**Restart** restarts every process; visitors may see errors for a few seconds.
Units the operator keeps disabled are not started.

**Stop** shuts down the application and its workers and keeps it down after a
reboot. Scheduled commands and automatic repository deployments pause and
visitors see a maintenance page. For containers, the current container, the
retained previous container and workers stop, and Docker's `unless-stopped`
policy keeps them off after host restarts. For native applications the stopped
state is saved first, every unit is disabled, the crontab block is paused and
the stopped page is published in Caddy. A stopped application refuses deploys
and per-service starts; its card shows the stopped badge with **Start** as its
primary action.

The domain, HTTPS certificate, configuration, files, database and cache are
retained. Database/cache services and database backups continue, so stopping an
app does not discard data or stop shared infrastructure. Redirect domains keep
working.

**Start** brings back exactly the units that ran (native) or queues a checked
reload of the last successfully deployed image (containers). Settings changed
while stopped, including environment variables and database attachments, are
applied. Follow **Deployments → Build logs** for readiness and startup failures;
a failed start leaves the app stopped. Pushes received while stopped are not
replayed on Start; use **Deploy** afterwards for the latest commit.

Maintenance mode remains available when you want the app and workers to keep
running behind a maintenance page. To remove an application, see
[deleting and restoring applications](deleting-applications.md).

API: `POST /api/control/apps/{id}/stop`, `/start` and `/restart` (agent:
`POST /v1/apps/{id}/stop|start|restart`). They are refused during a move.
