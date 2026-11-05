# Rails applications

Rails applications run either **natively** through the release engine (the
default for new applications whose repository builds natively) or on the
**container runtime** from the repository's production Dockerfile. Both appear
in the same Applications view with runtime badges and share deployment history,
logs, stop/start, reload, rollback, domains, HTTPS, encrypted settings and
retained data.

## Native Rails

Choose a repository in **New application**; detection reads `Gemfile`,
`Gemfile.lock`, `.ruby-version`, `config/database.yml` and `package.json`, and
proposes the Ruby/Node versions, web and worker processes (for example Sidekiq
or Solid Queue), and PostgreSQL/Redis needs for confirmation. The agent
then creates a dedicated system user, an application root with
`releases/`, `current` and `shared/`, hardened systemd units, and, when chosen,
the application's own PostgreSQL database and non-superuser role and its own
Redis instance.

Each deploy runs `bundle install` with the locked gems, precompiles assets with
database access when the app needs it, takes a verified pre-deployment backup of
the protected databases, runs the migrations when the app has them, switches `current`, restarts the
running units and probes the readiness path (`/up` by default). Schedules from
the Whenever gem are rendered by the gem itself and installed as the owner's
crontab block. See [native releases](native-release.md) for the policy, the
confinement of build and release commands, and the API.

Use backward-compatible migrations: rolling back re-links the previous release
but does not reverse schema changes.

## Container Rails

Supply a repository, branch, server, domain, and readiness path (`/up` by
default). Rails 7.1+ supplies `/up`; it proves that the application booted, not
that all external dependencies work. An app can provide a stronger endpoint and
select that path instead. The container runtime supports the standard
production Dockerfile contract:

- Regular `Gemfile`, `Gemfile.lock`, `Dockerfile` and `bin/rails` files.
- Working directory `/rails`, non-root UID/GID `1000:1000`, and a production
  `bin/docker-entrypoint`. The agent runs `./bin/rails server` with `PORT=3000`,
  overriding a Thrust wrapper's port 80 default. Puma must bind all container
  interfaces and honor `PORT`.
- The repository entrypoint owns database preparation. Standard Rails
  entrypoints run `db:prepare` for `./bin/rails server`. The agent's existing
  pre-deployment backup runs before container startup. Use backward-compatible
  migrations: retaining the old container cannot undo database schema changes.
- Supply the **original** `RAILS_MASTER_KEY`, `SECRET_KEY_BASE`, and any Active
  Record encryption keys as credentials. The agent does not generate Rails keys
  or copy Go authentication, admin, mail or migration defaults into Rails apps.
- Database attachments provide `DATABASE_URL`. Multiple PostgreSQL attachments
  remain supported. Configure Redis/Sidekiq through `REDIS_URL`; a managed cache's
  `CACHE_URL` is also exposed as `REDIS_URL` at runtime when no explicit
  `REDIS_URL` is supplied. Neither value is put in command arguments.

The app root is read-only. Writable temporary/log directories are memory-backed.
One retained, app-owned Docker volume contains `storage`, `public-system` and
`public-uploads` subdirectories, mounted at `/rails/storage`,
`/rails/public/system` and `/rails/public/uploads`. Docker must support volume
subpaths (26+). SQLite databases must reside in persistent storage; custom paths,
shared config files, or other mounts need an explicit app adaptation.

Use workers/schedules with argument lists such as `["./bin/jobs"]` or
`["bundle", "exec", "sidekiq"]`. They share the Rails user, environment, private
service networks and persistent files. Coordinate `SOLID_QUEUE_IN_PUMA` with
separate worker tasks to avoid duplicate job processing.

Go stack profiles, framework customer-host approval and PR previews are
unavailable for Rails. Both standard and Dockerfile-specific ignore files are
hardened in the disposable checkout to exclude `.env` files, `.bundle`, master
keys and `*.key` files.

## Observe existing applications

Applications → **Import existing applications for observation** accepts an
inventory JSON plus a source server label. Observations appear beside managed
apps with an **Observed** badge. They are separate identities, have no
management buttons, and are never sent to an agent. Importing does not verify
runtime health or authorize adoption. Reimporting replaces observations for that
exact source. Only an allowlist of metadata is saved; extra commands and
environment values in an uploaded file are discarded. The import requires
administrator infrastructure permission.

## Moving between servers

See [moving applications](moving-applications.md). A move preserves app keys,
database contents, uploads and schedules, verifies the destination before DNS
changes, and keeps the source serving until the switch completes.
