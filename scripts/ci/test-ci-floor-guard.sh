#!/usr/bin/env bash
# test-ci-floor-guard.sh — pins for the CI per-package coverage-floor guard.
#
# THE HOLE (bughunt 2026-10-10, audit G9): the coverage matrix in
# .github/workflows/check.yml is generated from `go list ./...`, so it grows by
# itself. The `case` block that assigns each package's FLOOR is hand-maintained.
# Seven packages had no case: internal/adminserver/gateway (80.2%),
# internal/batchops (88.4%), internal/frontend/admin (85.2%),
# internal/frontend/autherr (100%), internal/frontend/h3 (91.0%), and the two
# main-only scripts/e2e binaries (0%).
#
# An unset FLOOR reaches
#     awk -v floor="" ... if ($3+0 < floor) { ... exit 1 }
# as the empty string, which compares as 0. So a package with 100% coverage and
# a package with none were gated identically: those jobs ran and enforced
# nothing. The floors being correct is beside the point — the ENFORCEMENT was
# the bug, and that is what this file pins.
#
# WHAT IS PINNED:
#   1. every package in `go list ./...` has a floor case (no MISSING/empty);
#   2. the guard after the `esac` exists and exits non-zero when FLOOR is unset;
#   3. the guard does NOT fire for packages that legitimately have floors,
#      including the two main-only binaries at FLOOR=0 (0 is a value, "" is not);
#   4. every floor is at or below the number CI actually measured for it, so a
#      ratchet cannot set a floor the platform cannot reach;
#   5. the awk comparison itself is not defeated by an empty floor.
#
# This test reads the workflow. It never edits it.
#
# Run: bash scripts/ci/test-ci-floor-guard.sh
set -u
cd "$(git rev-parse --show-toplevel)" || exit 1
WF=.github/workflows/check.yml
pass=0
fails=0

ok()   { pass=$((pass + 1)); printf '  ok   %s\n' "$1"; }
bad()  { fails=$((fails + 1)); printf '  FAIL %s\n' "$1"; }

[ -f "$WF" ] || { echo "FAIL: $WF not found"; exit 1; }

# --- 1. every package has a floor -------------------------------------------
# Extract the case block and run it per package, reading FLOOR back out. The
# block is executed, not pattern-matched, so this tests the same code the runner
# executes.
CASE_BLOCK=$(awk '/^ *case "\$\{\{ matrix\.package \}\}" in$/,/^ *esac$/' "$WF")
[ -n "$CASE_BLOCK" ] || { echo "FAIL: could not extract the FLOOR case block from $WF"; exit 1; }

probe=$(mktemp "${TMPDIR:-/tmp}/floorguard.XXXXXX") || exit 1
harness=$(mktemp "${TMPDIR:-/tmp}/floorguard.XXXXXX") || exit 1
trap 'rm -f "$probe" "$harness"' EXIT
{
	printf '#!/usr/bin/env bash\nset -u\n'
	printf '%s\n' "$CASE_BLOCK" \
		| sed 's/"\${{ matrix\.package }}"/"$PKG"/g; s/\${{ matrix\.package }}/$PKG/g'
	printf 'printf "FLOOR=%%s\\n" "${FLOOR-unset}"\n'
} > "$harness"

missing=""
for pkg in $(go list ./...); do
	out=$(PKG="$pkg" bash "$harness" 2>/dev/null | head -1)
	case "$out" in
		FLOOR=) missing="$missing $pkg" ;;
		FLOOR=unset) missing="$missing $pkg" ;;
	esac
done
if [ -z "$missing" ]; then
	ok "every package in go list ./... has a FLOOR case"
else
	bad "packages with NO floor case (gated at 0):$missing"
fi

# --- 2. the guard exists -----------------------------------------------------
if grep -q 'no coverage floor for' "$WF"; then
	ok "the missing-floor guard exists"
else
	bad "no guard: a package can join the matrix and be gated at 0 silently"
fi

# The guard must be an `exit 1`, not an `echo` that lets the job continue.
if grep -A3 'no coverage floor for' "$WF" | grep -qE '^ *exit 1'; then
	ok "the guard exits non-zero (not just prints)"
else
	bad "the guard prints an error but does not fail the job"
fi

# --- 3. the guard fires on an unknown package, and not on a real one ---------
GUARD_BLOCK=$(awk '/no coverage floor for/{f=1} f{print} f&&/^ *fi$/{exit}' "$WF")
[ -n "$GUARD_BLOCK" ] || { echo "FAIL: could not extract the guard"; exit 1; }
{
	printf '#!/usr/bin/env bash\nset -u\nFLOOR=""\n'
	printf '%s\n' "$GUARD_BLOCK" \
		| sed 's/\${{ matrix\.package }}/$PKG/g'
	printf 'echo "REACHED-THE-TEST-LINES"\n'
} > "$probe"

