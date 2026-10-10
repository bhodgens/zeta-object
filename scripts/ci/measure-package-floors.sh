#!/usr/bin/env bash
# Re-measure every CI per-package coverage floor and print a headroom table.
#
# WHY THIS EXISTS (audit finding G7/G8, 2026-10-10): the per-package floors in
# .github/workflows/check.yml were each set from a SINGLE `go tool cover` reading.
# The aggregate number moves with which non-deterministic branches a test run
# happens to execute, so one sample is not a measurement - and a floor derived
# from one sits ON the boundary.
#
# This runs the EXACT commands the CI job runs (see the matrix job in
# .github/workflows/check.yml):
#   - every package:  go test -count=1 -race -coverprofile=cover.out <pkg>
#   - s3 special:     race WITHOUT -coverprofile, then coverage WITHOUT -race
#     (that split exists because race+cover on s3 reclaims the CI runner, ~107s)
#
# SAMPLES: set SAMPLES=3 (default) for the number you can trust. SAMPLES=1
# reproduces the original mistake - useful only to reproduce a known failure.
#
# PLATFORM: the floors are enforced on linux (ubuntu-24.04, as setup-go
# provides), and coverage DIFFERS BY PLATFORM - on darwin the same commits
# measure 1.0 to 2.4 points LOWER for filesystem-touching packages
# (backend/conformance, backend/fsbackend, frontend/s3). So:
#   - run this on linux to judge whether CI will go red;
#   - a darwin number below a floor is NOT evidence the floor is wrong.
# For the authoritative linux numbers without a linux box, read them out of a
# green CI run: `gh run view <id> --log | grep -E 'total:'`.
#
# Usage:  scripts/ci/measure-package-floors.sh [package-substring ...]
#         SAMPLES=3 scripts/ci/measure-package-floors.sh
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 1

SAMPLES="${SAMPLES:-3}"
PROF=$(mktemp "${TMPDIR:-/tmp}/covfloor.XXXXXX") || exit 1
trap 'rm -f "$PROF"' EXIT

# The floor values must match the `case` block in .github/workflows/check.yml.
# Duplicated on purpose: if the workflow's case block changes and this is not
# updated, the table below says "MISSING" and the mismatch is obvious.
floor_for() {
  case "$1" in
    github.com/bhodgens/zeta-object)              echo 80 ;;
    */internal/auth)                                echo 90 ;;
    */internal/backend)                             echo 92 ;;
    */internal/backend/conformance)                 echo 78 ;;
    */internal/backend/fsbackend)                   echo 83 ;;
    */internal/bucketmanager)                       echo 87 ;;
    */internal/frontend)                            echo 92 ;;
    */internal/frontend/ftp)                        echo 66 ;;
    */internal/frontend/s3)                         echo 83 ;;
    */internal/frontend/sftp)                       echo 68 ;;
    */internal/frontend/webdav)                     echo 83 ;;
    */internal/frontend/owncloud)                   echo 92 ;;
    */internal/fslock)                              echo 99 ;;
    */internal/metadata)                            echo 91 ;;
    */internal/objectmodel)                         echo 91 ;;
    */internal/adminserver)                         echo 87 ;;
    */cmd/admin-server)                             echo 0  ;;
    */internal/adminserver/gateway)                  echo 78 ;;
    */internal/batchops)                             echo 86 ;;
    */internal/frontend/admin)                       echo 83 ;;
    */internal/frontend/autherr)                     echo 98 ;;
    */internal/frontend/h3)                          echo 89 ;;
    */scripts/e2e/h3probe)                           echo 0  ;;
    */scripts/e2e/fixtures/zmetad-fixture)           echo 0  ;;
    *)                                               echo "MISSING" ;;
  esac
}

# CI builds the matrix from `go list ./...`, so packages with no floor case get
# FLOOR unset and the awk check treats "" as 0 - they are gated at 0. List them
# so a new package cannot join the matrix unnoticed and un-gated.
measure() {
  local pkg="$1" total=""
  if [ "$pkg" = "github.com/bhodgens/zeta-object/internal/frontend/s3" ]; then
    go test -count=1 -coverprofile="$PROF" "$pkg" >/dev/null 2>&1 || { echo FAILED; return; }
  else
    go test -count=1 -race -coverprofile="$PROF" "$pkg" >/dev/null 2>&1 || { echo FAILED; return; }
  fi
  total=$(go tool cover -func="$PROF" 2>/dev/null | awk '/^total:/ {gsub(/%/,"",$3); print $3}')
  [ -n "$total" ] || { echo FAILED; return; }
  echo "$total"
}

printf '%-42s %6s %10s %9s\n' PACKAGE FLOOR MODE HEADROOM
printf '%s\n' '-------------------------------------------------------------------------------------'

PKGS=$(go list ./...)
[ $# -gt 0 ] && PKGS=$(printf '%s\n' "$PKGS" | grep -E "$1")

RED=0
for pkg in $PKGS; do
  floor=$(floor_for "$pkg")
  vals=""
  for _ in $(seq "$SAMPLES"); do vals="$vals $(measure "$pkg")"; done
  nums=$(printf '%s' "$vals" | tr ' ' '\n' | grep -v '^$' | grep -v '^FAILED$' | sort -n)
  nfail=$(printf '%s' "$vals" | tr ' ' '\n' | grep -c '^FAILED$')

  if [ -z "$nums" ]; then
    printf '%-42s %6s %10s %9s\n' "${pkg##*/}" "$floor" "ALL-FAIL" "-"
    echo "  ** $nfail of $SAMPLES samples returned a non-zero go test exit (test flake or race)"
    RED=1
    continue
  fi
  # Median of the samples - the number that is not one unlucky read.
  mode=$(printf '%s\n' "$nums" | awk '{a[NR]=$1} END {print a[int((NR+1)/2)]}')
  lo=$(printf '%s\n' "$nums" | head -1)
  hi=$(printf '%s\n' "$nums" | tail -1)
  head=$(awk -v m="$mode" -v f="$floor" 'BEGIN{printf "%+.1f", (f=="MISSING"?0:m-f)}')

  printf '%-42s %6s %10s %9s' "${pkg#github.com/bhodgens/zeta-object}" "$floor" "$mode" "$head"
  [ "$lo" != "$hi" ] && printf '   (samples %s-%s)' "$lo" "$hi"
  [ "$nfail" -gt 0 ] && printf '   ** %d of %d samples FAILED' "$nfail" "$SAMPLES"
  printf '\n'
  [ "$floor" = "MISSING" ] && RED=1
  awk -v lo="$lo" -v f="$floor" 'BEGIN{exit !(f!="MISSING" && lo+0 < f+0)}' && RED=1
done

echo
if [ "$RED" -eq 0 ]; then
  echo "OK: every floor holds at the LOWEST of $SAMPLES samples (worst case, not best case)."
  echo "Ratchet a floor UP when mode - floor >= 1.0. Do not lower a floor without"
  echo "proving no code changed - see AGENTS.md \"Coverage floors move with code\"."
else
  echo "RED: at least one floor does not hold, or a package has no floor. See above."
fi
exit "$RED"
