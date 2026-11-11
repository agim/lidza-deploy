# Native static Astro settings

An approved native static publisher supports branch, build output directory and
application environment edits through the common Settings page. Changes are
sealed in agent state. Every one is build-time for a static site, so Save
reads **Save and deploy** and the panel starts a deployment that publishes
them; the save itself never mutates the serving release, imported environment
files, services, runtime pins or routing.
Imported environment values are retained in sealed state before the first native
publication so a release-local environment file is not lost with the old release.
Deleted keys remain deleted through explicit tombstones.

Hosting variables (ASDF/NVM, listener, shell and runtime controls) remain
managed. Hostnames, runtime mode/version, database/cache ownership, Rails settings
and unapproved static apps remain guarded. Credential reveal and credential-to-
setting classification retain the dashboard's purpose-bound MFA check. Normal
native settings responses contain no credential values or previews.

The publisher inherits existing Node/tool version files when a checkout omits
host-generated pins. Explicit different or new pins, symlinked pins and build-time
pin mutations refuse publication. Static publication retains the previous release
and hashed assets and atomically swaps current without reloading Caddy or any app
service. Settings cannot change while a native deployment is active.