if PKG=github.com/bhodgens/zeta-object/internal/brandnewpkg bash "$probe" >/dev/null 2>&1; then
	bad "guard did NOT fire for a package with no floor case"
else
	ok "guard fires (exit non-zero) for a package with no floor case"
fi

if PKG=github.com/bhodgens/zeta-object/internal/brandnewpkg bash "$probe" 2>&1 | grep -q 'REACHED-THE-TEST-LINES'; then
	bad "guard fired but execution continued to the test lines"
else
	ok "guard stops execution before the test lines"
fi

# FLOOR=0 is a real value: the two main-only e2e binaries must NOT trip the guard.
for pkg in github.com/bhodgens/zeta-object/scripts/e2e/h3probe \
           github.com/bhodgens/zeta-object/cmd/admin-server; do
	f=$(PKG="$pkg" bash "$harness" 2>/dev/null | head -1)
	if [ "$f" = "FLOOR=0" ]; then
		ok "$pkg has an explicit FLOOR=0 (a value, not an absence)"
	else
		bad "$pkg floor is '$f', expected an explicit FLOOR=0"
	fi
done

# --- 4. no floor exceeds what CI measured ------------------------------------
# Numbers from CI run 38114160393 (ubuntu-24.04, go1.27.2) - the FIRST run on the
# go1.27.2 toolchain. Every number moved when the compiler moved (frontend/s3
# 83.2 -> 81.4), which is why this table is versioned WITH the toolchain rather
# than treated as a constant. This is
# the ratchet direction check: a floor ABOVE the measured value is what turned
# a green run red once already (audit G7).
measured() {
	case "$1" in
		github.com/bhodgens/zeta-object)            echo 82.7 ;;
		*/internal/adminserver)                     echo 87.1 ;;
		*/internal/adminserver/gateway)             echo 77.6 ;;
		*/internal/auth)                            echo 91.1 ;;
		*/internal/backend)                         echo 93.9 ;;
		*/internal/backend/conformance)             echo 76.6 ;;
		*/internal/backend/fsbackend)               echo 81.6 ;;
		*/internal/batchops)                        echo 88.4 ;;
		*/internal/bucketmanager)                   echo 87.8 ;;
		*/internal/frontend)                        echo 92.2 ;;
		*/internal/frontend/admin)                  echo 84.9 ;;
		*/internal/frontend/autherr)                echo 100.0 ;;
		*/internal/frontend/ftp)                    echo 67.1 ;;
		*/internal/frontend/h3)                     echo 90.7 ;;
		*/internal/frontend/owncloud)               echo 93.6 ;;
		*/internal/frontend/s3)                     echo 81.4 ;;
		*/internal/frontend/sftp)                   echo 68.7 ;;
		*/internal/frontend/webdav)                 echo 84.0 ;;
		*/internal/fslock)                          echo 100.0 ;;
		*/internal/metadata)                        echo 92.1 ;;
		*/internal/objectmodel)                     echo 92.6 ;;
	esac
}
over=""
for pkg in $(go list ./...); do
	f=$(PKG="$pkg" bash "$harness" 2>/dev/null | head -1)
	f=${f#FLOOR=}
	[ -z "$f" ] && continue
	m=$(measured "$pkg")
	[ -z "$m" ] && continue          # main-only packages: nothing to compare
	awk -v f="$f" -v m="$m" 'BEGIN{exit !(f+0 > m+0)}' && over="$over ${pkg##*/}(floor $f > measured $m)"
done
if [ -z "$over" ]; then
	ok "no floor exceeds the coverage CI measured for it"
else
	bad "floor above measured - will red-line a green run:$over"
fi

# --- 5. the empty-floor arithmetic, run through the CI line itself ----------
# THE ROOT CAUSE, pinned with the CI job's own expression rather than a
# paraphrase. `$3+0 < floor` with floor empty is false for EVERY measurement,
# including 0.0% - so the job passes and prints nothing. That is why a package
# with 100% coverage and a package with no coverage were gated identically, and
# why the guard (not just the floors) was the fix.
#
# A first attempt at this pin asserted `50+0 < ("" + 0)`, which is ALSO false -
# an empty string coerces to 0, so 50 < 0 is false too. That pin passed for the
# wrong reason and would have gone red if awk ever made "" coerce to a large
# number, i.e. it did not test what it claimed. Use the real expression.
_ci_floor_line() {
	awk '/^total:/ {gsub(/%/,"",$3); if ($3+0 < floor) {print "FAIL_ENFORCED"; exit 1} else {print "PASSED_UNDER_GATED"}}'
}
sample='total:					(statements)	88.4%'

if printf '%s\n' "$sample" | _ci_floor_line | grep -q PASSED_UNDER_GATED; then
	ok "confirmed: 88.4% with no floor assigned PASSES (the under-gating)"
else
	bad "an unassigned floor no longer passes 88.4% - re-check the guard's premise"
fi

if printf '%s\n' "$sample" | awk -v floor=86 '/^total:/ {gsub(/%/,"",$3); if ($3+0 < floor) {print "FAIL_ENFORCED"; exit 1} else {print "PASSED"}}' | grep -q PASSED; then
	ok "the same line DOES enforce a real floor (86): 88.4% passes on merit"
else
	bad "the CI awk line does not enforce a real floor - the comparison is broken"
fi

if printf '%s\n' "$sample" | awk -v floor=90 '/^total:/ {gsub(/%/,"",$3); if ($3+0 < floor) {print "FAIL_ENFORCED"; exit 1} else {print "PASSED"}}' | grep -q FAIL_ENFORCED; then
	ok "the same line FAILS 88.4% against floor 90 (a real floor can go red)"
else
	bad "a floor above the measurement did not fail - the gate cannot fail"
fi

if printf 'total:	(statements)	0.0%%\n' | _ci_floor_line | grep -q PASSED_UNDER_GATED; then
	ok "confirmed: even 0.0% passes with no floor assigned (the worst case)"
else
	bad "0.0% no longer passes unassigned - re-check the guard's premise"
fi

# --- 6. the measurement script's mirrored floor table matches the workflow ---
# measure-package-floors.sh keeps its own `floor_for` copy so it can report
# headroom without parsing YAML. A stale copy produces a table that says every
# floor holds while CI is red, or the reverse - the dishonesty this file exists
# to prevent.
#
# Two earlier attempts at this pin were wrong and both reported "ok" on a
# deliberately stale table: the first built the wrong package name, the second
# read a temp harness whose EXIT trap had already fired. The third compares both
# sides through ONE function, and asserts it compared a real number of floors -
# a check that compared nothing must never read as a pass. It writes the case
# block to a file and SOURCES it: piping it into `eval "$(cat)"` fails here,
# because the extracted block is not a complete script on stdin.
mir_file="${TMPDIR:-/tmp}/zeta-ci-mirrorchk.$$"
rm -f "$mir_file"

wf_floor_for() {
	printf '%s\n' "$CASE_BLOCK" \
		| sed 's/"\${{ matrix\.package }}"/"$1"/g; s/\${{ matrix\.package }}/$1/g' > "$mir_file"
	FLOOR=unset
	# shellcheck disable=SC1090 # generated from the workflow on purpose
	. "$mir_file"
	printf '%s\n' "$FLOOR"
}

mir_mismatch=""
mir_checked=0
TAB=$(printf '\t')
while IFS="$TAB" read -r pat floor; do
	[ -z "$pat" ] && continue
	case "$pat" in
		github.com/*) wf_pkg="$pat" ;;
		# "*/internal/fslock" -> "github.com/bhodgens/zeta-object/internal/fslock".
		# ${pat#\*/} removes "*" AND "/", so the slash must be re-added.
		*)            wf_pkg="github.com/bhodgens/zeta-object/${pat#\*/}" ;;
	esac
	got=$(wf_floor_for "$wf_pkg")
	if [ "$got" = "unset" ] || [ -z "$got" ]; then
		mir_mismatch="$mir_mismatch $pat(workflow has NO floor)"
		continue
	fi
	mir_checked=$((mir_checked + 1))
	[ "$got" != "$floor" ] && mir_mismatch="$mir_mismatch $pat(wf=$got mirror=$floor)"
