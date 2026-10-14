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

# --- run-one.sh: verdict + tally handoff (W2 LOW, M28 recurrence shape) ------
# run-one.sh duplicates the three-way verdict logic: it forks each case into a
# generated body script whose LAST line writes "<pass> <fail> <skip>" to
# .case-tally, reads that tally back, then applies the skip rules. Both halves
# are hand-maintained, so both are pinned here — the same reason fold_verdict
# above is re-declared rather than sourced.
#
# THE HOLE (M28 shape): if the generated body stops writing the tally (a
# rewrite drops the printf, or the case's own `exit` fires before it), the
# read-back falls into the missing-tally branch and counts the run as a
# failure — OR a hand-edit of the verdict rules turns "0 asserts" into a
# PASS again. Neither would break any test: nothing pinned this copy.

# 1. The verdict rules. Extracted from run-one.sh and executed with a stub
#    tally — same pin mechanics as the CASE_BLOCK extraction from check.yml
#    in test-ci-floor-guard.sh. Starts at the FAILURE branch (cf != 0), which
#    run-one.sh checks first, and runs through the PASS line.
VERDICT_BLOCK=$(awk '/^if \[ "\$cf" -ne 0 \]; then$/,0' scripts/e2e/run-one.sh \
	| awk "/^echo 'PASS'\$/{print; exit} {print}")
if [ -z "$VERDICT_BLOCK" ]; then
	echo '  FAIL could not extract run-one.sh verdict block'
	fails=$((fails + 1))
else
	verdict_harness=$(mktemp "${TMPDIR:-/tmp}/runone-pin.XXXXXX")
	verdict_work=$(mktemp -d "${TMPDIR:-/tmp}/runone-pin-work.XXXXXX")
	: > "$verdict_work/server.log"
	trap 'rm -rf "$verdict_harness" "$verdict_work"' EXIT
	{
		printf '#!/usr/bin/env bash\nset -u\n'
		printf 'WORK=%q\n' "$verdict_work"
		printf '%s\n' "$VERDICT_BLOCK"
	} > "$verdict_harness"
	# cp/cs/cf are the harness's inputs (run-one.sh's variable names); the
	# block's own `exit` codes are the verdict under test.
	v_out() { ( export cp=$1 cf=$2 cs=$3; bash "$verdict_harness" 2>/dev/null ); echo "rc=$?"; }
	check 'run-one: 0 asserts, 0 fails, 0 skip -> SKIP' \
		'SKIP (0 asserts — preconditions not met on this host)
rc=0' "$(v_out 0 0 0)"
	check 'run-one: 0 asserts + skip -> SKIP' \
		'SKIP (preconditions not met on this host)
rc=0' "$(v_out 0 0 1)"
	check 'run-one: 12 asserts, 0 fails -> PASS' \
		'PASS
rc=0' "$(v_out 12 0 0)"
	check 'run-one: any failure exits 1 (server-log path)' \
		'rc=1' "$(v_out 3 1 0 | tail -1)"
fi

# 2. The tally handoff: the generated body's last line must write the
#    three counters, in run-e2e.sh's field order (pass fail skip).
if grep -qE 'printf "%s %s %s.{0,4}n" "\$E2E_PASS" "\$E2E_FAIL" "\$\{E2E_SKIP:-0\}" > .*\.case-tally' scripts/e2e/run-one.sh; then
	echo '  ok   run-one.sh writes the pass/fail/skip tally from the case body'
else
	echo '  FAIL run-one.sh no longer writes the .case-tally line — the read-back below counts every run as a failure'
	fails=$((fails + 1))
fi

# 3. The read-back is honest: a missing tally is a FAILURE (cf=1), never a
#    pass — mirrored in the text and enforced by the else branch.
if grep -q 'case bailed before any assert' scripts/e2e/run-one.sh; then
	echo '  ok   run-one.sh treats a missing tally as a failure, not a pass'
else
	echo '  FAIL run-one.sh lost the missing-tally-is-failure branch'
	fails=$((fails + 1))
fi

printf '\n'
if [ "$fails" -eq 0 ]; then
	echo 'ALL SKIP-REPORTING PINS PASS'
else
	echo "$fails PIN(S) FAILED"
fi
exit "$fails"
