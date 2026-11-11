#!/usr/bin/env bash
# Validates the source before a release with the checks the release workflow
# (.github/workflows/release.yml) runs, so local and CI checks do not drift.
# Run it on the commit to be tagged:
#
#   scripts/release-check.sh [--browser] [VERSION]
#
# VERSION (or $VERSION) is the tag to be cut, for example v0.3.0 or, on a
# signed distribution channel, v0.3.0-private.1; when given it is checked
# against the release pattern. The working tree must
# be clean: the build must reproduce the committed console assets.
#
# The checks: npm run typecheck, npm run test:ui, npm run build with the
# committed console assets unchanged, go vet ./..., go test -race ./... and
# tests/upgrade-signed-bundle.sh. --browser adds npm run test:browser (the
# browser suites against the fresh build; CI runs them in their own job).
#
# Independent checks run at the same time. The only ordering is the build:
# it deletes and rewrites web/static/assets/console, which the Go package web
# embeds, so the Go checks and the browser suites start once the build is
# done. Each check writes its own log; the summary lists every check with its
# time and prints the end of the log of each one that failed.
#
# Environment:
#   RELEASE_CHECK_LOGS=dir   keep the logs there (default: a temporary directory)
#   RELEASE_CHECK_TAIL=n     log lines shown per failed check (default 80)
#   GOFLAGS                  passed to go as usual (for example -p=4)
set -euo pipefail
cd "$(dirname "$0")/.."

browser=false
version=${VERSION:-}
for arg in "$@"; do
  case "$arg" in
    --browser) browser=true ;;
    -h | --help) sed -n '2,27p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*) echo "unknown option $arg" >&2; exit 2 ;;
    *) version=$arg ;;
  esac
done
if [[ -n $version ]]; then
  [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-private\.[0-9]+)?$ ]] || { echo "$version is not a release version such as v0.3.0 or v0.3.0-private.1"; exit 1; }
fi

logs=${RELEASE_CHECK_LOGS:-$(mktemp -d "${TMPDIR:-/tmp}/release-check.XXXXXX")}
mkdir -p "$logs"
tail_lines=${RELEASE_CHECK_TAIL:-80}
started=$(date +%s)
steps=()
declare -A pids=()

# Stop every running check when the script is interrupted or a setup step fails.
cleanup() {
  local pid
  for pid in "${pids[@]}"; do kill -- "-$pid" 2>/dev/null || kill "$pid" 2>/dev/null || true; done
}
trap cleanup EXIT
trap 'exit 130' INT TERM

slug() { tr -c 'a-zA-Z0-9\n' '-' <<<"$1"; }

# start NAME COMMAND...: runs a check in the background, in its own process
# group, with its output in its own log and its exit code and seconds in
# $logs/<slug>.status.
start() {
  local name=$1 file
  shift
  file=$logs/$(slug "$name")
  steps+=("$name")
  printf '  started  %s\n' "$name"
  setsid bash -c '
    at=$(date +%s)
    status=0
    "${@:3}" >"$1.log" 2>&1 </dev/null || status=$?
    printf "%s %s\n" "$status" $(($(date +%s) - at)) >"$1.status"
    printf "  %-8s %s (%ss)\n" "$([[ $status == 0 ]] && echo passed || echo FAILED)" "$2" $(($(date +%s) - at))
  ' check "$file" "$name" "$@" &
  pids[$name]=$!
}

# finish NAME: waits for a check and returns its exit code.
finish() {
  local name=$1 file
  file=$logs/$(slug "$name")
  wait "${pids[$name]}" 2>/dev/null || true
  unset "pids[$name]"
  [[ -f $file.status ]] || { printf '1 0\n' >"$file.status"; }
  return "$(cut -d' ' -f1 "$file.status")"
}

echo "Release check; logs in $logs"
if [[ -L node_modules ]]; then
  # npm ci deletes node_modules first, which would empty a shared directory
  # this checkout links to. Everything else runs as in CI.
  echo "node_modules is a symbolic link; skipping npm ci and using the linked dependencies"
  case $(readlink -f node_modules) in
    /mnt/*) echo "  (they are on the Windows drive, which makes npm run test:ui take minutes; see docs/updates.md for a local copy)" ;;
  esac
else
  at=$(date +%s)
  npm ci --no-audit --no-fund >"$logs/npm-ci.log" 2>&1 || { tail -n "$tail_lines" "$logs/npm-ci.log"; echo 'npm ci failed'; exit 1; }
  printf '  passed   npm ci (%ss)\n' $(($(date +%s) - at))
fi
if $browser && [[ ! -d tests/browser/node_modules ]]; then
  echo 'tests/browser has no dependencies: npm ci --prefix tests/browser, then npx --prefix tests/browser playwright install chromium' >&2
  exit 1
fi

start 'npm run typecheck' npm run typecheck
start 'npm run test:ui' npm run test:ui
start 'upgrade helper' bash tests/upgrade-signed-bundle.sh
start 'npm run build' bash -c "npm run build && { git diff --quiet || { echo 'Console build output differs from the committed assets'; git status --short; exit 1; }; }"
# The Go checks wait for the build: it rewrites the assets package web embeds.
build_ok=true
finish 'npm run build' || build_ok=false
start 'go vet' go vet ./...
start 'go test -race' go test -race ./...
if $browser; then
  if $build_ok; then
    start 'npm run test:browser' npm run test:browser
  else
    steps+=('npm run test:browser')
    printf '1 0\n' >"$logs/$(slug 'npm run test:browser').status"
    echo 'Not run: the console build failed.' >"$logs/$(slug 'npm run test:browser').log"
  fi
fi

failed=()
for name in "${steps[@]}"; do
  [[ -n ${pids[$name]:-} ]] || continue
  finish "$name" || true
done
printf '\n%-24s %-7s %s\n' check result seconds
for name in "${steps[@]}"; do
  read -r status seconds <"$logs/$(slug "$name").status"
  result=passed
  [[ $status == 0 ]] || { result=FAILED; failed+=("$name"); }
  printf '%-24s %-7s %s\n' "$name" "$result" "$seconds"
done
for name in "${failed[@]}"; do
  log=$logs/$(slug "$name").log
  # go test prints a failing package's tests where they fail, which can be
  # far from the end of the log: show those first.
  if [[ $name == go* ]] && grep -qE '^(--- FAIL|panic:|WARNING: DATA RACE)' "$log"; then
    printf '\n---- %s: failures in %s ----\n' "$name" "$log"
    grep -E -A20 '^(--- FAIL|panic:|WARNING: DATA RACE)' "$log" | head -n $((tail_lines * 2))
  fi
  printf '\n---- %s: last %s lines of %s ----\n' "$name" "$tail_lines" "$log"
  tail -n "$tail_lines" "$log"
done
if ((${#failed[@]})); then
  printf '\nRelease check FAILED in %ss: %s. Logs: %s\n' $(($(date +%s) - started)) "$(IFS=,; echo "${failed[*]}" | sed 's/,/, /g')" "$logs"
  exit 1
fi
printf '\nRelease check passed in %ss.\n' $(($(date +%s) - started))
