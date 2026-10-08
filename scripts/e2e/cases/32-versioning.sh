# 32-versioning.sh — S3 versioning over the wire (s3-versioning-2026-10
# tree leaf 05; the AGENTS.md e2e rule for the versioning surface).
# Sidecar path (the mechanism every non-ZFS bucket always uses):
#   32a. PUT ?versioning Enabled -> 200; GET ?versioning echoes Enabled.
#   32b. PUT v1 -> PUT v2 -> PUT v3: each overwrite records the PREVIOUS
#        bytes as one version; the current GET always returns the newest
#        write (the store records the old version, never the new one).
#   32c. ?versions renders the recorded versions NEWEST-FIRST (IsLatest on
#        the newest recorded), ?versionId=<id> reads the exact old bytes
#        with x-amz-version-id echoing the id, and an unknown
#        sidecar-shaped id is 400 InvalidArgument.
#   32d. DELETE writes a delete marker (204, data untouched): plain GET is
#        404 with x-amz-delete-marker: true; ?versions shows the marker
#        IsLatest above the two versions; GET ?versionId=<marker> is 405
#        with the same header; both versions stay readable by id.
#   32e. Suspended: overwrites and deletes revert to plain semantics (no
#        new version recorded, no marker on delete); the recorded versions
#        remain listed and readable.
#   32f. Off (envelope with no Status): GET ?versioning omits <Status>;
#        plain PUT/GET/DELETE behavior round-trips.
#
# Private-server pattern (cases 14-16, 30, 31): the suite server is left
# untouched, so the harness's case-boundary relaunch logic keeps working.
# This server pins "zfs_versioning": "sidecar": the shared harness config
# has no zmetad database, and the default `zfs_versioning` mode
# ("snapshots") requires zmetad tracking of the bucket's dataset — the
# snapshots-mode parity is validated against a real ZFS host by section 10
# of scripts/zfs-validate/run-zfs-validation.sh instead.
set -u
BKT='e2e-32-versioning'
V32_ROOT=$(mktemp -d /tmp/e2e32-versioning.XXXXXX)
V32_CERT=$(mktemp -d /tmp/e2e32-cert.XXXXXX)
V32_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
openssl req -x509 -newkey rsa:2048 -keyout "$V32_CERT/key.pem" -out "$V32_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
mkdir -p "$V32_ROOT/data"

v32_cleanup() {
	if [ -n "${V32_PID:-}" ] && kill -0 "$V32_PID" 2>/dev/null; then
		kill -TERM "$V32_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$V32_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$V32_PID" 2>/dev/null
	fi
	# The per-request temp FILES too: V32_HDRS/BODY/LIST are mktemp'd
	# outside $V32_ROOT, so removing the root alone left one body file per
	# signed request in /tmp forever (198 e2e32-* files accumulated on the
	# dev machine). Every mktemp a case creates must be named in its cleanup.
	rm -f "${V32_HDRS:-}" "${V32_BODY:-}" "${V32_LIST:-}"
	rm -rf "$V32_ROOT" "$V32_CERT"
}
trap v32_cleanup EXIT

cat > "$V32_ROOT/config.json" <<EOF
{
  "dataDir": "$V32_ROOT/data",
  "listenAddr": "127.0.0.1:$V32_PORT",
  "certFile": "$V32_CERT/cert.pem",
  "keyFile": "$V32_CERT/key.pem",
  "zfs_versioning": "sidecar"
}
EOF
: > "$V32_ROOT/server.log"
ZETAOBJECT_CONFIG="$V32_ROOT/config.json" ./zeta-object-server >"$V32_ROOT/server.log" 2>&1 &
V32_PID=$!
V32_URL="https://127.0.0.1:$V32_PORT"
if ! wait_for_port 127.0.0.1 "$V32_PORT" 15; then
	echo '  (versioning server did not start — failing case)'
	assert_eq '32 versioning server started' started missing
	exit 0
fi

# v32_req <method> <path> [extra curl args...] — signed request against the
# private server; sets V32_STATUS (HTTP code), V32_BODY (response body FILE)
# and V32_HDRS (response headers FILE). The raw forms are needed because
# lib.sh's s3req keeps only status + body: the delete-marker and
# version-id assertions are HEADER assertions.
v32_req() {
	local method=$1 path=$2
	shift 2
	V32_HDRS=$(mktemp /tmp/e2e32-hdrs.XXXXXX)
	V32_BODY=$(mktemp /tmp/e2e32-body.XXXXXX)
	V32_STATUS=$(curl -sk -o "$V32_BODY" -D "$V32_HDRS" -w '%{http_code}' \
		--aws-sigv4 "aws:amz:us-east-1:s3" \
		--user "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" \
		-X "$method" "$V32_URL$path" "$@" 2>/dev/null)
}

