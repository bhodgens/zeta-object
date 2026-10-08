# 18-zmetad-events.sh — ?events served from a REAL zmetad SQLite fixture DB
# (zmetad-provider-2026-09 leaf 05; AGENTS.md e2e-coverage rule for the
# zmetad_db_path config surface and the provider behavior).
#
# e2e hosts have neither ZFS nor a zmetad daemon, so this case:
#   1. builds the canned v5 fixture DB with the Go builder in
#      scripts/e2e/fixtures/zmetad-fixture (graceful SKIP — pass +1 and
#      return 0, case-22 convention — when go run cannot build it);
#   2. maps the fixture's datasets-table mountpoint onto the case's
#      pre-created bucket dir (symlink-resolved: the provider EvalSymlinks
#      before resolution);
#   3. launches a PRIVATE server (case-14 pattern; the suite server stays
#      untouched) with zmetad_db_path in config.json and
#      ZETAOBJECT_ASSUME_ZFS=1 — the statfs-hint bypass that lets the real
#      provider chain (datasets + sync_state checks) attach on a ZFS-less
#      host. DetectZFS is a hint only; the DB is authoritative.
#
# Asserted on the wire (curl --aws-sigv4 via s3req):
#   - GET /{bkt}?events → 200, dataset=testpool/e2e, recordsLost=7
#     (knownLost ONLY), ringSwaps=1 (separate class, never folded)
#   - key-scoped ?events on a nested key → the PARTIAL create via the
#     conservative bare-name match (NULL full_path, ancestor absent)
#   - GET /{bkt}?events&versions → XML IsLossy=true RecordsLost=7
#     RingSwaps=1 (Contract 5)
#   - since-id cursor (gateway issue #15): page with max-events, resume
#     with since-id=<last id> EXACTLY (no duplicate, no gap — union of
#     pages = the full stream); every entry carries its monotonic id;
#     invalid since-id → 400 InvalidArgument; the webdav frontend's
#     bridge answers the SAME query byte-identically (universality rule)
#   - unsigned request → 403 (auth precedes dispatch)
#
# Purge is NOT exercised over HTTP: there is no purge endpoint (pre-existing
# gap, recorded in leaf 04's report). Provider-level Purge (zmetad
# --purge argv shape, error wrapping, never-at-probe) is unit-covered by
# TestZmetadProviderPurge in internal/metadata/zmetad_provider_test.go.
set -u
BKT='e2e-18-zmetad'
E18_WORK=$(mktemp -d /tmp/e2e18-zmetad.XXXXXX)
E18_CERT=$(mktemp -d /tmp/e2e18-zmetad-cert.XXXXXX)
E18_DATA="$E18_WORK/data"
E18_BKT_DIR="$E18_DATA/$BKT"
mkdir -p "$E18_BKT_DIR"

# The provider resolves bucket paths with EvalSymlinks before consulting
# the datasets table (macOS: /var -> /private/var), so the fixture must
# map the PHYSICAL path.
E18_BKT_PHYS=$(cd "$E18_BKT_DIR" && pwd -P)

e18_cleanup() {
	if [ -n "${E18_PID:-}" ] && kill -0 "$E18_PID" 2>/dev/null; then
		kill -TERM "$E18_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$E18_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$E18_PID" 2>/dev/null
	fi
	rm -rf "$E18_WORK" "$E18_CERT"
}
trap e18_cleanup EXIT

# --- fixture DB: graceful SKIP when go run cannot build it -------------------
E18_DB="$E18_WORK/zmetad.db"
# -mod=mod: cases 12/13 create vendor/ (boto3 venv, mc) mid-suite; Go then
# treats vendor/ as a vendor tree and dies on the missing modules.txt.
if ! go run -mod=mod ./scripts/e2e/fixtures/zmetad-fixture "$E18_DB" "$E18_BKT_PHYS" >"$E18_WORK/fixture.log" 2>&1; then
	echo '  (zmetad fixture builder could not run — skipping case; unit + wire-shape coverage in internal/metadata and internal/frontend/s3 carries the contract)'
	cat "$E18_WORK/fixture.log"
	E2E_PASS=$((E2E_PASS + 1))
	e2e_finish
	# return, NOT exit: this file is SOURCED (case-22 skip convention).
	return 0
fi
assert_eq 'fixture builder produced the v5 database' yes "$([ -s "$E18_DB" ] && echo yes || echo no)"