done < <(sed -n '/^floor_for() {/,/^}/p' scripts/ci/measure-package-floors.sh \
	| grep -oE '^ +[^ ]+\) +echo [0-9]+' \
	| sed -E 's/^ +//; s/\) +echo /\t/')

# Count the floors on BOTH sides. A mirror that dropped a case would otherwise
# compare only what it still has and report agreement: proven by deleting the
# fslock case from the mirror, which this check passed until the count was added.
mir_total=$(sed -n '/^floor_for() {/,/^}/p' scripts/ci/measure-package-floors.sh \
	| grep -cE '^ +[^ ]+\) +echo [0-9]+')
wf_total=$(printf '%s\n' "$CASE_BLOCK" | grep -cE '\) +FLOOR=[0-9]+')

if [ "$mir_checked" -lt 20 ]; then
	bad "the mirror check only compared $mir_checked floors - it is not reading both sides"
elif [ "$mir_total" -ne "$wf_total" ]; then
	bad "measure-package-floors.sh has $mir_total floors, the workflow has $wf_total - a case was added or dropped on one side only"
elif [ -z "$mir_mismatch" ]; then
	ok "measure-package-floors.sh mirrors all $mir_checked workflow floors"
else
	bad "measure-package-floors.sh floor table is stale:$mir_mismatch"
fi

echo
echo "$pass passed, $fails failed."
if [ "$fails" -ne 0 ]; then
	echo "CI FLOOR GUARD: FAIL"
	exit 1
fi
echo "CI FLOOR GUARD: PASS"
