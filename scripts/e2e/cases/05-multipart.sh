# 05-multipart.sh — multipart round-trip 3 parts, ETag -3 suffix, abort cleanup,
# ListMultipartUploads, ListParts, EntityTooSmall via undersized non-final part,
# in-flight multipart blocks bucket delete → 409.
# (Expired-upload sweep is unit-covered — not testable in real time; skipped.)
set -u
BKT='e2e-05-multipart'
aws s3api create-bucket --bucket "$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

TMPD=$(mktemp -d /tmp/e2e05.XXXXXX)

# --- 3-part multipart round trip ------------------------------------------
# Parts 1,2: 5MiB each (>= min); part 3 (final): small.
python3 - "$TMPD" <<'PY'
import os, sys, random
d = sys.argv[1]
rng = random.Random(42)
with open(os.path.join(d, 'p1'), 'wb') as f:
	f.write(bytes(rng.getrandbits(8) for _ in range(5*1024*1024)))
with open(os.path.join(d, 'p2'), 'wb') as f:
	f.write(bytes(rng.getrandbits(8) for _ in range(5*1024*1024)))
with open(os.path.join(d, 'p3'), 'wb') as f:
	f.write(b'final-part-tail')
PY
cat "$TMPD/p1" "$TMPD/p2" "$TMPD/p3" > "$TMPD/expected"

UID1=$(aws s3api create-multipart-upload --bucket "$BKT" --key 'assembled.bin' \
	--endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["UploadId"])')
assert_eq 'create-multipart-upload returned 32-hex id' 32 "${#UID1}"

E1=$(aws s3api upload-part --bucket "$BKT" --key 'assembled.bin' --upload-id "$UID1" \
	--part-number 1 --body "$TMPD/p1" --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["ETag"])')
E2=$(aws s3api upload-part --bucket "$BKT" --key 'assembled.bin' --upload-id "$UID1" \
	--part-number 2 --body "$TMPD/p2" --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["ETag"])')
E3=$(aws s3api upload-part --bucket "$BKT" --key 'assembled.bin' --upload-id "$UID1" \
	--part-number 3 --body "$TMPD/p3" --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["ETag"])')

# ListParts shows 3 parts
LP=$(aws s3api list-parts --bucket "$BKT" --key 'assembled.bin' --upload-id "$UID1" \
	--endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null)
N=$(printf '%s' "$LP" | python3 -c 'import json,sys;print(len(json.load(sys.stdin).get("Parts",[])))')
assert_eq 'ListParts shows 3 parts' 3 "$N"

# ListMultipartUploads shows the in-flight upload
LMU=$(aws s3api list-multipart-uploads --bucket "$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null)
assert_contains 'ListMultipartUploads shows upload id' "$LMU" "$UID1"

cat > "$TMPD/comp.json" <<EOF
{"Parts": [{"ETag": $E1, "PartNumber": 1}, {"ETag": $E2, "PartNumber": 2}, {"ETag": $E3, "PartNumber": 3}]}
EOF
CMP=$(aws s3api complete-multipart-upload --bucket "$BKT" --key 'assembled.bin' \
	--upload-id "$UID1" --multipart-upload "file://$TMPD/comp.json" \
	--endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null)
ETAG=$(printf '%s' "$CMP" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("ETag",""))')
assert_contains 'completed ETag has -3 suffix' "$ETAG" '-3"'

# byte-exact round trip
aws s3 cp "s3://$BKT/assembled.bin" "$TMPD/got" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
if cmp -s "$TMPD/expected" "$TMPD/got"; then
	assert_eq 'multipart assembly byte-exact' same same
else
	assert_eq 'multipart assembly byte-exact' same DIFFER
fi

# --- abort cleanup -----------------------------------------------------------
UID2=$(aws s3api create-multipart-upload --bucket "$BKT" --key 'aborted.bin' \
	--endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["UploadId"])')
aws_ok 'abort multipart upload' s3api abort-multipart-upload --bucket "$BKT" \
	--key 'aborted.bin' --upload-id "$UID2"
LMU2=$(aws s3api list-multipart-uploads --bucket "$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null)
assert_eq 'aborted upload gone from ListMultipartUploads' '0' "$(printf '%s' "$LMU2" | grep -c Upload || true)"

# --- in-flight multipart blocks bucket delete (409 BucketNotEmpty) -----------
UID3=$(aws s3api create-multipart-upload --bucket "$BKT" --key 'inflight.bin' \
	--endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["UploadId"])')
assert_status 'delete bucket with in-flight multipart → 409' 409 DELETE "/$BKT"
assert_s3code 'error code BucketNotEmpty (in-flight multipart)' 'BucketNotEmpty'
aws_ok 'cleanup: abort in-flight upload' s3api abort-multipart-upload \
	--bucket "$BKT" --key 'inflight.bin' --upload-id "$UID3"

# --- EntityTooSmall: non-final part below 5MiB -------------------------------
UID4=$(aws s3api create-multipart-upload --bucket "$BKT" --key 'small.bin' \
	--endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["UploadId"])')
printf 'tiny' > "$TMPD/tiny"
ST1=$(aws s3api upload-part --bucket "$BKT" --key 'small.bin' --upload-id "$UID4" \
	--part-number 1 --body "$TMPD/tiny" --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["ETag"])')
printf 'tail' > "$TMPD/tail"
ST2=$(aws s3api upload-part --bucket "$BKT" --key 'small.bin' --upload-id "$UID4" \
	--part-number 2 --body "$TMPD/tail" --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["ETag"])')
# complete via raw SigV4 POST with a real XML body (CompleteMultipartUpload is
# XML on the wire) so we can assert status + error code directly
cat > "$TMPD/comp2.xml" <<EOF
<CompleteMultipartUpload xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Part><ETag>$ST1</ETag><PartNumber>1</PartNumber></Part><Part><ETag>$ST2</ETag><PartNumber>2</PartNumber></Part></CompleteMultipartUpload>
EOF
s3req POST "/$BKT/small.bin?uploadId=$UID4" \
	--data-binary "@$TMPD/comp2.xml" \
	-H 'Content-Type: application/xml'
assert_eq 'complete with undersized non-final part → 400' 400 "$S3_STATUS"
assert_s3code 'error code EntityTooSmall' 'EntityTooSmall'
aws_ok 'cleanup: abort small upload' s3api abort-multipart-upload \
	--bucket "$BKT" --key 'small.bin' --upload-id "$UID4"

aws s3 rb "s3://$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl --force >/dev/null 2>&1
rm -rf "$TMPD"
