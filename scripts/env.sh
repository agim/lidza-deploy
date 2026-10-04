#!/usr/bin/env bash
# Source this file in the prepared cloud workspace.
if [ -x /workspace/toolchains/go/bin/go ]; then
  export PATH="/workspace/toolchains/go/bin:$PATH"
  export GOMODCACHE=/workspace/.cache/gomod GOPATH=/workspace/.cache/gopath GOCACHE=/workspace/.cache/go-build
fi

if [ -d /workspace ]; then
  export DOCKER_CONFIG=/workspace/.cache/docker
  mkdir -p "$DOCKER_CONFIG"
fi
