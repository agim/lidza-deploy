#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/env.sh
# Run each target separately; Go only fuzzes one target per invocation.
for target in FuzzAppEnvironment FuzzCacheConnection; do
  go test ./internal/agent -run '^$' -fuzz "^${target}$" -fuzztime "${FUZZ_TIME:-30s}" -parallel 2
done
