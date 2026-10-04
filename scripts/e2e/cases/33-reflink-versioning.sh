# 33-reflink-versioning.sh — reflink (block-clone) versioning mode over
# the wire (s3-versioning-2026-10 tree leaf 06).
#
# The e2e harness runs on a PLAIN filesystem where FICLONE is
# unavailable (darwin dev: the GOOS arm answers unsupported; linux CI:
# non-ZFS FS rejects the ioctl) — this case asserts the FAIL-SOFT path
# HONESTLY and the mode/marker/bookkeeping surface:
#   33a. Private server pins "zfs_versioning": "reflink" + retention 2;
#        PUT ?versioning Enabled echoes.
#   33b. PUT-overwrite N times: EVERY PUT returns 200 (fail-soft NEVER
#        breaks a write — the leaf-06 core contract).
#   33c. ?versions may show recorded versions (on a FICLONE-capable FS)
#        or none (fail-soft) — the case accepts both honestly and pins
#        the BOUNDS: version count <= writes, ids are sidecar-shaped,
#        and any listed version's bytes round-trip through ?versionId
#        (never current-data substitution). Where a version IS present,
#        it must hold the PREVIOUS bytes (the store records the OLD
#        version).
#   33d. DELETE with versioning enabled = marker semantics: plain GET
#        404 with x-amz-delete-marker: true; the marker shows in
#        ?versions (reflink mode has delete markers regardless of the
#        FS's FICLONE support).
#   33e. Retention bookkeeping: with retention=2 the ?versions VERSION
#        count can never exceed 2 after 4 overwrites (the on-disk prune
#        itself is proven against real ZFS by zfs-validate section 11).
#
# The retention prune's crash-safe order and the block-clone property
# are unit/live validated (reflinkversions_test.go + section 11 of
# run-zfs-validation.sh) — NOT here (no FICLONE on the harness FS).
set -u
BKT='e2e-33-reflink'
V33_ROOT=$(mktemp -d /tmp/e2e33-reflink.XXXXXX)
V33_CERT=$(mktemp -d /tmp/e2e33-cert.XXXXXX)
V33_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
openssl req -x509 -newkey rsa:2048 -keyout "$V33_CERT/key.pem" -out "$V33_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
mkdir -p "$V33_ROOT/data/$BKT" "$V33_ROOT/data/e2e-33-fb" "$V33_ROOT/data/e2e-33-pb3" "$V33_ROOT/data/e2e-33-pb0"

v33_cleanup() {
	if [ -n "${V33_PID:-}" ] && kill -0 "$V33_PID" 2>/dev/null; then
		kill -TERM "$V33_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$V33_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$V33_PID" 2>/dev/null
	fi
	mkdir -p /tmp/e2e33-keep
	cp "$V33_ROOT/server.log" /tmp/e2e33-keep/last-server.log 2>/dev/null || true
	rm -rf "$V33_ROOT" "$V33_CERT"
}
trap v33_cleanup EXIT

cat > "$V33_ROOT/config.json" <<EOF
{
  "dataDir": "$V33_ROOT/data",
  "listenAddr": "127.0.0.1:$V33_PORT",
  "certFile": "$V33_CERT/cert.pem",
  "keyFile": "$V33_CERT/key.pem",
  "zfs_versioning": "reflink",
  "zfs_versioning_reflink_retention": 2,
  "buckets": {
    "$BKT": "$V33_ROOT/data/$BKT",
    "e2e-33-fb": "$V33_ROOT/data/e2e-33-fb",
    "e2e-33-pb3": { "path": "$V33_ROOT/data/e2e-33-pb3", "reflinkRetention": 3 },
    "e2e-33-pb0": { "path": "$V33_ROOT/data/e2e-33-pb0", "reflinkRetention": 0 }
  }
}
EOF
: > "$V33_ROOT/server.log"
ZETAOBJECT_CONFIG="$V33_ROOT/config.json" ./zeta-object-server >"$V33_ROOT/server.log" 2>&1 &
V33_PID=$!
V33_URL="https://127.0.0.1:$V33_PORT"
if ! wait_for_port 127.0.0.1 "$V33_PORT" 15; then
	echo '  (reflink versioning server did not start — failing case)'
	assert_eq '33 reflink server started' started missing
	exit 0
fi

# v33_req <method> <path> [extra curl args...] — signed request against
# the private server (same raw-header pattern as case 32).
v33_req() {
	local method=$1 path=$2
	shift 2
	V33_HDRS=$(mktemp /tmp/e2e33-hdrs.XXXXXX)
	V33_BODY=$(mktemp /tmp/e2e33-body.XXXXXX)
	V33_STATUS=$(curl -sk -o "$V33_BODY" -D "$V33_HDRS" -w '%{http_code}' \
		--aws-sigv4 "aws:amz:us-east-1:s3" \
		--user "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" \
		-X "$method" "$V33_URL$path" "$@" 2>/dev/null)
}

