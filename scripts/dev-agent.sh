#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/env.sh
source .local/dev.env
exec bin/lidza-agent -config .local/agent.json
