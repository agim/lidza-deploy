# Stop and start an application

Open **Applications → More actions → Stop application** and confirm. Admins and
deployers can change this state; viewers cannot.

Stop shuts down the current container, retained previous container, and workers.
Scheduled commands and automatic repository deployments pause. Public requests
receive HTTP 503. Stopped state is saved and enforced after agent restarts;
Docker's `unless-stopped` policy keeps deliberately stopped containers off after
host restarts.

The domain, HTTPS certificate, application configuration, files, database and
cache are retained. Database/cache services and database backups continue, so
stopping an app does not discard data or stop shared infrastructure. OAuth
configuration and credentials are preserved.

Wait for an active deployment, restore, cache operation, scheduled command or
worker startup to finish before stopping. Maintenance mode remains available
when you want the app and workers to continue running behind a maintenance page.

Choose **Start application** to queue a checked reload of the current image.
Settings changed while stopped, including environment variables and database
attachments, are applied to the new container. Follow **Deployments → Build logs**
for backups, readiness and any startup failure. The app stays unavailable until
the candidate passes `/readyz`; a failed start leaves it stopped. Enabled workers
and schedules resume on the next scheduler tick after successful activation.

Start uses the last successfully deployed image. Pushes received while stopped
are acknowledged without deploying and are not replayed on Start; use **Deploy**
after starting to fetch the latest repository commit. An app that never deployed
successfully can also be stopped; Start enables it for its next deployment.
