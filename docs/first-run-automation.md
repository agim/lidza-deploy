# Installation and first-run contract

The owner requires installation and configuration to be product workflows, not a list of manual server tasks.

## Available now

The agent bundle installs Git, Docker and Caddy, private credentials, configuration and services on a dedicated supported host. It checks package signatures, its bundle checksums, Caddy configuration and the agent API. Staging mode is tested without touching host services; live installation still needs a fresh supported VM acceptance run.

On deployment, a missing Dockerfile is generated in the disposable checkout through the released Līdza scaffold API. The app's framework/toolchain versions are used, user-authored files are retained, and readiness uses the framework's real `/readyz` endpoint. No generated files are pushed upstream.

## Framework response: v0.1.70 released

Both issues are closed. [#24](https://github.com/agim/lidza/issues/24) assigns setup UI, ownership, provisioning and DNS to this product. Reuse `lidza.Boot` / `Booted.Handler` to start packs after configuration, framework `credentials.Set` for atomic encrypted storage, and framework auth for the first operator. This is not a bypass of the framework-first rule: the maintainer explicitly confirms the existing APIs are sufficient.

[#25](https://github.com/agim/lidza/issues/25) delivers app-version/toolchain pinning and Rust builder support in `scaffold.DeployFiles`, now used here. It declines missing-manifest inference: every Līdza app has `lidza.json`. Temporary build contexts also remain product-owned. Do not wait for another release or claim either issue is still open.

## Implemented product wizard

1. Claim the installation using a one-time installer credential; avoid exposing an unclaimed admin account to the public Internet.
2. Create the initial operator account and generate encryption/session keys through framework facilities.
3. Choose managed PostgreSQL on the installation host or connect an existing PostgreSQL server. Provision/test the selected database, persist credentials securely, and recover cleanly after interrupted setup.
4. Set the control-panel hostname and contact email. Offer authorized DNS-provider integration where supported, otherwise display the exact required records and verify resolution before enabling public HTTPS. A hostname alone does not authorize editing a DNS zone.
5. Configure GitHub from the browser. Guide the OAuth app registration/authorization and callback, persist credentials through the framework, and verify private-repository access. Secrets are entered only in the protected setup UI.
6. Pair the local or remote agent and verify its authenticated API. Finish setup only after required checks pass, then permanently close the bootstrap entry point and open the approved Signal workspace.

Provisioning Docker/PostgreSQL and DNS-provider actions are deployment-product hooks; framework bootstrap handles lifecycle, ownership, validation and secure persistence. Subsequent settings changes must remain in the GUI rather than requiring env-file edits.

## Repository preparation

Use the existing manifest and framework deployment generator in the disposable clone. Preserve custom Dockerfiles and use runtime readiness. Repositories missing `lidza.json` must be reported as missing required Līdza project metadata; automatic inference is not supplied or promised by the framework. External-service credentials cannot be invented from generated build files.

Validation: the real browser wizard provisions managed PostgreSQL, creates the operator, closes setup and restarts successfully. Installer staging includes both services and preserves private pairing. DNS-provider and Caddy transformations are tested with provider mocks; real DNS/ACME and fresh-VM apt/systemd installation remain unverified. GitHub configuration after boot without connectors is fixed by v0.1.71 (#26), and its regression now passes.

The installer now requires `--fqdn HOST` or explicit `--fqdn localhost`. Public installations configure the GUI Caddy route and allowed setup origin before first boot, making the wizard directly available over HTTPS. The wizard locks the installer-selected address. Local mode creates no public GUI route; remote access uses an SSH tunnel. Missing or invalid FQDNs stop before installation side effects.
