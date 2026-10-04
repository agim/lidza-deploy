# Provenance

Source inspected: `agim/monolithcms-app`, commit `706aee8a6eed6c456338c7b36b4142254ff8bdaa`, directory `agent/`.

- `internal/agent/network.go` is copied from the source with its package renamed.
- `internal/agent/config.go` retains the source's private persistent configuration approach.
- `internal/agent/server.go` adapts its authenticated JSON API and asynchronous provisioning structure.
- PHP/Apache provisioning, bundled CMS assets, database maintenance, root-only host changes, and automatic registration were replaced by Līdza container releases, explicit authenticated configuration, readiness checks, and Caddy ingress.

The original source checkout and its upstream repository are unchanged. Framework code is consumed as the pinned Go module `github.com/agim/lidza v0.1.70`; it is not forked into this repository. Reusable OAuth support was requested in agim/lidza#23 and integrated after its release.
