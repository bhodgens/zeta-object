# 25-owncloud-props.sh — pins the ownCloud-namespace discovery properties
# (GH issue #5): every PROPFIND 207 reply (discovery + children, trailing
# slash AND the slash-less owncloudcmd form) must carry
#   xmlns:oc="http://owncloud.org/ns" on the multistatus root,
#   oc:fileid       on every file and collection (derived: stable across
#                   requests, distinct file vs parent collection),
#   oc:permissions  on every resource (readwrite: RW file / RDNVCK dir),
#   oc:size         on collections (subtree aggregate).
# These are what the real client's DiscoverySingleDirectoryJob requires;
# without them sync aborts with "reply is missing data".
#
# Private-server pattern (cases 14-24).
set -u
OC25_ROOT=$(mktemp -d /tmp/e2e25-owncloud.XXXXXX)
OC25_WORK=$(mktemp -d /tmp/e2e25-work.XXXXXX)
OC25_CERT=$(mktemp -d /tmp/e2e25-cert.XXXXXX)
OC25_BKT='e2e25-owncloud-bkt'
# A dedicated frontend listenAddr CANNOT share the global listenAddr port:
# buildDedicatedListeners races the default HTTPS listener for the port and
# one ListenAndServeTLS fails, aborting startup (the rule case 19/20
# document). The owncloud frontend gets its OWN port.
OC25_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
OC25_S3_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
openssl req -x509 -newkey rsa:2048 -keyout "$OC25_CERT/key.pem" -out "$OC25_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
mkdir -p "$OC25_WORK/data"

OC25_USER='oc-props'
OC25_PASS='oc-props-pass'
OC25_DAV="/remote.php/webdav/$OC25_BKT"
OC25_NS='http://owncloud.org/ns'

oc25_cleanup() {
	if [ -n "${OC25_PID:-}" ] && kill -0 "$OC25_PID" 2>/dev/null; then
		kill -TERM "$OC25_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$OC25_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$OC25_PID" 2>/dev/null
	fi
	rm -rf "$OC25_ROOT" "$OC25_WORK" "$OC25_CERT"
}
trap oc25_cleanup EXIT

# oc25_req <method> <url-path> [extra curl args...] — OC25_STATUS/OC25_BODY.
oc25_req() {
	local method=$1 path=$2
	shift 2
	local body_file
	body_file=$(mktemp /tmp/e2e25-req.XXXXXX)
	OC25_STATUS=$(curl -sk -o "$body_file" -w '%{http_code}' \
		--user "${OC25_USER}:${OC25_PASS}" \
		-X "$method" "https://127.0.0.1:$OC25_PORT$path" "$@" 2>/dev/null)
	OC25_BODY=$(cat "$body_file")
	rm -f "$body_file"
}

# --- launch the private server -------------------------------------------------
cat > "$OC25_WORK/config.json" <<EOF
{
  "dataDir": "$OC25_WORK/data",
  "listenAddr": "127.0.0.1:$OC25_S3_PORT",
  "certFile": "$OC25_CERT/cert.pem",
  "keyFile": "$OC25_CERT/key.pem",
  "frontends": [
    {"type": "s3"},
    {"type": "owncloud", "listenAddr": "127.0.0.1:$OC25_PORT", "bucket": "$OC25_BKT"}
  ],
  "identities": [
    {"name": "oc-props", "accessKey": "$OC25_USER", "secretKey": "$OC25_PASS", "grants": {"*": "readwrite"}}
  ]
}
EOF

ZETAOBJECT_ACCESS_KEY=zetaadmin ZETAOBJECT_SECRET_KEY=zetaadmin \
	ZETAOBJECT_CONFIG="$OC25_WORK/config.json" ./zeta-object-server >"$OC25_WORK/server.log" 2>&1 &
OC25_PID=$!
ENDPOINT="https://127.0.0.1:$OC25_PORT"
BASE_URL="$ENDPOINT"
export E2E_ENDPOINT="$ENDPOINT"
if ! wait_for_port 127.0.0.1 "$OC25_PORT" 15; then
	echo '  (owncloud props server did not start — failing case)'
	E2E_FAIL=$((E2E_FAIL + 1))
	exit 0
fi

# --- seed: one collection (two files) --------------------------------------------
# Collections are virtual prefixes: the first PUT under reports/ brings the
# collection into existence (an explicit MKCOL of a never-written prefix is a
# documented 409 — missing parent — and the real client creates directories
# implicitly by writing files into them).
printf 'q1-body' > "$OC25_WORK/q1.txt"        # 7 bytes
printf 'q2-body-longer' > "$OC25_WORK/q2.txt"  # 14 bytes
oc25_req PUT "$OC25_DAV/reports/q1.txt" --data-binary @"$OC25_WORK/q1.txt"
assert_eq 'PUT reports/q1.txt (creates the prefix)' 201 "$OC25_STATUS"
oc25_req PUT "$OC25_DAV/reports/q2.txt" --data-binary @"$OC25_WORK/q2.txt"
assert_eq 'PUT reports/q2.txt' 201 "$OC25_STATUS"

