#!/usr/bin/env bash
# mc-runner.sh — client shim for the leaf 5.2 interop matrix (external mc binary).
#
# Dispatch: mc-runner.sh <scenario> [args...]
#
# Requires: INTEROP_MC_BIN (path to the mc binary), INTEROP_ENDPOINT
# (https://host:port). Prints the same single-line JSON contract as
# boto3-runner.py; exit 0 ok / 1 S3 failure / 2 parse or setup error.
#
# mc quirks handled here (per leaf 5.2):
#   - errors surface as mc-rendered messages; the server's S3 code is
#     extracted best-effort (mc_code below)
#   - mc cannot send raw Range headers -> range_get / range_get_invalid
#     assert the closest observable (plain GET of the object)
#   - mc drives multipart automatically (64 MiB threshold); no manual
#     3-phase MPU -> multipart_roundtrip streams a >5 MiB object from
#     stdin and asserts a byte-exact round-trip
#   - mc rm surfaces the missing-key error client-side, so batch_delete
#     counts per-key successes (the server's Deleted array is invisible)
#   - expired presigned GET -> curl the share URL; the wire XML carries
#     AccessDenied, which mc passes through untouched (no re-render)
set -u

MC_BIN=${INTEROP_MC_BIN:?INTEROP_MC_BIN not set}
ENDPOINT=${INTEROP_ENDPOINT:?INTEROP_ENDPOINT not set}
ALIAS=zetaobject

jemit() {
	# jemit <ok 0/1> <status> <code> <data> <detail>
	local ok=false
	[ "$1" = 0 ] && ok=true
	printf '{"ok":%s,"status":%s,"code":"%s","data":"%s","detail":"%s"}\n' \
		"$ok" "$2" "$3" "$4" "$(printf '%s' "$5" | tr -d '"\\')"
	[ "$1" = 0 ] && return 0
	return 1
}

parse_emit() {
	# parse_emit <detail> — client could not be driven/interpreted at all:
	# a FINDING under the frozen parity rule.
	printf '{"parse_error":true,"detail":"%s"}\n' \
		"$(printf '%s' "$1" | tr -d '"\\')"
	return 2
}

# mc_code <stderr-json> — best-effort S3 error-code extraction. mc re-renders
# errors client-side; newer builds carry a "code" field, older ones only
# human text — map both.
mc_code() {
	local err=$1 code=''
	# mc embeds the server S3 code in error.cause.error.Code (nested JSON);
	# match the field name case-insensitively, at any nesting depth.
	code=$(printf '%s' "$err" | sed -n 's/.*"[Cc]ode":"\([A-Za-z]*\)".*/\1/p' | head -1)
	if [ -z "$code" ]; then
		case "$err" in
		*BucketAlreadyOwnedByYou* | *"already exists"*) code=BucketAlreadyOwnedByYou ;;
		*BucketNotEmpty*)                               code=BucketNotEmpty ;;
		*NoSuchKey* | *"Object does not exist"*)        code=NoSuchKey ;;
		*NoSuchBucket* | *"does not exist"*)            code=NoSuchBucket ;;
		*AccessDenied* | *"Access Denied"*)             code=AccessDenied ;;
		*InvalidBucketName*)                            code=InvalidBucketName ;;
		*InvalidRange*)                                 code=InvalidRange ;;
		*EntityTooSmall*)                               code=EntityTooSmall ;;
		esac
	fi
	printf '%s' "$code"
}

# xml_code <body> — <Code> straight from the wire (used when mc hands us the
# raw response, e.g. fetching a share URL with curl).
xml_code() {
	printf '%s' "$1" | sed -n 's/.*<Code>\([^<]*\)<\/Code>.*/\1/p' | head -1
}

# mcrun <args...> — run mc with --json; rc + MC_OUT (stdout) + MC_ERR (stderr).
MC_OUT=''; MC_ERR=''
mcrun() {
	MC_ERR=$(mktemp /tmp/interop-mc-err.XXXXXX)
	MC_OUT=$("$MC_BIN" --insecure --json "$@" 2>"$MC_ERR")
	local rc=$?
	MC_ERR=$(cat "$MC_ERR")
	rm -f "$MC_ERR" 2>/dev/null
	return $rc
}

# mcrun_to <file> <args...> — like mcrun but streams mc's stdout to <file>
# (binary-safe: command substitution would strip NULs/newlines and mix
# progress JSON with payload bytes).
MC_ERR2=''
mcrun_to() {
	local f=$1
	shift
	MC_ERR2=$(mktemp /tmp/interop-mc-err.XXXXXX)
	"$MC_BIN" --insecure --json "$@" >"$f" 2>"$MC_ERR2"
	local rc=$?
	MC_OUT=$(cat "$f")
	MC_ERR=$(cat "$MC_ERR2")
	rm -f "$MC_ERR2"
	return $rc
}