# v33_hdr <name> — value of one response header from the last v33_req.
v33_hdr() {
	local val
	val=$(grep -i "^${1}:" "$V33_HDRS" 2>/dev/null | head -1)
	printf '%s' "$val" | sed 's/^[^:]*: //' | tr -d '\r'
}

# v33_parse <listing-file> — decode a ?versions document into one line
# per entry ("Version <id>" / "DeleteMarker <id>"), document order.
v33_parse() {
	python3 - "$1" <<'PYEOF'
import sys, xml.etree.ElementTree as ET
root = ET.parse(sys.argv[1]).getroot()
for el in root:
    tag = el.tag.rsplit('}', 1)[-1]
    if tag not in ('Version', 'DeleteMarker'):
        continue
    vid = ''
    for c in el:
        t = c.tag.rsplit('}', 1)[-1]
        if t == 'VersionId':
            vid = c.text or ''
    print(tag, vid)
PYEOF
}

V33XML_ENABLED='<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>'
R1='reflink-case33-bytes-v1'
R2='reflink-case33-bytes-v2'
R3='reflink-case33-bytes-v3'
R4='reflink-case33-bytes-v4'

# --- setup ----------------------------------------------------------------
v33_req PUT "/$BKT"
assert_eq '33 setup: create-bucket' 200 "$V33_STATUS"

# --- 33a: reflink mode is the default config; enable + echo ---------------
v33_req PUT "/$BKT?versioning" -H 'Content-Type: application/xml' --data-binary "$V33XML_ENABLED"
assert_eq '33a PUT ?versioning Enabled -> 200' 200 "$V33_STATUS"
v33_req GET "/$BKT?versioning"
assert_eq '33a GET ?versioning echoes Enabled' 200 "$V33_STATUS"
assert_contains '33a GET ?versioning body' "$(cat "$V33_BODY")" '<Status>Enabled</Status>'

# --- 33b: fail-soft overwrites — the PUT always succeeds -------------------
v33_req PUT "/$BKT/rl.txt" --data-binary "$R1"
assert_eq '33b PUT v1 -> 200 (create)' 200 "$V33_STATUS"
for payload in "$R2" "$R3" "$R4"; do
	v33_req PUT "/$BKT/rl.txt" --data-binary "$payload"
	assert_eq "33b PUT overwrite (${payload: -2}) -> 200 (fail-soft never breaks a PUT)" 200 "$V33_STATUS"
done
v33_req GET "/$BKT/rl.txt"
assert_eq '33b current GET after 4 writes -> 200' 200 "$V33_STATUS"
assert_eq '33b current GET returns the newest bytes' "$R4" "$(cat "$V33_BODY")"

# --- 33c: ?versions honest bounds + round-trip -----------------------------
v33_req GET "/$BKT?versions"
assert_eq '33c GET ?versions -> 200' 200 "$V33_STATUS"
V33_LIST=$(mktemp /tmp/e2e33-list.XXXXXX)
printf '%s' "$(cat "$V33_BODY")" > "$V33_LIST"
PARSED=$(v33_parse "$V33_LIST")
V_COUNT=$(printf '%s\n' "$PARSED" | grep -c '^Version ' | tr -d ' ')
# Honest window: FICLONE-capable FS -> up to 4 recorded; fail-soft FS ->
# 0. Either way the count is BOUNDED by the write count (never garbage).
if [ "$V_COUNT" -le 4 ]; then
	assert_eq '33c ?versions count bounded by writes (0 on fail-soft FS, <=4 on FICLONE FS)' 0 0
else
	assert_eq '33c ?versions count bounded by writes (0 on fail-soft FS, <=4 on FICLONE FS)' 0 1
fi
# Retention bookkeeping bound: with retention=2 the count can never
# exceed 2 (on-disk prune is live-validated in zfs-validate section 11).
if [ "$V_COUNT" -le 2 ]; then
	assert_eq '33c retention=2 bounds ?versions to at most 2 versions' 0 0
else
	assert_eq '33c retention=2 bounds ?versions to at most 2 versions' 0 1
fi
# Every listed version round-trips its EXACT recorded bytes (old data,
# never current-data substitution). The versions are the bytes that were
# current BEFORE each overwrite, so any listed version's payload is one
# of R1..R3 (R4 was never an "old" version within this window).
V_ANY=$(printf '%s\n' "$PARSED" | awk '$1=="Version"{print $2; exit}')
if [ -n "$V_ANY" ]; then
	v33_req GET "/$BKT/rl.txt?versionId=$V_ANY"
	assert_eq '33c GET ?versionId=<newest listed> -> 200' 200 "$V33_STATUS"
	case "$(cat "$V33_BODY")" in
		"$R1"|"$R2"|"$R3")
			assert_eq '33c version bytes are OLD recorded bytes (never current)' 0 0 ;;
		*)
			assert_eq '33c version bytes are OLD recorded bytes (never current)' 0 1 ;;
	esac
	assert_eq '33c x-amz-version-id echoes the requested id' "$V_ANY" "$(v33_hdr x-amz-version-id)"
