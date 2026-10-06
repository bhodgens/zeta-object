# 24-owncloud-sync-roundtrip.sh — the ownCloud client workflow in wire form
# (owncloud-2026-09 tree / GH issue #3, leaf 05 Task 3):
#   negotiate via OCS (capabilities first — mirrors the real client's first
#   contact), then sync up / sync down / delete over the WebDAV data plane
#   through the SAME owncloud listener (the /remote.php/webdav/ URL scheme
#   the classic desktop client uses):
#   24a. OCS capabilities probe, then MKCOL + PUT two files (sync up).
#   24b. PROPFIND Depth 1 lists both; GET byte-compares both (sync down).
#   24c. overwrite one file, GET back, compare (sync-up update).
#   24d. DELETE one file; PROPFIND shows absence (delete propagation).
#   24e. degraded probe: a versioning REPORT attempt 405s (no versioning);
#        cleanup deletes the tree; final PROPFIND 404s.
#
# REAL-CLIENT NOTE: the official ownCloud desktop client pass is a MANUAL
# gate — see docs/plans/owncloud-2026-09/decision.md §6. This case pins
# the wire behavior only.
#
# Private-server pattern (cases 14–23).
set -u
OC24_ROOT=$(mktemp -d /tmp/e2e24-owncloud.XXXXXX)
OC24_WORK=$(mktemp -d /tmp/e2e24-work.XXXXXX)
OC24_CERT=$(mktemp -d /tmp/e2e24-cert.XXXXXX)
OC24_BKT='e2e24-owncloud-bkt'
OC24_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
# S3 gets its own port: the owncloud frontend takes a dedicated HTTPS listener,
# so sharing the default listenAddr made two servers race for one port
# (EADDRINUSE on whichever bound second — same rule case 23 documents).
OC24_S3_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
openssl req -x509 -newkey rsa:2048 -keyout "$OC24_CERT/key.pem" -out "$OC24_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
mkdir -p "$OC24_WORK/data"

OC24_USER='oc-sync'
OC24_PASS='oc-sync-pass'
OC24_DAV="/remote.php/webdav/$OC24_BKT"

oc24_cleanup() {
	if [ -n "${OC24_PID:-}" ] && kill -0 "$OC24_PID" 2>/dev/null; then
		kill -TERM "$OC24_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$OC24_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$OC24_PID" 2>/dev/null
	fi
	rm -rf "$OC24_ROOT" "$OC24_WORK" "$OC24_CERT"
}
trap oc24_cleanup EXIT

# oc24_req <method> <url-path> [extra curl args...] — OC24_STATUS/OC24_BODY.
oc24_req() {
	local method=$1 path=$2
	shift 2
	local body_file
	body_file=$(mktemp /tmp/e2e24-req.XXXXXX)
	OC24_STATUS=$(curl -sk -o "$body_file" -w '%{http_code}' \
		--user "${OC24_USER}:${OC24_PASS}" \
		-X "$method" "https://127.0.0.1:$OC24_PORT$path" "$@" 2>/dev/null)
	OC24_BODY=$(cat "$body_file")
	rm -f "$body_file"
}

# --- launch the private server -------------------------------------------------
cat > "$OC24_WORK/config.json" <<EOF
{
  "dataDir": "$OC24_WORK/data",
  "listenAddr": "127.0.0.1:$OC24_S3_PORT",
  "certFile": "$OC24_CERT/cert.pem",
  "keyFile": "$OC24_CERT/key.pem",
  "frontends": [
    {"type": "s3"},
    {"type": "owncloud", "listenAddr": "127.0.0.1:$OC24_PORT", "bucket": "$OC24_BKT"}
  ],
  "identities": [
    {"name": "oc-sync", "accessKey": "$OC24_USER", "secretKey": "$OC24_PASS", "grants": {"*": "readwrite"}}
  ]
}
EOF

ZETAOBJECT_ACCESS_KEY=zetaadmin ZETAOBJECT_SECRET_KEY=zetaadmin \
	ZETAOBJECT_CONFIG="$OC24_WORK/config.json" ./zeta-object-server >"$OC24_WORK/server.log" 2>&1 &
OC24_PID=$!
ENDPOINT="https://127.0.0.1:$OC24_S3_PORT"
BASE_URL="$ENDPOINT"
export E2E_ENDPOINT="$ENDPOINT"
if ! wait_for_port 127.0.0.1 "$OC24_PORT" 15; then
	echo '  (owncloud sync server did not start — failing case)'
	E2E_FAIL=$((E2E_FAIL + 1))
	exit 0
fi

# --- part 24a: negotiate, then sync up ------------------------------------------
# First contact: the client's capabilities probe through the same listener.
oc24_req GET '/ocs/v2.php/cloud/capabilities'
assert_eq 'capabilities probe (first contact)' 200 "$OC24_STATUS"
assert_contains 'capabilities say ok' "$OC24_BODY" '<status>ok</status>'