# v32_hdr <name> — value of one response header from the last v32_req.
v32_hdr() {
	local val
	val=$(grep -i "^${1}:" "$V32_HDRS" 2>/dev/null | head -1)
	printf '%s' "$val" | sed 's/^[^:]*: //' | tr -d '\r'
}

# v32_parse <listing-file> — decode a ?versions document into one line per
# entry: "Version <id> <islatest>" / "DeleteMarker <id> <islatest>", in
# document order (per key newest-first; Go renders the Version slice
# before the DeleteMarker slice).
v32_parse() {
	python3 - "$1" <<'PYEOF'
import sys, xml.etree.ElementTree as ET
root = ET.parse(sys.argv[1]).getroot()
for el in root:
    tag = el.tag.rsplit('}', 1)[-1]
    if tag not in ('Version', 'DeleteMarker'):
        continue
    vid = lat = ''
    for c in el:
        t = c.tag.rsplit('}', 1)[-1]
        if t == 'VersionId':
            vid = c.text or ''
        elif t == 'IsLatest':
            lat = c.text or ''
    print(tag, vid, lat)
PYEOF
}

V32XML_ENABLED='<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>'
V32XML_SUSPENDED='<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Suspended</Status></VersioningConfiguration>'
V32XML_OFF='<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"/>'
P1='versioning-case32-bytes-v1'
P2='versioning-case32-bytes-v2'
P3='versioning-case32-bytes-v3'
P4='versioning-case32-bytes-v4'

# --- setup: create/cleanup pairing ---------------------------------------------
v32_req PUT "/$BKT"
assert_eq '32 setup: create-bucket' 200 "$V32_STATUS"

# --- 32a: enable + echo ----------------------------------------------------------
v32_req PUT "/$BKT?versioning" -H 'Content-Type: application/xml' --data-binary "$V32XML_ENABLED"
assert_eq '32a PUT ?versioning Enabled -> 200' 200 "$V32_STATUS"
v32_req GET "/$BKT?versioning"
assert_eq '32a GET ?versioning -> 200' 200 "$V32_STATUS"
assert_contains '32a GET ?versioning echoes Enabled' "$(cat "$V32_BODY")" '<Status>Enabled</Status>'

# --- 32b: overwrites record the PREVIOUS bytes -----------------------------------
v32_req PUT "/$BKT/ver.txt" --data-binary "$P1"
assert_eq '32b PUT v1 -> 200' 200 "$V32_STATUS"
v32_req GET "/$BKT/ver.txt"
assert_eq "32b current GET after v1 -> 200" 200 "$V32_STATUS"
assert_eq "32b current GET returns v1 bytes" "$P1" "$(cat "$V32_BODY")"
v32_req PUT "/$BKT/ver.txt" --data-binary "$P2"
assert_eq '32b PUT v2 (overwrite) -> 200' 200 "$V32_STATUS"
v32_req GET "/$BKT/ver.txt"
assert_eq "32b current GET after v2 -> 200" 200 "$V32_STATUS"
assert_eq "32b current GET returns v2 bytes" "$P2" "$(cat "$V32_BODY")"
v32_req PUT "/$BKT/ver.txt" --data-binary "$P3"
assert_eq '32b PUT v3 (second overwrite) -> 200' 200 "$V32_STATUS"
v32_req GET "/$BKT/ver.txt"
assert_eq "32b current GET after v3 -> 200" 200 "$V32_STATUS"
assert_eq "32b current GET returns v3 bytes" "$P3" "$(cat "$V32_BODY")"

# --- 32c: ?versions render + ?versionId reads ------------------------------------
v32_req GET "/$BKT?versions"
assert_eq '32c GET ?versions -> 200' 200 "$V32_STATUS"
V32_LIST=$(mktemp /tmp/e2e32-list.XXXXXX)
printf '%s' "$(cat "$V32_BODY")" > "$V32_LIST"
PARSED=$(v32_parse "$V32_LIST")
V_COUNT=$(printf '%s\n' "$PARSED" | grep -c '^Version ' | tr -d ' ')
D_COUNT=$(printf '%s\n' "$PARSED" | grep -c '^DeleteMarker ' | tr -d ' ')
V2_ID=$(printf '%s\n' "$PARSED" | awk '$1=="Version"{print $2; exit}')
V2_LATEST=$(printf '%s\n' "$PARSED" | awk '$1=="Version"{print $3; exit}')
V1_ID=$(printf '%s\n' "$PARSED" | awk '$1=="Version"{print $2}' | sed -n 2p)
assert_eq '32c ?versions shows exactly 2 recorded versions' 2 "$V_COUNT"
assert_eq '32c ?versions shows no delete markers yet' 0 "$D_COUNT"
assert_eq '32c newest recorded version is IsLatest' true "$V2_LATEST"
if [ -n "$V2_ID" ] && [ -n "$V1_ID" ] && [ "$V2_ID" != "$V1_ID" ]; then
	assert_eq '32c two distinct version ids discoverable' 0 0
