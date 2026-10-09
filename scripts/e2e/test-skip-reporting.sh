#!/usr/bin/env bash
# test-skip-reporting.sh — pins for the skip verdict (bughunt 2026-10-08).
#
# THE HOLE: a case whose preconditions are not met on this host (no ZFS
# dataset, no admin console binary, a fixture builder that will not compile)
# used to exit with 0 asserts and 0 failures, and run-e2e.sh printed it as
# PASS. That is the same reporting shape as the ZFS validation harness's
# unfalsifiable "TOTAL: 0/0", which survived a whole wave because nothing in
# the output distinguished "verified nothing" from "nothing to verify here".
# Case 18 went further and incremented E2E_PASS to look deliberate.
#
# THE CONTRACT: 0 asserts and 0 failures is a SKIP, never a PASS. A skip is not
# a failure - the host genuinely cannot run the case - but it is counted,
# named, and listed in the summary, so a green suite is never mistaken for full
# coverage.
#
# Run: bash scripts/e2e/test-skip-reporting.sh
set -u
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
fails=0

# fold_verdict mirrors run-e2e.sh's three-way verdict exactly. It is duplicated
# here on purpose: a pin that sourced the harness would pass even if the
# harness changed shape.
fold_verdict() {
	local cp_="$1" cf_="$2" cs_="$3" crashed="${4:-0}" v
	if [ "$cf_" -ne 0 ]; then v=FATAL
	elif [ "$cs_" -ne 0 ] || { [ "$cp_" -eq 0 ] && [ "$crashed" -ne 1 ]; }; then v=SKIP
	else v=PASS
	fi
	printf '%s' "$v"
}

check() {
	local label="$1" want="$2" got="$3"
	if [ "$got" = "$want" ]; then
		printf '  ok   %s -> %s\n' "$label" "$got"
	else
		printf '  FAIL %s -> %s, want %s\n' "$label" "$got" "$want"
		fails=$((fails + 1))
	fi
}

check '0 asserts, 0 fails, no e2e_skip' SKIP "$(fold_verdict 0 0 0)"
check '22 asserts, 0 fails' PASS "$(fold_verdict 22 0 0)"
check '12 asserts plus e2e_skip' SKIP "$(fold_verdict 12 0 1)"
check '3 asserts, 1 fail, e2e_skip' FATAL "$(fold_verdict 3 1 1)"
check 'crashed, no usable tally' FATAL "$(fold_verdict 0 1 0 1)"

# The lib.sh helpers: a skip is counted as a skip and never as a pass.
E2E_PASS=0
E2E_FAIL=0
E2E_SKIP=0
# shellcheck source=lib.sh
. scripts/e2e/lib.sh
e2e_skip 'no ZFS dataset on this host'
e2e_finish >/dev/null
check 'e2e_skip counts one skip' 1 "$E2E_SKIP"
check 'e2e_skip counts no pass' 0 "$E2E_PASS"
check 'e2e_skip counts no failure' 0 "$E2E_FAIL"

# No case may still fake a pass on a skip path.
if grep -rn 'E2E_PASS=\$((E2E_PASS + 1))' scripts/e2e/cases/ >/dev/null 2>&1; then
	echo "  FAIL a case still credits itself a pass on a skip path"
	grep -rn 'E2E_PASS=\$((E2E_PASS + 1))' scripts/e2e/cases/
	fails=$((fails + 1))
else
	echo '  ok   no case fakes a pass on a skip path'
fi

printf '\n'
if [ "$fails" -eq 0 ]; then
	echo 'ALL SKIP-REPORTING PINS PASS'
else
	echo "$fails PIN(S) FAILED"
fi
exit "$fails"
