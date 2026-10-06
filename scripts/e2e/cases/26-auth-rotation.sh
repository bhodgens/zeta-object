#!/usr/bin/env bash
# 26-auth-rotation.sh — SIGHUP key rotation/revocation (design-leaf 08,
# docs/plans/auth-2026-09/extensions/08-key-rotation.md). A private server
# (case 18 pattern: own config.json, own port, create/cleanup pairing) is
# edited on disk and HUP'd — never restarted:
#   26a. baseline: the configured identity (plus the env pair) does a full
#        PUT/GET round-trip.
#   26b. hot-add: append identity ak-new to config.json → kill -HUP →
#        within the retry window ak-new round-trips WITHOUT a restart, and
#        ak-old still works (the rotation window: both keys valid).
#   26c. hot-revoke: remove ak-old from config.json → SIGHUP → ak-old PUT
#        fails with 403 InvalidAccessKeyId; ak-new is unaffected.
#   26d. fail-closed reload: syntactically-valid-but-invalid config
#        (duplicate accessKey) → SIGHUP → the server logs the named-offender
#        error and KEEPS SERVING with the previous registry (ak-new still
#        round-trips) — the startup abort semantics do NOT apply at reload.
#   26e. torn file: truncated JSON → SIGHUP → same fail-closed behavior.
#
# Wire assertions use s3req (curl --aws-sigv4, lib.sh) so the HTTP status and
# <Code> come straight off the wire; log assertions pin the reload contract
# lines (names only — no secrets are ever printed by the reload path).
set -u
A26_ROOT=$(mktemp -d /tmp/e2e26-rotation.XXXXXX)
A26_WORK=$(mktemp -d /tmp/e2e26-work.XXXXXX)
A26_CERT=$(mktemp -d /tmp/e2e26-cert.XXXXXX)
A26_BKT='e2e26-rotation-bkt'
A26_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
openssl req -x509 -newkey rsa:2048 -keyout "$A26_CERT/key.pem" -out "$A26_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
mkdir -p "$A26_WORK/data"
A26_LOG="$A26_WORK/server.log"

a26_cleanup() {
	if [ -n "${A26_PID:-}" ] && kill -0 "$A26_PID" 2>/dev/null; then
		kill -TERM "$A26_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$A26_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$A26_PID" 2>/dev/null
	fi
	rm -rf "$A26_ROOT" "$A26_WORK" "$A26_CERT"
}
trap a26_cleanup EXIT

# a26_write_config <identities-json-body> — write the rotation server's
# config.json with the given identities array contents.
a26_write_config() {
	cat > "$A26_WORK/config.json" <<EOF
{
  "dataDir": "$A26_WORK/data",
  "listenAddr": "127.0.0.1:$A26_PORT",
  "certFile": "$A26_CERT/cert.pem",
  "keyFile": "$A26_CERT/key.pem",
  "identities": [$1]
}
EOF
}

# a26_hup_count — number of completed success reloads in the log so far.
a26_hup_count() {
	grep -c 'SIGHUP auth reload complete' "$A26_LOG" 2>/dev/null || echo 0
}

# a26_hup <expected-success-count> — SIGHUP, then poll (bounded) until the
# log holds <expected-success-count> 'reload complete' lines. Any failure
# line ('reload failed') is checked by the caller explicitly, so waiting for
# the count alone can never mask a fail-closed reload as success.
a26_hup() {
	local want=$1 waited=0 have
	kill -HUP "$A26_PID"
	have=$(a26_hup_count)
	while [ "$have" -lt "$want" ] && [ "$waited" -lt 25 ]; do
		sleep 0.2
		waited=$((waited + 1))
		have=$(a26_hup_count)
	done
	[ "$have" -ge "$want" ]
}