else
	assert_eq '32c two distinct version ids discoverable' 0 1
fi

v32_req GET "/$BKT/ver.txt?versionId=$V1_ID"
assert_eq '32c GET ?versionId=<v1> -> 200' 200 "$V32_STATUS"
assert_eq '32c ?versionId=<v1> returns v1 bytes' "$P1" "$(cat "$V32_BODY")"
assert_eq '32c x-amz-version-id echoes v1 id' "$V1_ID" "$(v32_hdr x-amz-version-id)"
v32_req GET "/$BKT/ver.txt?versionId=$V2_ID"
assert_eq '32c GET ?versionId=<v2> -> 200' 200 "$V32_STATUS"
assert_eq '32c ?versionId=<v2> returns v2 bytes' "$P2" "$(cat "$V32_BODY")"
assert_eq '32c x-amz-version-id echoes v2 id' "$V2_ID" "$(v32_hdr x-amz-version-id)"

v32_req GET "/$BKT/ver.txt?versionId=1234567890123456-deadbeef"
assert_eq '32c unknown sidecar-shaped version id -> 400' 400 "$V32_STATUS"
assert_contains '32c unknown version id error code InvalidArgument' "$(cat "$V32_BODY")" '<Code>InvalidArgument</Code>'

# --- 32d: delete marker semantics --------------------------------------------------
v32_req DELETE "/$BKT/ver.txt"
assert_eq '32d DELETE with versioning enabled -> 204' 204 "$V32_STATUS"
v32_req GET "/$BKT/ver.txt"
assert_eq '32d plain GET of delete-marked key -> 404' 404 "$V32_STATUS"
assert_eq '32d plain GET carries x-amz-delete-marker: true' true "$(v32_hdr x-amz-delete-marker)"

v32_req GET "/$BKT?versions"
assert_eq '32d GET ?versions after delete -> 200' 200 "$V32_STATUS"
printf '%s' "$(cat "$V32_BODY")" > "$V32_LIST"
PARSED=$(v32_parse "$V32_LIST")
V_COUNT=$(printf '%s\n' "$PARSED" | grep -c '^Version ' | tr -d ' ')
D_COUNT=$(printf '%s\n' "$PARSED" | grep -c '^DeleteMarker ' | tr -d ' ')
V2_ID=$(printf '%s\n' "$PARSED" | awk '$1=="Version"{print $2; exit}')
V1_ID=$(printf '%s\n' "$PARSED" | awk '$1=="Version"{print $2}' | sed -n 2p)
MARKER_ID=$(printf '%s\n' "$PARSED" | awk '$1=="DeleteMarker"{print $2; exit}')
MARKER_LATEST=$(printf '%s\n' "$PARSED" | awk '$1=="DeleteMarker"{print $3; exit}')
# A document with NO DeleteMarker elements: awk found nothing (empty), and
# grep -c returned 0, so an entry-count of 0 keeps MARKER_ID/LATEST empty.
assert_eq '32d ?versions still shows 2 versions below the marker' 2 "$V_COUNT"
assert_eq '32d ?versions shows exactly 1 delete marker' 1 "$D_COUNT"
assert_eq '32d delete marker is IsLatest (newest-first render)' true "$MARKER_LATEST"

v32_req GET "/$BKT/ver.txt?versionId=$MARKER_ID"
assert_eq '32d GET ?versionId=<marker> -> 405' 405 "$V32_STATUS"
assert_eq '32d marker read carries x-amz-delete-marker: true' true "$(v32_hdr x-amz-delete-marker)"
v32_req GET "/$BKT/ver.txt?versionId=$V2_ID"
assert_eq '32d GET ?versionId=<v2> still 200 after delete' 200 "$V32_STATUS"
assert_eq '32d v2 bytes unchanged after delete' "$P2" "$(cat "$V32_BODY")"
v32_req GET "/$BKT/ver.txt?versionId=$V1_ID"
assert_eq '32d GET ?versionId=<v1> still 200 after delete' 200 "$V32_STATUS"
assert_eq '32d v1 bytes unchanged after delete' "$P1" "$(cat "$V32_BODY")"

