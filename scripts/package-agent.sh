#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/env.sh
arch=${1:-amd64}
case "$arch" in amd64|arm64) ;; *) echo 'Supported architectures: amd64, arm64' >&2;exit 2;; esac
bundle=$(mktemp -d /tmp/lidza-agent-bundle.XXXXXX)
trap 'rm -rf "$bundle"' EXIT
install -d "$bundle/bin" "$bundle/deploy" dist
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "$bundle/bin/lidza-agent" ./cmd/agent
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "$bundle/bin/lidza-control" ./cmd/web
printf '%s\n' "$arch" > "$bundle/ARCH"
install -m 0755 scripts/install-agent.sh "$bundle/install-agent.sh"
install -m 0644 deploy/lidza-agent.service deploy/lidza-control.service deploy/agent.example.json "$bundle/deploy/"
(cd "$bundle" && sha256sum ARCH bin/lidza-agent bin/lidza-control install-agent.sh deploy/lidza-control.service deploy/lidza-agent.service deploy/agent.example.json > SHA256SUMS)
tar -C "$bundle" -czf "dist/lidza-agent-linux-$arch.tar.gz" .
(cd dist && sha256sum "lidza-agent-linux-$arch.tar.gz" > "lidza-agent-linux-$arch.tar.gz.sha256")
printf 'Built dist/lidza-agent-linux-%s.tar.gz\n' "$arch"