# --- private server with zmetad_db_path + the statfs-hint bypass -------------
E18_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
# A second listener for the webdav frontend (since-id parity across BOTH
# frontends against the same zmetad fixture).
E18_PORT_DAV=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
openssl req -x509 -newkey rsa:2048 -keyout "$E18_CERT/key.pem" -out "$E18_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
cat > "$E18_WORK/config.json" <<EOF
{
  "dataDir": "$E18_DATA",
  "listenAddr": "127.0.0.1:$E18_PORT",
  "certFile": "$E18_CERT/cert.pem",
  "keyFile": "$E18_CERT/key.pem",
  "zmetad_db_path": "$E18_DB",
  "zmetad_binary": "/nonexistent-zmetad-e2e",
  "frontends": [
    {"type": "s3"},
    {"type": "webdav", "listenAddr": "127.0.0.1:$E18_PORT_DAV"}
  ]
}
EOF
ZETAOBJECT_ASSUME_ZFS=1 ZETAOBJECT_CONFIG="$E18_WORK/config.json" \
	./zeta-object-server >>"$E2E_SERVER_LOG" 2>&1 &
E18_PID=$!
E18_ENDPOINT="https://127.0.0.1:$E18_PORT"
ENDPOINT="$E18_ENDPOINT"
BASE_URL="$ENDPOINT"
export E2E_ENDPOINT="$ENDPOINT"
if ! wait_for_port 127.0.0.1 "$E18_PORT" 15; then
	echo '  (zmetad-events server did not start — failing case)'
	E2E_FAIL=$((E2E_FAIL + 1))
	exit 0
fi
wait_for_port 127.0.0.1 "$E18_PORT_DAV" 15 || true

# The pre-created bucket dir is discovered as a bucket (dataDir layout).
aws_ok 'HeadBucket on the zmetad fixture bucket' s3api head-bucket --bucket "$BKT"

# --- GET /{bucket}?events: 200 + Contract 5 envelope with fixture values ----
s3req GET "/$BKT?events"
assert_eq 'bucket?events → 200 (zmetad provider attached)' 200 "$S3_STATUS"
assert_contains '?events envelope carries the fixture dataset' "$S3_BODY" '"dataset":"testpool/e2e"'
assert_contains '?events recordsLost=7 (knownLost only, never folded)' "$S3_BODY" '"recordsLost":7'
assert_contains '?events ringSwaps=1 (separate loss class)' "$S3_BODY" '"ringSwaps":1'
assert_contains '?events carries the recorded fixture rename row' "$S3_BODY" '"key":"edge/inner/renamed.txt"'
assert_contains '?events rename oldKey is the resolved old_full_path' "$S3_BODY" '"oldKey":"edge/inner/deep.txt"'

# --- key-scoped ?events: PARTIAL row via conservative bare-name match -------
# The canned nested create has NULL full_path (ancestor 999 absent from
# objmap): it MUST surface for the nested key as the bare name, never
# fabricated into a full path (SCHEMA.md section 7).
s3req GET "/$BKT/edge/inner/orphan.txt?events"
assert_eq 'key-scoped ?events on nested key → 200' 200 "$S3_STATUS"
assert_contains 'nested key matches the PARTIAL create (bare name)' "$S3_BODY" '"key":"orphan.txt"'
assert_contains 'PARTIAL create op is create' "$S3_BODY" '"op":"create"'
assert_contains 'key-scoped envelope keeps recordsLost=7' "$S3_BODY" '"recordsLost":7'