# sha16 <file> — 16-hex sha256 prefix (matches boto3-runner's h()).
sha16() {
	shasum -a 256 "$1" | cut -c1-16
}

# run_fail <scenario-detail> — emit a failure with best-effort code.
# NOTE: mc writes error JSON to STDOUT (even with --json), not stderr.
run_fail() {
	local detail=$1 err code
	err="$MC_OUT$MC_ERR"
	code=$(mc_code "$err")
	[ -z "$detail" ] && detail=$(printf '%.160s' "$err")
	jemit 1 0 "$code" "" "$detail"
}

main() {
	local scenario=$1
	shift
	local rc=0 data='' detail=''

	case "$scenario" in
	bucket_create)
		mcrun mb "$ALIAS/$1" || { rc=1; }
		;;
	bucket_create_dup)
		mcrun mb "$ALIAS/$1" || { rc=1; }
		;;
	bucket_head)
		mcrun stat "$ALIAS/$1" || { rc=1; }
		;;
	bucket_head_missing)
		# trailing slash forces bucket (folder) semantics so the server's
		# real Code=NoSuchBucket survives mc's error rendering.
		mcrun stat "$ALIAS/$1/" || { rc=1; }
		;;
	bucket_list)
		if mcrun ls "$ALIAS/"; then
			if printf '%s' "$MC_OUT" | grep -q "\"key\":\"$1/"; then
				data=1
			else
				data=0
				rc=1
			fi
		else
			rc=1
		fi
		;;
	bucket_delete)
		mcrun rb --force "$ALIAS/$1" || { rc=1; }
		;;
	object_put)
		# object_put <bucket> <key> <payload> (argv — payload is small text)
		local pf
		pf=$(mktemp /tmp/interop-mc.XXXXXX)
		printf '%s' "$3" >"$pf"
		if mcrun cp "$pf" "$ALIAS/$1/$2"; then
			data=$(sha16 "$pf")
		else
			rc=1
		fi
		rm -f "$pf"
		;;
	object_get_roundtrip)
		# object_get_roundtrip <bucket> <key> <payload-hash-ignored>
		local gf
		gf=$(mktemp /tmp/interop-mc.XXXXXX)
		if mcrun_to "$gf" cat "$ALIAS/$1/$2"; then
			data=$(sha16 "$gf")
		else
			rc=1
		fi
		rm -f "$gf"
		;;
	unicode_key)
		# unicode_key <bucket> <unicode-key>: put+get round-trip, byte-exact.
		local pf gf
		pf=$(mktemp /tmp/interop-mc.XXXXXX)
		gf=$(mktemp /tmp/interop-mc.XXXXXX)
		printf 'unicode-payload' >"$pf"
		# cat WITHOUT --json: mc's JSON status line would append to the
		# redirected stdout and break the byte-exact cmp.
		if mcrun cp "$pf" "$ALIAS/$1/$2" \
			&& "$MC_BIN" --insecure cat "$ALIAS/$1/$2" >"$gf" 2>/dev/null \
			&& cmp -s "$pf" "$gf"; then
			data='unicode-ok'
		else
			rc=1
		fi
		rm -f "$pf" "$gf"
		;;
	error_nosuchbucket)
		mcrun cat "$ALIAS/e2e-12-no-such-bucket-xyz/k" || { rc=1; }
		;;
	error_nosuchkey)
		mcrun cat "$ALIAS/$1/does-not-exist" || { rc=1; }
		;;
	error_accessdenied)
		# AccessDenied via an EXPIRED presigned URL (parity: case 07's
		# expired-URL branch). mc share download --expire 1s, wait, curl:
		# the 403 wire XML carries AccessDenied which we read directly.
		local purl pfile code xcode key=${2:-plain.txt}
		pfile=$(mktemp /tmp/interop-mc.XXXXXX)
		if mcrun share download --expire 1s "$ALIAS/$1/$key"; then
			purl=$(printf '%s' "$MC_OUT" | sed -n 's/.*"share":"\([^"]*\)".*/\1/p' | head -1)
			[ -z "$purl" ] && purl=$(printf '%s' "$MC_OUT" | sed -n 's/.*"url":"\([^"]*\)".*/\1/p' | head -1)
			if [ -n "$purl" ]; then
				sleep 2
				code=$(curl -ks -o "$pfile" -w '%{http_code}' "$purl" 2>/dev/null)
				xcode=$(xml_code "$(cat "$pfile")")
				detail="presigned-after-expiry status=$code code=$xcode"
			else
				rc=2
				detail='no url in share output'
			fi
		else
			rc=1
		fi
		rm -f "$pfile"
		[ "$rc" = 2 ] && { parse_emit "$detail"; return; }
		if [ "$rc" = 0 ]; then
			# success path of this scenario = the fetch must FAIL with
			# AccessDenied (the code comes off the wire XML directly).
			jemit 1 "${code:-0}" "$xcode" "" "$detail"
			return
		fi
		;;
	list_prefix)
		if mcrun ls --recursive "$ALIAS/$1/pre/"; then
			data=$(printf '%s' "$MC_OUT" | grep -c '"key"' || true)
		else
			rc=1
		fi
		;;
	range_get | range_get_invalid)
		# mc cannot send Range headers (no CLI flag); closest observable:
		# plain full GET of the object (leaf 5.2 tolerance clause).
		local gf
		gf=$(mktemp /tmp/interop-mc.XXXXXX)
		if mcrun_to "$gf" cat "$ALIAS/$1/$2"; then
			data=$(sha16 "$gf")
		else
			rc=1
		fi
		rm -f "$gf"
		;;
	multipart_roundtrip)
		# multipart_roundtrip <bucket>: 5 MiB object streamed on stdin,
		# byte-exact round-trip (mc's own multipart path needs 64 MiB).
		local big gf
		big=$(mktemp /tmp/interop-mc.XXXXXX)
		gf=$(mktemp /tmp/interop-mc.XXXXXX)
		cat >"$big"
		if mcrun cp "$big" "$ALIAS/$1/big.bin" \
			&& mcrun_to "$gf" cat "$ALIAS/$1/big.bin" \
			&& cmp -s "$big" "$gf"; then
			data='mpu-ok'
		else
			rc=1
		fi
		rm -f "$big" "$gf"
		;;
	presigned_get)
		# presigned_get <bucket> <key> <payload-hash>: mc share download,
		# curl the URL, hash the payload.
		local purl pf code
		pf=$(mktemp /tmp/interop-mc.XXXXXX)
		if mcrun share download --expire 5m "$ALIAS/$1/$2"; then
			purl=$(printf '%s' "$MC_OUT" | sed -n 's/.*"share":"\([^"]*\)".*/\1/p' | head -1)
			[ -z "$purl" ] && purl=$(printf '%s' "$MC_OUT" | sed -n 's/.*"url":"\([^"]*\)".*/\1/p' | head -1)
			if [ -n "$purl" ]; then
				code=$(curl -ks -o "$pf" -w '%{http_code}' "$purl" 2>/dev/null)
				if [ "$code" = 200 ]; then
					data=$(sha16 "$pf")
				else
					rc=1
					detail="presigned fetch status=$code"
				fi
			else
				rc=2
				detail='no url in share output'
			fi
		else
			rc=1
		fi
		rm -f "$pf"
		[ "$rc" = 2 ] && { parse_emit "$detail"; return; }
		;;
	copy_object)
		# copy_object <dst-bucket> <dst-key> <src-bucket> <src-key> [hash]
		local gf
		gf=$(mktemp /tmp/interop-mc.XXXXXX)
		if mcrun cp "$ALIAS/$3/$4" "$ALIAS/$1/$2" \
			&& mcrun_to "$gf" cat "$ALIAS/$1/$2"; then
			data=$(sha16 "$gf")
		else
			rc=1
		fi
		rm -f "$gf"
		;;
	batch_delete)
		# batch_delete <bucket> <keys...>: count keys mc successfully
		# removed (the missing one fails client-side — its code is noted).
		local deleted=0 k
		for k in "$@"; do
			if mcrun rm "$ALIAS/$1/$k"; then
				deleted=$((deleted + 1))
			else
				detail="$detail $k:$(mc_code "$MC_ERR")"
			fi
		done
		data=$deleted
		;;
	*)
		parse_emit "unknown scenario: $scenario"
		return
		;;
	esac

	if [ "$rc" = 2 ]; then
		parse_emit "$detail"
		return
	fi
	if [ "$rc" = 1 ]; then
		# batch_delete already produced its data (count); emit with code.
		local code
		code=$(mc_code "$MC_OUT$MC_ERR")
		jemit 1 0 "$code" "$data" "$detail"
		return
	fi
	jemit 0 200 '' "$data" "$detail"
}

main "$@"