# --- 25a: slash-less discovery PROPFIND (the owncloudcmd form) -------------------
# F-oc-1 relaxation: no trailing slash, Depth 1 — oc: props MUST appear.
oc25_req PROPFIND "$OC25_DAV/reports" -H 'Depth: 1' --data-binary ''
assert_eq 'slash-less discovery PROPFIND 207' 207 "$OC25_STATUS"
assert_contains 'xmlns:oc declared on multistatus root' "$OC25_BODY" "xmlns:oc=\"$OC25_NS\""
assert_contains 'oc:fileid present (collection)' "$OC25_BODY" "<oc:fileid>"
assert_contains 'oc:permissions present' "$OC25_BODY" "<oc:permissions>RDNVCK</oc:permissions>"
assert_contains 'oc:size present on collection' "$OC25_BODY" "<oc:size>21</oc:size>"

# --- 25b: oc:fileid is stable across two requests --------------------------------
oc25_req PROPFIND "$OC25_DAV/reports" -H 'Depth: 1' --data-binary ''
assert_eq 'second discovery PROPFIND 207' 207 "$OC25_STATUS"
OC25_ID_1=$(printf '%s' "$OC25_BODY" | grep -o "<oc:fileid>[0-9]*</oc:fileid>" | head -1)
oc25_req PROPFIND "$OC25_DAV/reports" -H 'Depth: 1' --data-binary ''
OC25_ID_2=$(printf '%s' "$OC25_BODY" | grep -o "<oc:fileid>[0-9]*</oc:fileid>" | head -1)
assert_eq 'oc:fileid stable across requests' "$OC25_ID_1" "$OC25_ID_2"

# --- 25c: file fileid differs from its parent collection's ------------------------
# Discovery Depth 0 on the collection vs Depth 0 on the file: the fileids
# must differ (derived from bucket+key).
oc25_req PROPFIND "$OC25_DAV/reports" -H 'Depth: 0' --data-binary ''
OC25_DIR_ID=$(printf '%s' "$OC25_BODY" | grep -o "<oc:fileid>[0-9]*</oc:fileid>" | head -1 | grep -o '[0-9]*')
assert_eq 'collection Depth 0 PROPFIND 207' 207 "$OC25_STATUS"
oc25_req PROPFIND "$OC25_DAV/reports/q1.txt" -H 'Depth: 0' --data-binary ''
OC25_FILE_ID=$(printf '%s' "$OC25_BODY" | grep -o "<oc:fileid>[0-9]*</oc:fileid>" | head -1 | grep -o '[0-9]*')
assert_eq 'file Depth 0 PROPFIND 207' 207 "$OC25_STATUS"
assert_contains 'file carries readwrite RW permissions' "$OC25_BODY" "<oc:permissions>RW</oc:permissions>"
if [ "$OC25_DIR_ID" != "$OC25_FILE_ID" ] && [ -n "$OC25_DIR_ID" ] && [ -n "$OC25_FILE_ID" ]; then
	assert_eq 'file fileid differs from parent collection' differ differ
else
	assert_eq 'file fileid differs from parent collection' differ "same($OC25_DIR_ID)"
fi

# --- 25d: trailing-slash form carries the same oc: properties ----------------------
oc25_req PROPFIND "$OC25_DAV/reports/" -H 'Depth: 1' --data-binary ''
assert_eq 'trailing-slash PROPFIND 207' 207 "$OC25_STATUS"
assert_contains 'trailing-slash: xmlns:oc present' "$OC25_BODY" "xmlns:oc=\"$OC25_NS\""
assert_contains 'trailing-slash: oc:fileid present' "$OC25_BODY" "<oc:fileid>"
# oc:size aggregates the whole subtree (7 + 14 = 21).
assert_contains 'oc:size subtree aggregate (7+14=21)' "$OC25_BODY" "<oc:size>21</oc:size>"

# --- 25e: allprop body naming oc: props (the named-prop discovery form) -------------
OC25_PROP_BODY="<?xml version=\"1.0\"?><D:propfind xmlns:D=\"DAV:\" xmlns:oc=\"$OC25_NS\"><D:prop><oc:fileid/><oc:permissions/><oc:size/></D:prop></D:propfind>"
oc25_req PROPFIND "$OC25_DAV/reports" -H 'Depth: 1' --data-raw "$OC25_PROP_BODY"
assert_eq 'named oc: prop PROPFIND 207' 207 "$OC25_STATUS"
assert_contains 'named prop: fileid returned' "$OC25_BODY" "<oc:fileid>"
assert_contains 'named prop: permissions returned' "$OC25_BODY" "<oc:permissions>"
assert_contains 'named prop: size returned' "$OC25_BODY" "<oc:size>"

# --- cleanup -------------------------------------------------------------------------
oc25_req DELETE "$OC25_DAV/reports/q1.txt"
assert_eq 'cleanup DELETE q1.txt' 204 "$OC25_STATUS"
oc25_req DELETE "$OC25_DAV/reports/q2.txt"
assert_eq 'cleanup DELETE q2.txt' 204 "$OC25_STATUS"
# The collection is a virtual prefix: once the last key under it is
# deleted the prefix vanishes, so this DELETE finds nothing (404) —
# the same semantics case 24 relies on for its final PROPFIND.
oc25_req DELETE "$OC25_DAV/reports/"
assert_eq 'cleanup DELETE reports/ (already gone) 404' 404 "$OC25_STATUS"

e2e_finish
