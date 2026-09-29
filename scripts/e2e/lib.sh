#!/usr/bin/env bash
# lib.sh — shared helpers for the mini-s3 e2e suite (sourced by run-e2e.sh).
#
# Contract (leaf 3.6): set -u (never set -e); assert helpers count failures
# with a REAL exit code at suite end; no `|| true` anywhere.
#
# Error-semantics assertions use s3req (curl --aws-sigv4) rather than the AWS
# CLI: aws-cli/2.33.0 fails to surface S3 error codes against this server
# ("An error occurred () ..." with empty code — see the report), so
# aws_fail-style greps on CLI stderr would test the CLI, not the server.
# s3req asserts HTTP status + <Code> straight from the wire.

E2E_PASS=0
E2E_FAIL=0
E2E_CASE=''

_e2e_record() {
	# $1 = 0/1 outcome, $2 = label
	if [ "$1" -eq 0 ]; then
		E2E_PASS=$((E2E_PASS + 1))
		printf '    ok   %s\n' "$2"
	else
		E2E_FAIL=$((E2E_FAIL + 1))
		printf '    FAIL %s\n' "$2"
	fi
}

# assert_eq <label> <expected> <actual>
assert_eq() {
	if [ "$2" = "$3" ]; then
		_e2e_record 0 "$1"
	else
		printf '         expected: %s\n         actual:   %s\n' "$2" "$3"
		_e2e_record 1 "$1"
	fi
}

# assert_contains <label> <haystack> <needle>
assert_contains() {
	case "$2" in
	*"$3"*) _e2e_record 0 "$1" ;;
	*)
		printf '         wanted substring: %s\n         in: %.200s\n' "$3" "$2"
		_e2e_record 1 "$1"
		;;
	esac
}

# --- SigV4-authenticated raw HTTP via curl ------------------------------------
# Requires BASE_URL (https://localhost:PORT) and the module credentials.
S3_STATUS=''
S3_BODY=''
s3req() {
	# s3req <METHOD> <path> [extra curl args...]
	# Sets S3_STATUS (HTTP code) and S3_BODY (response body).
	local method=$1 path=$2
	shift 2
	local body_file
	body_file=$(mktemp /tmp/e2e-s3req.XXXXXX)
	S3_STATUS=$(curl -sk -o "$body_file" -w '%{http_code}' \
		--aws-sigv4 "aws:amz:us-east-1:s3" \
		--user "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" \
		-X "$method" "$BASE_URL$path" "$@" 2>/dev/null)
	S3_BODY=$(cat "$body_file")
	rm -f "$body_file"
}

# s3_code — extract the <Code> element from S3_BODY (empty when none).
s3_code() {
	printf '%s' "$S3_BODY" | sed -n 's/.*<Code>\([^<]*\)<\/Code>.*/\1/p' | head -1
}

# assert_status <label> <expected_status> <METHOD> <path> [curl args...]
assert_status() {
	local label=$1 want=$2 method=$3 path=$4
	shift 4
	s3req "$method" "$path" "$@"
	assert_eq "$label" "$want" "$S3_STATUS"
}

# assert_s3code <label> <expected_code> — after assert_status/s3req.
assert_s3code() {
	assert_eq "$1" "$2" "$(s3_code)"
}

# aws_ok <label> <aws-args...> — assert an aws command exits 0 (success paths
# work fine through the CLI; only its error-code reporting is broken).
aws_ok() {
	local label=$1
	shift
	if aws "$@" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1; then
		_e2e_record 0 "$label"
	else
		_e2e_record 1 "$label"
	fi
}

# aws_fail_status <label> <expected_error_text> <aws-args...>
# Assert aws exits non-zero AND stderr contains expected_error_text (any
# distinctive fragment — CLI-independent, does not rely on the S3 <Code>).
aws_fail_status() {
	local label=$1 want=$2
	shift 2
	local err rc
	err=$(aws "$@" --endpoint-url "$ENDPOINT" --no-verify-ssl 2>&1 >/dev/null)
	rc=$?
	if [ "$rc" -ne 0 ] && printf '%s' "$err" | grep -q "$want"; then
		_e2e_record 0 "$label"
	else
		printf '         rc=%s want=%s err=%.200s\n' "$rc" "$want" "$err"
		_e2e_record 1 "$label"
	fi
}

# aws_cap <varname> <aws-args...> — capture stdout of an aws command.
aws_cap() {
	local __var=$1
	shift
	printf -v "$__var" '%s' "$(aws "$@" --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null)"
}

# launch_expect_fail <config.json> <logfile> <timeout_seconds>
# Fail-loud startup contract: start ./mini-s3-server with <config> and
# expect it to EXIT BY ITSELF within <timeout> seconds (log.Fatalf aborts
# startup on bad backend/frontend config — the server must never keep
# running with a half-built table). Mirrors run-e2e.sh's launch_server
# but for the failure path. Sets:
#   FAILSTART_EXIT = 0 when the process exited on its own in time, 1 when
#                    it had to be killed (still running at the timeout)
#   FAILSTART_RC   = its exit status (empty when it was killed)
# The caller asserts on FAILSTART_EXIT/FAILSTART_RC and greps <logfile>.
launch_expect_fail() {
	local cfg=$1 logf=$2 timeout=$3 waited=0
	FAILSTART_EXIT=1
	FAILSTART_RC=''
	MINIS3_CONFIG="$cfg" ./mini-s3-server >"$logf" 2>&1 &
	FAILSTART_PID=$!
	# 0.2s steps, 5 per second.
	while [ "$waited" -lt "$((timeout * 5))" ]; do
		if ! kill -0 "$FAILSTART_PID" 2>/dev/null; then
			FAILSTART_EXIT=0
			break
		fi
		sleep 0.2
		waited=$((waited + 1))
	done
	if [ "$FAILSTART_EXIT" -eq 0 ]; then
		wait "$FAILSTART_PID" 2>/dev/null
		FAILSTART_RC=$?
	else
		kill -9 "$FAILSTART_PID" 2>/dev/null
		wait "$FAILSTART_PID" 2>/dev/null
	fi
}

# wait_for_port <host> <port> <timeout_seconds>
wait_for_port() {
	local host=$1 port=$2 timeout=$3 waited=0
	while ! nc -z "$host" "$port" >/dev/null 2>&1; do
		if [ "$waited" -ge "$timeout" ]; then
			return 1
		fi
		sleep 0.2
		waited=$(awk "BEGIN{print $waited + 0.2}")
	done
	return 0
}

# e2e_finish — print totals; return non-zero on any failure (REAL exit code).
e2e_finish() {
	printf '\n=== e2e summary ===\n'
	printf '  passed: %d\n  failed: %d\n' "$E2E_PASS" "$E2E_FAIL"
	[ "$E2E_FAIL" -eq 0 ]
}
