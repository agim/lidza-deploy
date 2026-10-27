# Līdza v0.1.128 tooling measurements

Measured on an otherwise idle cloud executor, with no dev server or concurrent
builds. Baseline: main 654ce78, framework/CLI v0.1.125. Updated CLI and module:
v0.1.128. Go toolchain: 1.27.3. Timings below are wall-clock seconds, using
Bash `time`; tool installation/downloads are excluded.

| Command | Before | After | Result |
| --- | ---: | ---: | --- |
| `lidza check --json`, first measured call | 2.852 | 3.270 | Both pass; after includes a fresh dependency scan |
| `lidza check --json`, next call | 2.872 | 2.676 | Pass; after skips dependency scanners |
| `lidza test` | 4.422 (failed) | 28.210 | Before failed on credential isolation; after passes |
| `lidza verify` after passing check/test | Not available | 0.060 | Both successful steps reused |
| `go test ./...` on unchanged baseline | 25.216 | See `lidza test` above | Baseline passes outside the CLI sandbox |
| Fresh MCP initialization + tools/list | Not measured | 0.013 / 0.012 | Two separate server processes, 29 tools immediately |
| MCP `lidza_routes` | Not measured | 0.032 | Successful response after reconnect |
| MCP `lidza_notes`, grep `dmarc` | Not available | Not available | Plain Go module: tool is not registered |

The initial cold check before the corrected baseline took 21.316 seconds; its
next call took 2.715. Do not attribute that compiler-cache warmup to the update.
The baseline verify failed in 5.023 seconds after its check passed; the baseline
CLI test failed in 4.422 seconds. These failures are not valid performance wins.

## Repository-specific findings

This is Lidza Deploy, not the separate Albaspot app whose large guide, 147 notes,
52 recipes and approximately 580-second tests package motivated the request.
Deploy has no lidza.json, schema.lidza, docs/lidza-guide.md, app recipes or team
notes to migrate. It has no master branch; its default branch is main.
AGENTS.md remains exactly 1,631 bytes, byte-for-byte unchanged, including every
application instruction. No generated guidance/skills were added or overwritten.
`lidza update --to v0.1.128` changed go.mod/go.sum and removed obsolete checksum
entries. `.lidza/` inspection/cache/stamp files are now explicitly ignored.
There was no convention change, so docs/decisions.md was not changed.

The old CLI deliberately removes shell master keys and disables credential-file
reads for tests. Deploy's managers generate encrypted temporary state, so their
tests failed under this sandbox even though direct go test passed. A shared
test-only helper now generates a random per-package key inside the test process;
onboarding fixtures that clear settings install their own fresh test key. The
key-file suppression remains enabled; runtime credential code is unchanged.
No existing test or assertion was removed, skipped or weakened.

Both post-update check reports have status ok and no L024. The second reports
"dependencies unchanged since the last scan" for govulncheck, npm audit and cargo
deny. However, govulncheck and staticcheck are not installed on this executor;
there is no Cargo.toml, root tsconfig.json or node_modules. Before and after use
the same conditions. The meaningful security scan timed here is npm audit, not a
full govulncheck/staticcheck/frontend validation. Existing npm audit warnings
include Vite/launch-editor/esbuild advisories; a skipped rescan suppresses the
repeated warning, it does not resolve those advisories. The repeated check is
about 7% faster here, not three times faster. Go vet and the infrastructure-heavy
rules remain most of the approximately 2.7-second repeated check.

A fresh stdio MCP connection was initialized twice and tools/list called before
any other tool. This exercises real server restart/reconnect, without claiming a
Claude UI session was restarted. The plain-Go toolset exposes lidza_routes but
not lidza_notes; its explicit call returns an unknown-tool error. No framework
repository files were changed to work around this limitation.

## Test layout

There are no Go test files directly under tests/. Its four child directories are
executable Docker fixtures, not the approximately 580-second integration test
package described in the request. Existing tests already run in feature packages:
internal/agent, internal/control, internal/onboarding, internal/providers/github,
internal/webapp and web. The measured agent package is approximately 24 seconds.
Moving private agent tests into domains/dmarc/billing packages would change their
access and architecture without addressing an observed 580-second bottleneck.
No unrelated Albaspot tests were moved. Optional Docker/Caddy/database integration
tests still require their documented opt-in environment and fixtures.