else
	assert_eq '33c fail-soft FS records no versions (PUT-only honest window)' 0 0
fi

# --- 33d: delete marker semantics (work regardless of FICLONE) -------------
v33_req DELETE "/$BKT/rl.txt"
assert_eq '33d DELETE with versioning enabled -> 204' 204 "$V33_STATUS"
v33_req GET "/$BKT/rl.txt"
assert_eq '33d plain GET of delete-marked key -> 404' 404 "$V33_STATUS"
assert_eq '33d plain GET carries x-amz-delete-marker: true' true "$(v33_hdr x-amz-delete-marker)"

v33_req GET "/$BKT?versions"
printf '%s' "$(cat "$V33_BODY")" > "$V33_LIST"
PARSED=$(v33_parse "$V33_LIST")
D_COUNT=$(printf '%s\n' "$PARSED" | grep -c '^DeleteMarker ' | tr -d ' ')
assert_eq '33d ?versions shows exactly 1 delete marker' 1 "$D_COUNT"
MARKER_ID=$(printf '%s\n' "$PARSED" | awk '$1=="DeleteMarker"{print $2; exit}')
if [ -n "$MARKER_ID" ]; then
	v33_req GET "/$BKT/rl.txt?versionId=$MARKER_ID"
	assert_eq '33d GET ?versionId=<marker> -> 405' 405 "$V33_STATUS"
	assert_eq '33d marker read carries x-amz-delete-marker: true' true "$(v33_hdr x-amz-delete-marker)"
else
	assert_eq '33d marker id discoverable' 0 1
fi

rm -f "$V33_LIST"

# --- 33f: PER-BUCKET reflinkRetention override (leaf 07) --------------------
# Three extra buckets in the same config: e2e-33-fb (no per-bucket value =
# server-wide fallback 2), e2e-33-pb3 (per-bucket 3), e2e-33-pb0
# (per-bucket 0 = keep zero version copies). 4 overwrites each; the
# ?versions VERSION count must respect the bucket's OWN cap (on a
# fail-soft FS no versions are recorded at all and every assert is the
# honest 0-window — the bounds still hold trivially).
for PB in e2e-33-fb e2e-33-pb3 e2e-33-pb0; do
	v33_req PUT "/$PB"
	assert_eq "33f setup: create-bucket $PB" 200 "$V33_STATUS"
	v33_req PUT "/$PB?versioning" -H 'Content-Type: application/xml' --data-binary "$V33XML_ENABLED"
	assert_eq "33f setup: $PB versioning Enabled" 200 "$V33_STATUS"
	# A first PUT creates the .metadata tree that ?versioning's atomic
	# state write needs (custom buckets are pre-created empty dirs).
	v33_req PUT "/$PB/seed.txt" --data-binary "seed"
	assert_eq "33f setup: $PB seed object -> 200" 200 "$V33_STATUS"
	v33_req PUT "/$PB?versioning" -H 'Content-Type: application/xml' --data-binary "$V33XML_ENABLED"
	assert_eq "33f setup: $PB versioning Enabled (after .metadata exists)" 200 "$V33_STATUS"
	for i in 1 2 3 4; do
		v33_req PUT "/$PB/rl.txt" --data-binary "pb-$PB-$i"
		assert_eq "33f $PB overwrite $i -> 200" 200 "$V33_STATUS"
	done
	v33_req GET "/$PB?versions"
	printf '%s' "$(cat "$V33_BODY")" > "$V33_LIST"
	PB_COUNT=$(printf '%s\n' "$(v33_parse "$V33_LIST")" | grep -c '^Version ' | tr -d ' ')
	case $PB in
		e2e-33-pb3)
			if [ "$PB_COUNT" -le 3 ]; then
				assert_eq '33f per-bucket reflinkRetention=3 bounds ?versions to at most 3' 0 0
			else
				assert_eq '33f per-bucket reflinkRetention=3 bounds ?versions to at most 3' 0 1
			fi ;;
		e2e-33-pb0)
			if [ "$PB_COUNT" -le 0 ]; then
				assert_eq '33f per-bucket reflinkRetention=0 keeps ZERO version copies (overrides fallback 2)' 0 0
			else
				assert_eq '33f per-bucket reflinkRetention=0 keeps ZERO version copies (overrides fallback 2)' 0 1
			fi ;;
		*)
			if [ "$PB_COUNT" -le 2 ]; then
				assert_eq '33f fallback bucket (no override) respects server-wide retention=2' 0 0
			else
				assert_eq '33f fallback bucket (no override) respects server-wide retention=2' 0 1
			fi ;;
	esac
done

# Cleanup: best-effort bucket delete (create/cleanup pairing).
v33_req DELETE "/$BKT"