# --- since-id cursor (gateway issue #15): mid-stream resume, exact ----------
# The canned fixture has 11 events with monotonic ids 1..11. Fetch the
# first page (max-events=6), take the last id, resume with since-id=<id>
# and assert the resume is EXACT: no duplicate (id <= cursor never
# redelivers), no gap (the union of both pages is the full stream).
s3req GET "/$BKT?events&max-events=6"
assert_eq 'cursor page 1 (max-events=6) → 200' 200 "$S3_STATUS"
E18_P1="$S3_BODY"
E18_CURSOR=$(printf '%s' "$E18_P1" | python3 -c '
import json, sys
env = json.load(sys.stdin)
ids = [e["id"] for e in env["events"]]
assert len(ids) == 6, f"page 1 = {len(ids)} events, want 6 (max-events cap)"
assert ids == sorted(ids) and len(set(ids)) == 6, f"ids not strictly increasing: {ids}"
print(ids[-1])
')
assert_eq 'page 1 cursor is the 6th monotonic id' 6 "$E18_CURSOR"

s3req GET "/$BKT?events&since-id=$E18_CURSOR"
assert_eq 'cursor page 2 (since-id resume) → 200' 200 "$S3_STATUS"
E18_P2="$S3_BODY"
printf '%s' "$E18_P2" | python3 -c '
import json, sys
p1 = json.loads(sys.argv[1])
p2 = json.load(sys.stdin)
ids1 = [e["id"] for e in p1["events"]]
ids2 = [e["id"] for e in p2["events"]]
cursor = ids1[-1]
assert all(i > cursor for i in ids2), f"resume redelivered ids <= {cursor}: {ids2}"
union = sorted(ids1 + ids2)
assert union == list(range(1, 12)), f"union of pages != full stream 1..11: {union}"
print("cursor resume exact: page1 ids", ids1, "+ page2 ids", ids2, "= 1..11, no duplicate no gap")
' "$E18_P1"

# Every events entry carries its cursor id (additive wire field); the
# resume from id 10 is EXACTLY the one remaining event.
s3req GET "/$BKT?events&since-id=10"
assert_eq 'since-id=10 → 200' 200 "$S3_STATUS"
assert_contains 'resume from id 10 = exactly ids 11' "$S3_BODY" '"id":11'
assert_eq 'resume from id 10 is a single event' 1 "$(printf '%s' "$S3_BODY" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["events"]))')"

# Invalid cursor: 400 InvalidArgument, fail loudly (a silently-ignored
# cursor would skip or redeliver events on reconnect).
s3req GET "/$BKT?events&since-id=-1"
assert_eq 'since-id=-1 → 400 InvalidArgument' 400 "$S3_STATUS"
assert_contains 'since-id=-1 error code InvalidArgument' "$S3_BODY" 'InvalidArgument'
s3req GET "/$BKT?events&since-id=abc"
assert_eq 'since-id=abc → 400 InvalidArgument' 400 "$S3_STATUS"
assert_contains 'since-id=abc error code InvalidArgument' "$S3_BODY" 'InvalidArgument'

# webdav parity: the SAME since-id pass-through through the webdav
# frontend's bridge entry (mode A: <bucket> collection ?events), Basic
# auth. Byte-parity with the s3 wire: the body must be IDENTICAL to the
# s3 response for the same query (one pipeline, both frontends).
s3req GET "/$BKT?events&since-id=5"
assert_eq 's3 since-id=5 baseline → 200' 200 "$S3_STATUS"
E18_S3_CURSOR_BODY="$S3_BODY"
W18_DAV_STATUS=$(curl -sk -o "$E18_WORK/dav-body.json" -w '%{http_code}' \
	--user 'zetaadmin:zetaadmin' \
	"https://127.0.0.1:$E18_PORT_DAV/$BKT?events&since-id=5" 2>/dev/null)
assert_eq 'webdav collection?events&since-id → 200' 200 "$W18_DAV_STATUS"
assert_eq 'webdav since-id body byte-identical to the s3 wire' "$E18_S3_CURSOR_BODY" "$(cat "$E18_WORK/dav-body.json")"

# webdav invalid cursor: the SAME 400 InvalidArgument.
W18_DAV_STATUS=$(curl -sk -o "$E18_WORK/dav-bad.json" -w '%{http_code}' \
	--user 'zetaadmin:zetaadmin' \
	"https://127.0.0.1:$E18_PORT_DAV/$BKT?events&since-id=-1" 2>/dev/null)
assert_eq 'webdav since-id=-1 → 400 InvalidArgument' 400 "$W18_DAV_STATUS"
assert_contains 'webdav invalid cursor error code InvalidArgument' "$(cat "$E18_WORK/dav-bad.json")" 'InvalidArgument'

# --- GET /{bucket}?events&versions: XML loss flags (Contract 5) -------------
s3req GET "/$BKT?events&versions"
assert_eq '?events&versions → 200' 200 "$S3_STATUS"
assert_contains 'versions XML is the ListObjectVersionsExt extension' "$S3_BODY" '<ListObjectVersionsExt'
assert_contains 'versions XML IsLossy=true (recordsLost>0 OR ringSwaps>0)' "$S3_BODY" '<IsLossy>true</IsLossy>'
assert_contains 'versions XML RecordsLost=7' "$S3_BODY" '<RecordsLost>7</RecordsLost>'
assert_contains 'versions XML RingSwaps=1 (never folded)' "$S3_BODY" '<RingSwaps>1</RingSwaps>'

# --- auth precedence: unsigned request → 403 --------------------------------
E18_UNSIGNED=$(curl -sk -o /dev/null -w '%{http_code}' "$BASE_URL/$BKT?events" 2>/dev/null)
assert_eq 'unsigned bucket?events → 403' 403 "$E18_UNSIGNED"

# --- server health: still serving after all events traffic ------------------
s3req GET "/$BKT?events"
assert_eq 'bucket?events still 200 after the full assertion pass' 200 "$S3_STATUS"

# --- cleanup pairing: stop the private server, wipe the case dirs -----------
kill -TERM "$E18_PID" 2>/dev/null
for _ in 1 2 3 4 5 6 7 8 9 10; do
	kill -0 "$E18_PID" 2>/dev/null || break
	sleep 0.5
done
kill -9 "$E18_PID" 2>/dev/null
wait "$E18_PID" 2>/dev/null
E18_PID=''
rm -rf "$E18_BKT_DIR"
assert_eq 'case bucket dir removed (create/cleanup pairing)' no "$([ -e "$E18_BKT_DIR" ] && echo yes || echo no)"
