# Declarative configuration and empty-settings bootstrap validation

The version-1 contract is [stack-configuration.md](../stack-configuration.md).
This change consumes released framework production setting metadata and preserves
its default checks. App-declared ownership replaces only missing-value checks for
explicit setup/optional settings; it neither invents provider values nor bypasses
the application's production mode/readiness.

Validation completed:

* `go test -race ./...`: all packages passed.
* `go vet ./...`: passed.
* `npm run typecheck` and `npm run test:ui`: passed, 31 UI tests.
* `TEST_DOCKER=1 go test -race ./internal/agent -run '^TestDockerStack' -v -count=1`:
  all three isolated Docker acceptance tests passed (48.708s).

The Docker lifecycle fixture now enables framework db, storage, mail and llm packs
and declares mail provider/sender as setup-owned and AI provider as optional.
It asserts MAIL_PROVIDER, MAIL_FROM and LLM_PROVIDER are absent before deployment.
A custom missing deploy credential first blocks the actual checkout/preflight
with its label and help, without activating any release. Removing that synthetic
requirement permits real Docker build/start with managed PostgreSQL and the
minimum profile. `/setup` is reachable, protected API/readiness remains503 until
publication, and publication followed by a fresh checkout/deployment passes real
readiness without duplicating app-owned configuration in Deploy's environment.

An established candidate forced to return the exact setup marker fails at app
readiness despite otherwise healthy services. The healthy previous release and
its persistent data are restored. The assertion reconciles Docker's reassigned
host ports and waits for ordinary readiness after the restart. The same suite
covers managed database reuse, profile sizing, sidecar health/inventory, private
routing, volume retention, reload/start/stop, rollback, crash recovery and removal.

Focused regressions cover strict undeclared defaults, configured development
providers (including optional embed), missing infrastructure, conditional optional
settings, conflicting declarations, missing original master key, mismatched sealed
credentials, successful original-key decryption and explicit secret masking.

These are platform fixtures, not a new Thura end-to-end acceptance claim. Thura
owns its manifest integration and genuine production wizard/provider acceptance.
No running production console or customer app was used for these checks.
