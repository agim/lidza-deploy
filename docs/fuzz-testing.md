# Fuzz testing

Run `bash scripts/fuzz.sh` from the checkout. It activates the pinned Go toolchain and fuzzes application environment validation and authenticated cache URL validation for 30 seconds each, with two workers. Override the duration with `FUZZ_TIME=2m bash scripts/fuzz.sh`.

These tests run locally without touching deployed servers. They exercise malformed input, injection characters, reserved environment names, and cache authentication requirements. The ordinary Go test suite also executes their seed corpus. Go saves any failing fuzz inputs under `internal/agent/testdata/fuzz`; retain these as regression cases after fixing a defect.

Fuzz testing complements integration tests and security review; it does not establish that the deployed website has no vulnerabilities.
