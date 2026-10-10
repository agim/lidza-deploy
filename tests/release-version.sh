#!/usr/bin/env bash
# Exercise the actual packaged executables, not mocked version output.
set -euo pipefail
bundle=$(realpath "${1:?bundle directory required}")
version=${2:?expected version required}
scratch=$(mktemp -d /tmp/lidza-version-test.XXXXXX)
trap 'rm -rf "$scratch"' EXIT
for binary in lidza-agent lidza-control;do
 (
  cd "$scratch"
  # Version checks must bypass configuration, onboarding, and server startup.
  CONTROL_DATA_DIR="$scratch/must-not-exist" LIDZA_ADDR=invalid \
   timeout 5 "$bundle/bin/$binary" -version > output 2> errors
  printf '%s\n' "$version" > expected
  cmp expected output
  test ! -s errors
  test ! -e must-not-exist
  test ! -e .lidza-deploy
 )
done
printf 'PASS: packaged agent and control version checks exit without starting services or creating setup state\n'
