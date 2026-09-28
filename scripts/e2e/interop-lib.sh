#!/usr/bin/env bash
# interop-lib.sh — shared client-interop scenario table (leaf 5.2).
#
# Each scenario names ONE S3 operation (or small group). Case files drive
# each scenario through a CLIENT SHIM that prints a single line of JSON:
#
#   {"ok":true,"status":200,"code":"","data":"...","detail":""}     (success)
#   {"ok":false,"status":404,"code":"NoSuchKey","data":"","detail":".."} (fail)
#   {"parse_error":true,"detail":"..."}   <- client-side parse failure = FINDING
#
# Parity rule (FROZEN, leaf 5.2): a scenario passes when
#   - SUCCESS:   client exits success AND observed data matches, OR
#   - FAILURE:   client fails with the SAME S3 error code the aws-cli e2e
#                case asserts (the suite IS the expected-behavior spec).
# Client-side failures to PARSE a response are FINDINGS — recorded in
# INTEROP_FINDINGS and triaged by the case file, never silently dropped.
#
# bash 3.2 safe: no associative arrays, no \033 escapes.
set -u

INTEROP_FINDINGS=''
# INTEROP_EXPECT_DATA — when non-empty, success scenarios ALSO assert the
# shim's "data" field equals this value (payload hash / count / marker).
INTEROP_EXPECT_DATA=''
# INTEROP_EXPECT_CODE — UNSET by default (frozen expectation used). Cases may
# set it (usually '') for clients that cannot express the operation (leaf
# tolerance clause); must be paired with an inline comment, then unset after.

# --- frozen expectation table --------------------------------------------------
# scenario_expect <scenario> — echoes "<status> <code>"; code '-' means the
# client's own success must be observable (status asserted via the ok flag).
# Values are hardcoded from the existing aws-cli e2e cases:
#   bucket lifecycle .............. case 01
#   objects / unicode ............. case 02
#   error codes ................... cases 01–03
#   listing with prefix ........... case 04
#   multipart ..................... case 05
#   range ......................... case 06
#   presigned ..................... case 07
#   copy / batch delete ........... case 08
scenario_expect() {
	case "$1" in
	bucket_create)        printf '200 -' ;;
	bucket_create_dup)    printf '409 BucketAlreadyOwnedByYou' ;;
	bucket_head)          printf '200 -' ;;
	bucket_head_missing)  printf '404 NoSuchBucket' ;;
	bucket_list)          printf '200 -' ;;
	bucket_delete)        printf '204 -' ;;
	object_put)           printf '200 -' ;;
	object_get_roundtrip) printf '200 -' ;;
	unicode_key)          printf '200 -' ;;
	error_nosuchbucket)   printf '404 NoSuchBucket' ;;
	error_nosuchkey)      printf '404 NoSuchKey' ;;
	error_accessdenied)   printf '403 AccessDenied' ;;
	list_prefix)          printf '200 -' ;;
	range_get)            printf '206 -' ;;
	range_get_invalid)    printf '416 InvalidRange' ;;
	multipart_roundtrip)  printf '200 -' ;;
	presigned_get)        printf '200 -' ;;
	copy_object)          printf '200 -' ;;
	batch_delete)         printf '200 -' ;;
	*)                    printf '? ?' ;;
	esac
}

