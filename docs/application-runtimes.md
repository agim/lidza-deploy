# Application runtimes

New applications run natively when their repository builds natively: Līdza (Go),
Rails, Node and Astro (static or standalone Node) all deploy through the release
engine as systemd services under their own system user, and static sites are
served by Caddy. See [native releases](native-release.md) and
[Rails applications](rails-applications.md). Compose projects with several or
third-party services, and repositories with an unrecognized Dockerfile, use the
container runtime; the agent installs Docker on a server only when such an
application needs it. The wizard shows the reason for its choice and allows an
override.

The rest of this page describes the container runtime.

On the container runtime the agent supports Lidza (Go) and Astro applications. Both use the same
inventory, source/branch settings, encrypted environment, domains, required
database backup, deployment history, readiness, reload and rollback pipeline.
Runtime selection is fixed on creation; choosing a different runtime needs a new
application identity. Optional repository subdirectories are resolved inside the
disposable checkout and cannot escape through `..` or symlinks.

Astro supports static output and the standalone Node adapter. The application
must declare Astro, an npm build script, and a package-lock.json. Default static
output is `dist`; a different output directory can be selected. Standalone Node
requires a production `@astrojs/node` dependency and the configured build must
produce `server/entry.mjs`. Configure the adapter in the app repository as described
in [Astro's deployment guide](https://docs.astro.build/en/guides/deploy/) and
[Node integration guide](https://docs.astro.build/en/guides/integrations-guide/node/).
Deploy does not execute Astro configuration during discovery or settings reads.

When there is no Dockerfile, a production image is generated only in the temporary
checkout. Static output is served by an unprivileged Caddy on port 3000, with its
file capability removed to retain the agent's `cap-drop=ALL` and no-new-privileges
restrictions. The final static image has no inherited anonymous data volumes.
Node output runs its standalone entry point, never the development/preview server.
Existing custom Dockerfiles are retained and must honor the selected port and
readiness path. Node 22 or 24 may be selected.

Only explicitly public `PUBLIC_` values enter the Astro build, through a temporary
0600 BuildKit secret. Runtime database credentials and master keys are not build
arguments. New Astro apps do not receive a Lidza master key or framework defaults.
Go-only stack profiles, customer-host validation and preview deployment are not
silently applied to Astro.

The dashboard checks agent runtime capabilities before creating an Astro or
subdirectory application. Existing Go requests omit new runtime/build fields when
unused, allowing the newer dashboard to continue managing an older Go agent.
Build settings are sent only when the serving agent advertises support.

Validation includes real static and standalone Astro production builds,
readiness and image reloads against an isolated Docker engine, retaining the old
container during the test. These checks do not deploy or change hosted apps.

## Servers ready for native applications

Native applications need a deployment account on the server and its home
named in the agent's configuration (`native.root`, `native.user`). The
installer prepares this on every server it sets up, including servers
connected with **Servers → Connect server**: it creates the `deploy` account
(a login shell, no password; the account managed SSH access uses) and adds the
`native` section, keeping one that is already there.

A server installed before that is prepared by its agent: when an agent that
runs as root from the installer's layout (`data_dir` `/var/lib/lidza-agent`)
starts without a `native` section, which it does after every update, it
creates the account and adds the section before it serves anything, and logs
`native applications enabled`. Agents of other layouts, readers of the web
routes (`ingress_only`) and agents that are not root are left as they are.
If that step fails the agent starts as before and logs why; the server's
**Agent** tab then offers **Enable native applications**, which does the same
on request: the agent creates the account if it
is missing, adds the section to its configuration file (every other key, the
file's mode and owner are kept, and the result must load before it replaces
the file) and restarts itself once. Hosted applications are not touched. A
move of a native application to a server that is not ready says so and points
here. `GET`/`POST /api/control/servers/{server}/host/native` is the API
(`configured`, `user`, `root`, `supported`, `reason`, `restarting`).