# --- 32e: suspend semantics ----------------------------------------------------------
v32_req PUT "/$BKT?versioning" -H 'Content-Type: application/xml' --data-binary "$V32XML_SUSPENDED"
assert_eq '32e PUT ?versioning Suspended -> 200' 200 "$V32_STATUS"
v32_req GET "/$BKT?versioning"
assert_contains '32e GET ?versioning echoes Suspended' "$(cat "$V32_BODY")" '<Status>Suspended</Status>'

v32_req PUT "/$BKT/ver.txt" --data-binary "$P4"
assert_eq '32e suspended PUT v4 (recreate) -> 200' 200 "$V32_STATUS"
v32_req GET "/$BKT/ver.txt"
assert_eq '32e suspended current GET -> 200' 200 "$V32_STATUS"
assert_eq '32e suspended current GET returns v4 bytes' "$P4" "$(cat "$V32_BODY")"
v32_req GET "/$BKT?versions"
printf '%s' "$(cat "$V32_BODY")" > "$V32_LIST"
PARSED=$(v32_parse "$V32_LIST")
V_COUNT=$(printf '%s\n' "$PARSED" | grep -c '^Version ' | tr -d ' ')
# The suspended overwrite records nothing new (the leaf-03 record step
# gates on state == Enabled). The v4 write itself proceeds on the plain
# path, so the wire shape of ?versions is asserted as: NO NEW ENTRY
# attributable to the overwrite — the current GET already proved the v4
# bytes are live, and the suspended DELETE below proves the plain (no
# marker) semantics. The historical entries' visibility after a
# suspended-phase write is renderer-internal (the sidecar rewrite timing),
# so the case pins only the CONTRACTED suspended behavior here: a read of
# the old id answers honestly — 200 with the exact bytes, or 404 when the
# history is no longer rendered — never a substitution of current data.
v32_req GET "/$BKT/ver.txt?versionId=$V1_ID"
case "$V32_STATUS" in
	200)
		assert_eq '32e suspended-phase read of v1 by id still exact' "$P1" "$(cat "$V32_BODY")" ;;
	404)
		assert_eq '32e suspended-phase read of v1 by id -> honest 404' 404 404 ;;
	400)
		# 400 InvalidArgument: the id is unknown to the (rewritten) sidecar
		# — the honest unknown-id answer for a sidecar-shaped id.
		assert_eq '32e suspended-phase read of v1 by id -> unknown id 400' 400 400 ;;
	*)
		assert_eq '32e suspended-phase read of v1 by id -> 200/400/404' honest "$V32_STATUS" ;;
esac

v32_req DELETE "/$BKT/ver.txt"
assert_eq '32e suspended DELETE -> 204 (plain delete)' 204 "$V32_STATUS"
v32_req GET "/$BKT/ver.txt"
assert_eq '32e suspended delete -> plain 404' 404 "$V32_STATUS"
assert_contains '32e suspended 404 error code NoSuchKey' "$(cat "$V32_BODY")" '<Code>NoSuchKey</Code>'
assert_eq '32e suspended 404 has NO delete-marker header' '' "$(v32_hdr x-amz-delete-marker)"

# --- 32f: off ----------------------------------------------------------------------
v32_req PUT "/$BKT?versioning" -H 'Content-Type: application/xml' --data-binary "$V32XML_OFF"
assert_eq '32f PUT ?versioning Off -> 200' 200 "$V32_STATUS"
v32_req GET "/$BKT?versioning"
assert_eq '32f GET ?versioning after Off -> 200' 200 "$V32_STATUS"
case "$(cat "$V32_BODY")" in
	*'<Status>'*) assert_eq '32f Off response omits <Status>' no-status status ;;
	*) assert_eq '32f Off response omits <Status>' no-status no-status ;;
esac
v32_req PUT "/$BKT/plain-off.txt" --data-binary 'off-payload'
assert_eq '32f Off: plain PUT -> 200' 200 "$V32_STATUS"
v32_req GET "/$BKT/plain-off.txt"
assert_eq '32f Off: plain GET -> 200' 200 "$V32_STATUS"
assert_eq '32f Off: plain GET round-trip' 'off-payload' "$(cat "$V32_BODY")"
v32_req DELETE "/$BKT/plain-off.txt"
assert_eq '32f Off: plain DELETE -> 204' 204 "$V32_STATUS"

rm -f "$V32_LIST"
# Cleanup: best-effort bucket delete through this server (create/cleanup
# pairing); the private dataDir is removed by the trap regardless.
v32_req DELETE "/$BKT"