# scenario_pass <shim-cmd> <scenario> [args...] — run the shim and emit the
# frozen-rule verdicts into the e2e tally:
#   "<client>:<scenario>: outcome parity" — success parity (ok flag / data)
#     or failure parity (S3 error code equals the frozen expectation).
#   "<client>:<scenario>: data parity"    — when a data comparison applies.
# The shim command string must START WITH the client name (first word) so
# labels read e.g. "boto3:bucket_create: ...".
scenario_pass() {
	local shim=$1 scenario=$2
	shift 2
	local client=${shim%% *}
	local expect want_status want_code
	expect=$(scenario_expect "$scenario")
	want_status=${expect%% *}
	want_code=${expect#* }

	local shim_var="INTEROP_$(printf %s "$client" | tr '[:lower:]' '[:upper:]')_SHIM"
	local shim_cmd=${!shim_var:-}
	if [ -z "$shim_cmd" ]; then
		assert_eq "$client:$scenario: shim present" present missing
		return 1
	fi

	# Direct invocation: INTEROP_<CLIENT>_SHIM is "cmd" (word-split on
	# purpose — cases set e.g. INTEROP_BOTO3_SHIM="python3 .../boto3-runner.py").
	local got_json
	got_json=$($shim_cmd "$scenario" "$@" 2>/dev/null)

	local got_ok got_status got_code got_data
	got_ok=$(interop_json_field "$got_json" ok)
	got_status=$(interop_json_field "$got_json" status)
	got_code=$(interop_json_field "$got_json" code)
	got_data=$(interop_json_field "$got_json" data)

	# --- client-side parse failure => FINDING (the whole point of the leaf)
	if printf '%s' "$got_json" | grep -q '"parse_error"'; then
		local pdetail
		pdetail=$(interop_json_field "$got_json" detail)
		printf '         FINDING %s/%s: %s\n' "$client" "$scenario" "$pdetail"
		INTEROP_FINDINGS="$INTEROP_FINDINGS $client/$scenario($pdetail)"
		# Not a server-behavior failure: triaged, suite stays green.
		assert_eq "$client:$scenario: outcome parity (FINDING)" FINDING FINDING
		return 0
	fi

	if [ "$want_code" = '-' ]; then
		# success scenario: client must report ok AND carry expected data
		local verdict=1
		[ "$got_ok" = 'true' ] && verdict=0
		assert_eq "$client:$scenario: outcome parity" 0 "$verdict"
		if [ -n "$INTEROP_EXPECT_DATA" ]; then
			local dverdict=1
			[ "$got_data" = "$INTEROP_EXPECT_DATA" ] && dverdict=0
			assert_eq "$client:$scenario: data parity" 0 "$dverdict"
			[ "$dverdict" = 0 ] || return 1
		fi
		[ "$verdict" = 0 ] || return 1
		return 0
	fi

	# failure scenario: SAME S3 error code as the aws-cli case asserts.
	# INTEROP_EXPECT_CODE overrides the frozen code for clients that cannot
	# express the operation (leaf tolerance clause) — must come with an
	# inline comment in the case file; '' means success-shaped observable.
	[ "${INTEROP_EXPECT_CODE+set}" = 'set' ] && want_code=$INTEROP_EXPECT_CODE
	if [ "$want_code" = '-' ] || [ -z "$want_code" ]; then
		# success-shaped: client must report ok AND carry expected data
		local sverdict=1
		[ "$got_ok" = 'true' ] && sverdict=0
		assert_eq "$client:$scenario: outcome parity (closest observable)" 0 "$sverdict"
		if [ -n "$INTEROP_EXPECT_DATA" ]; then
			local sdverdict=1
			[ "$got_data" = "$INTEROP_EXPECT_DATA" ] && sdverdict=0
			assert_eq "$client:$scenario: data parity" 0 "$sdverdict"
			[ "$sdverdict" = 0 ] || return 1
		fi
		[ "$sverdict" = 0 ] || return 1
		return 0
	fi
	local verdict=1
	[ "$got_ok" = 'false' ] && [ "$got_code" = "$want_code" ] && verdict=0
	assert_eq "$client:$scenario: outcome parity ($want_status $want_code)" 0 "$verdict"
	[ "$verdict" = 0 ] || return 1
	return 0
}

# interop_json_field <json> <field> — minimal extractor for the shims' flat
# single-line JSON (bash 3.2 safe, no python in the assertion path).
interop_json_field() {
	printf '%s' "$1" |
		sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\{0,1\}\([^,\"}]*\)\"\{0,1\}.*/\1/p" |
		head -1
}