# a26_start_server <access_key> <secret_key> — launch the private server
# whose ENV PAIR is given (the env identity always exists per the migration
# contract); wait for its port. ENDPOINT/BASE_URL are re-pointed at the
# private listener (case 18 pattern) so s3req/aws_ok leave the suite server.
a26_start_server() {
	local ak=$1 sk=$2
	ZETAOBJECT_ACCESS_KEY="$ak" ZETAOBJECT_SECRET_KEY="$sk" \
		ZETAOBJECT_CONFIG="$A26_WORK/config.json" ./zeta-object-server >"$A26_LOG" 2>&1 &
	A26_PID=$!
	ENDPOINT="https://127.0.0.1:$A26_PORT"
	BASE_URL="$ENDPOINT"
	if ! wait_for_port 127.0.0.1 "$A26_PORT" 15; then
		echo '  (rotation server did not start — failing case)'
		E2E_FAIL=$((E2E_FAIL + 1))
		exit 0
	fi
}

# --- 26a: baseline — ak-old configured at startup -----------------------------
a26_write_config '
    {"name": "old", "accessKey": "e2e26-ak-old", "secretKey": "e2e26-sk-old-not-real", "grants": {"*": "readwrite"}}
'
a26_start_server e2e26-env-ak e2e26-env-sk-not-real

# s3req reads AWS_* for signing; point it at the ak-old identity.
export AWS_ACCESS_KEY_ID=e2e26-ak-old AWS_SECRET_ACCESS_KEY=e2e26-sk-old-not-real
export AWS_DEFAULT_REGION=us-east-1 AWS_EC2_METADATA_DISABLED=true
aws_ok '26a baseline: CreateBucket with configured ak-old' s3api create-bucket --bucket "$A26_BKT"
printf 'case26-roundtrip-body' > "$A26_WORK/obj.txt"
aws_ok '26a baseline: PutObject with ak-old' s3api put-object \
	--bucket "$A26_BKT" --key 'case26/obj.txt' --body "$A26_WORK/obj.txt"
aws_ok '26a baseline: GetObject with ak-old' s3api get-object \
	--bucket "$A26_BKT" --key 'case26/obj.txt' "$A26_WORK/got.txt"
if cmp -s "$A26_WORK/obj.txt" "$A26_WORK/got.txt"; then
	assert_eq '26a baseline round-trip byte-exact' same same
else
	assert_eq '26a baseline round-trip byte-exact' same DIFFER
fi

# --- 26b: hot-add ak-new + SIGHUP, no restart ---------------------------------
a26_write_config '
    {"name": "old", "accessKey": "e2e26-ak-old", "secretKey": "e2e26-sk-old-not-real", "grants": {"*": "readwrite"}},
    {"name": "new", "accessKey": "e2e26-ak-new", "secretKey": "e2e26-sk-new-not-real", "grants": {"*": "readwrite"}}
'
if a26_hup 1; then
	assert_eq '26b hot-add: SIGHUP reload completes' 0 0
else
	assert_eq '26b hot-add: SIGHUP reload completes' 0 1
fi
# The SAME process still answers (no restart happened — a restart would have
# re-bound the port and killed this pid).
if kill -0 "$A26_PID" 2>/dev/null; then
	assert_eq '26b hot-add: server process never restarted' 0 0
else
	assert_eq '26b hot-add: server process never restarted' 0 1
fi
# New key works over the wire WITHOUT any restart.
export AWS_ACCESS_KEY_ID=e2e26-ak-new AWS_SECRET_ACCESS_KEY=e2e26-sk-new-not-real
s3req PUT "/$A26_BKT/case26/new.txt" --data-binary 'hot-added'
assert_eq '26b hot-add: new key PutObject WITHOUT restart' 200 "$S3_STATUS"
s3req GET "/$A26_BKT/case26/new.txt"
assert_eq '26b hot-add: new key GetObject' 200 "$S3_STATUS"
# Rotation window: the OLD key still works alongside the new one.
export AWS_ACCESS_KEY_ID=e2e26-ak-old AWS_SECRET_ACCESS_KEY=e2e26-sk-old-not-real
s3req GET "/$A26_BKT/case26/new.txt"
assert_eq '26b rotation window: old key still works' 200 "$S3_STATUS"

# --- 26c: hot-revoke ak-old + SIGHUP ------------------------------------------
a26_write_config '
    {"name": "new", "accessKey": "e2e26-ak-new", "secretKey": "e2e26-sk-new-not-real", "grants": {"*": "readwrite"}}
'
if a26_hup 2; then
	assert_eq '26c hot-revoke: SIGHUP reload completes' 0 0
