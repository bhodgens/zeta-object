# 17-events-endpoints.sh — metadata capability endpoints (metadata-zfs leaf 04)
# through the REAL SigV4-signed wire path (curl --aws-sigv4):
#   GET /{bucket}/{key}?events   → JSON ObjectEventHistory (key-scoped)
#   GET /{bucket}?events         → JSON ObjectEventHistory (bucket summary)
#   GET /{bucket}?events&versions→ XML ListObjectVersionsExt extension —
#                                  MUST route to the events extension, never
#                                  the plain ?versions listing (dispatch
#                                  order pin).
# Contract on a ZFS-less host (this one): no provider attaches, so ?events
# answers the contracted 503 with error code NotImplemented. Also pinned:
#   - ?events on a MISSING bucket → 404 NoSuchBucket (404 precedence over
#     provider resolution)
#   - unsigned request → 403 (auth runs before any dispatch)
#   - plain GET ?versions still 200s (unversioned listing untouched)
set -u
BKT='e2e-17-events'
MISSING='e2e-17-events-missing'
aws s3api create-bucket --bucket "$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
# One object so the plain ?versions listing has content to contrast against.
printf 'case17-events-body' | aws s3 cp - "s3://$BKT/obj.txt" \
	--endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

# --- GET /{bucket}/{key}?events: 503 NotImplemented (no provider on this host)
s3req GET "/$BKT/obj.txt?events"
assert_eq 'GET key?events → 503 on ZFS-less bucket' 503 "$S3_STATUS"
assert_s3code 'key?events error code NotImplemented' 'NotImplemented'
assert_contains 'key?events error is S3 XML' "$S3_BODY" '<Error>'

# --- GET /{bucket}?events: same contract, bucket summary --------------------
s3req GET "/$BKT?events"
assert_eq 'GET bucket?events → 503 on ZFS-less bucket' 503 "$S3_STATUS"
assert_s3code 'bucket?events error code NotImplemented' 'NotImplemented'
assert_contains 'bucket?events error is S3 XML' "$S3_BODY" '<Error>'

# --- 404 precedence: ?events on a missing bucket ----------------------------
s3req GET "/$MISSING?events"
assert_eq 'bucket?events on missing bucket → 404' 404 "$S3_STATUS"
assert_s3code 'missing-bucket events error code NoSuchBucket' 'NoSuchBucket'
s3req GET "/$MISSING/some-key?events"
assert_eq 'key?events on missing bucket → 404' 404 "$S3_STATUS"
assert_s3code 'missing-bucket key events error code NoSuchBucket' 'NoSuchBucket'

# --- auth precedence: unsigned request → 403 -------------------------------
UNSIGNED_STATUS=$(curl -sk -o /dev/null -w '%{http_code}' "$BASE_URL/$BKT?events" 2>/dev/null)
assert_eq 'unsigned bucket?events → 403' 403 "$UNSIGNED_STATUS"
UNSIGNED_STATUS=$(curl -sk -o /dev/null -w '%{http_code}' "$BASE_URL/$BKT/obj.txt?events" 2>/dev/null)
assert_eq 'unsigned key?events → 403' 403 "$UNSIGNED_STATUS"

# --- ?events&versions routes to the extension, NOT the plain versions ------
s3req GET "/$BKT?events&versions"
assert_eq 'GET bucket?events&versions → 503 (events extension, not versions)' 503 "$S3_STATUS"
assert_s3code 'events&versions error code NotImplemented' 'NotImplemented'
case "$S3_BODY" in
*ListVersionsResult*) assert_eq 'events&versions does NOT return plain versions XML' no_versions_xml PRESENT ;;
*) assert_eq 'events&versions does NOT return plain versions XML' no_versions_xml no_versions_xml ;;
esac

# --- plain GET ?versions still 200s (dispatch-order regression pin) --------
s3req GET "/$BKT?versions"
assert_eq 'plain GET bucket?versions → 200' 200 "$S3_STATUS"
assert_contains 'plain versions listing is ListVersionsResult XML' "$S3_BODY" '<ListVersionsResult'
assert_contains 'plain versions lists the object' "$S3_BODY" '<Key>obj.txt</Key>'

aws s3 rb "s3://$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl --force >/dev/null 2>&1