# MKCOL the sync directory, PUT two files (single-bucket mode: "/" IS the
# bucket, so the WebDAV path is /remote.php/webdav/<dir>).
oc24_req MKCOL "$OC24_DAV/phone-photos/"
assert_eq 'MKCOL sync directory' 201 "$OC24_STATUS"
OC24_F1="$OC24_WORK/upload-one.txt"
OC24_F2="$OC24_WORK/upload-two.txt"
printf 'oc24-file-one-payload' > "$OC24_F1"
printf 'oc24-file-two-payload-different-lengths' > "$OC24_F2"
oc24_req PUT "$OC24_DAV/phone-photos/one.txt" --data-binary @"$OC24_F1"
assert_eq 'PUT one.txt (sync up)' 201 "$OC24_STATUS"
oc24_req PUT "$OC24_DAV/phone-photos/two.txt" --data-binary @"$OC24_F2"
assert_eq 'PUT two.txt (sync up)' 201 "$OC24_STATUS"

# --- part 24b: sync down ---------------------------------------------------------
oc24_req PROPFIND "$OC24_DAV/phone-photos/" -H 'Depth: 1' --data-binary ''
assert_eq 'PROPFIND sync directory' 207 "$OC24_STATUS"
assert_contains 'listing shows one.txt' "$OC24_BODY" 'one.txt'
assert_contains 'listing shows two.txt' "$OC24_BODY" 'two.txt'

oc24_req GET "$OC24_DAV/phone-photos/one.txt"
assert_eq 'GET one.txt' 200 "$OC24_STATUS"
printf '%s' "$OC24_BODY" > "$OC24_WORK/down-one.txt"
cmp -s "$OC24_F1" "$OC24_WORK/down-one.txt"
assert_eq 'one.txt round-trips byte-identical' 0 $?

oc24_req GET "$OC24_DAV/phone-photos/two.txt"
assert_eq 'GET two.txt' 200 "$OC24_STATUS"
printf '%s' "$OC24_BODY" > "$OC24_WORK/down-two.txt"
cmp -s "$OC24_F2" "$OC24_WORK/down-two.txt"
assert_eq 'two.txt round-trips byte-identical' 0 $?

# --- part 24c: sync-up update -----------------------------------------------------
printf 'oc24-file-one-UPDATED-body' > "$OC24_F1"
oc24_req PUT "$OC24_DAV/phone-photos/one.txt" --data-binary @"$OC24_F1"
assert_eq 'PUT overwrite one.txt' 204 "$OC24_STATUS"
oc24_req GET "$OC24_DAV/phone-photos/one.txt"
printf '%s' "$OC24_BODY" > "$OC24_WORK/down-one-v2.txt"
cmp -s "$OC24_F1" "$OC24_WORK/down-one-v2.txt"
assert_eq 'updated one.txt round-trips' 0 $?

# --- part 24d: delete propagation ---------------------------------------------------
oc24_req DELETE "$OC24_DAV/phone-photos/two.txt"
assert_eq 'DELETE two.txt' 204 "$OC24_STATUS"
oc24_req PROPFIND "$OC24_DAV/phone-photos/" -H 'Depth: 1' --data-binary ''
assert_eq 'PROPFIND after delete' 207 "$OC24_STATUS"
if printf '%s' "$OC24_BODY" | grep -q 'two.txt'; then
	assert_eq 'deleted two.txt absent from listing' absent present
else
	assert_eq 'deleted two.txt absent from listing' absent absent
fi
assert_contains 'one.txt still listed' "$OC24_BODY" 'one.txt'

# --- part 24e: degraded versioning probe + cleanup -----------------------------------
# No versioning (provider-less): a versions REPORT is an unimplemented
# method on the data plane → 405 (rejection, not emulation).
oc24_req REPORT "$OC24_DAV/phone-photos/one.txt" --data-binary ''
assert_eq 'versioning REPORT rejected 405' 405 "$OC24_STATUS"

# Cleanup: delete the tree; the final PROPFIND 404s. DELETE of the empty
# directory 404s — collections are VIRTUAL prefixes (Contract 3: a
# collection exists iff its prefix lists), so once the last child is
# gone the dir is already gone. The old 204 expectation contradicted
# that contract (same correction as case 19).
oc24_req DELETE "$OC24_DAV/phone-photos/one.txt"
assert_eq 'cleanup DELETE one.txt' 204 "$OC24_STATUS"
oc24_req DELETE "$OC24_DAV/phone-photos/"
assert_eq 'cleanup DELETE empty directory is already gone' 404 "$OC24_STATUS"
oc24_req PROPFIND "$OC24_DAV/phone-photos/" -H 'Depth: 0' --data-binary ''
assert_eq 'final PROPFIND of deleted dir 404' 404 "$OC24_STATUS"

e2e_finish