else
	assert_eq '26c hot-revoke: SIGHUP reload completes' 0 1
fi
export AWS_ACCESS_KEY_ID=e2e26-ak-old AWS_SECRET_ACCESS_KEY=e2e26-sk-old-not-real
s3req PUT "/$A26_BKT/case26/revoked.txt" --data-binary 'must-not-land'
assert_eq '26c revoked key: PutObject rejected' 403 "$S3_STATUS"
assert_s3code '26c revoked key error is InvalidAccessKeyId (unchanged pre-tree error)' 'InvalidAccessKeyId'
export AWS_ACCESS_KEY_ID=e2e26-ak-new AWS_SECRET_ACCESS_KEY=e2e26-sk-new-not-real
s3req GET "/$A26_BKT/case26/new.txt"
assert_eq '26c surviving key unaffected by the revocation' 200 "$S3_STATUS"

# --- 26d: fail-closed reload — duplicate accessKey ------------------------------
a26_write_config '
    {"name": "dup-one", "accessKey": "e2e26-ak-dup", "secretKey": "e2e26-sk-dup-a"},
    {"name": "dup-two", "accessKey": "e2e26-ak-dup", "secretKey": "e2e26-sk-dup-b"}
'
kill -HUP "$A26_PID"
sleep 1
if grep -q 'auth identity reload failed, keeping previous registry' "$A26_LOG"; then
	assert_eq '26d fail-closed: reload failure logged (keep serving)' 0 0
else
	assert_eq '26d fail-closed: reload failure logged (keep serving)' 0 1
fi
if grep -q 'configured more than once' "$A26_LOG"; then
	assert_eq '26d fail-closed: log carries the named-offender error' 0 0
else
	assert_eq '26d fail-closed: log carries the named-offender error' 0 1
fi
if [ "$(a26_hup_count)" -eq 2 ]; then
	assert_eq '26d fail-closed: no success line for the rejected config' 0 0
else
	assert_eq '26d fail-closed: no success line for the rejected config' 0 1
fi
# The PREVIOUS registry (post-26c) is still serving: ak-new round-trips.
export AWS_ACCESS_KEY_ID=e2e26-ak-new AWS_SECRET_ACCESS_KEY=e2e26-sk-new-not-real
s3req PUT "/$A26_BKT/case26/still-writing.txt" --data-binary 'old-registry-serves'
assert_eq '26d fail-closed: previous registry still writes' 200 "$S3_STATUS"
s3req GET "/$A26_BKT/case26/still-writing.txt"
assert_eq '26d fail-closed: previous registry still serves' 200 "$S3_STATUS"
# The rejected config must not leak in: ak-dup was never valid.
export AWS_ACCESS_KEY_ID=e2e26-ak-dup AWS_SECRET_ACCESS_KEY=e2e26-sk-dup-a
s3req GET "/$A26_BKT/case26/still-writing.txt"
assert_eq '26d rejected config key never resolves' 403 "$S3_STATUS"
assert_s3code '26d rejected config key error is InvalidAccessKeyId' 'InvalidAccessKeyId'

# --- 26e: torn file — truncated JSON -------------------------------------------
printf '{"identities": [{"name": "torn' > "$A26_WORK/config.json"
kill -HUP "$A26_PID"
sleep 1
if tail -n 3 "$A26_LOG" | grep -q 'auth identity reload failed, keeping previous registry'; then
	assert_eq '26e torn file: reload failure logged (keep serving)' 0 0
else
	assert_eq '26e torn file: reload failure logged (keep serving)' 0 1
fi
export AWS_ACCESS_KEY_ID=e2e26-ak-new AWS_SECRET_ACCESS_KEY=e2e26-sk-new-not-real
s3req GET "/$A26_BKT/case26/still-writing.txt"
assert_eq '26e torn file: server still serves with the previous registry' 200 "$S3_STATUS"
if [ "$(a26_hup_count)" -eq 2 ]; then
	assert_eq '26e torn file: still no successful reload' 0 0
else
	assert_eq '26e torn file: still no successful reload' 0 1
fi

# Restore the suite credentials so later cases keep using the harness pair.
export AWS_ACCESS_KEY_ID=zetaadmin AWS_SECRET_ACCESS_KEY=zetaadmin
