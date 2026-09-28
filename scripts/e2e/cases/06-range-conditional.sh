# 06-range-conditional.sh — --range bytes=0-99 (206 + length 100), suffix range,
# 416, If-Match success + 412, If-None-Match → 304 (curl — aws CLI hides it).
set -u
BKT='e2e-06-range'
aws s3api create-bucket --bucket "$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

# 120-byte object with known content
TMP=$(mktemp /tmp/e2e06.XXXXXX)
python3 -c "print(''.join(chr(97 + i % 26) for i in range(119)) + '\n', end='')" > "$TMP"
s3req PUT "/$BKT/r.bin" --data-binary @"$TMP" -H 'Content-Type: application/octet-stream'
assert_eq 'setup: put 120-byte object' 200 "$S3_STATUS"
s3req GET "/$BKT/r.bin"
ETAG=$(printf '%s' "$S3_BODY" | head -c 0; curl -sk --aws-sigv4 "aws:amz:us-east-1:s3" \
	--user "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" \
	-o /dev/null -w '%header{etag}' "$BASE_URL/$BKT/r.bin" 2>/dev/null)

# bytes=0-99 → 206 with exactly 100 bytes
s3req GET "/$BKT/r.bin" -H 'Range: bytes=0-99'
assert_eq 'range bytes=0-99 → 206' 206 "$S3_STATUS"
assert_eq 'range bytes=0-99 length=100' 100 "${#S3_BODY}"

# suffix range bytes=-10 → last 10 bytes (curl body written to file: no
# command-substitution newline stripping)
s3req GET "/$BKT/r.bin" -H 'Range: bytes=-10'
assert_eq 'suffix range → 206' 206 "$S3_STATUS"
RFILE=$(mktemp /tmp/e2e06.XXXXXX)
curl -sk --aws-sigv4 "aws:amz:us-east-1:s3" \
	--user "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" \
	-H 'Range: bytes=-10' -o "$RFILE" "$BASE_URL/$BKT/r.bin" 2>/dev/null
assert_eq 'suffix range length=10' 10 "$(wc -c < "$RFILE" | tr -d ' ')"
TAIL=$(tail -c 10 "$TMP")
assert_eq 'suffix range content matches object tail' "$TAIL" "$(cat "$RFILE")"
rm -f "$RFILE"

# out-of-bounds range → 416
s3req GET "/$BKT/r.bin" -H 'Range: bytes=500-600'
assert_eq 'range beyond EOF → 416' 416 "$S3_STATUS"
assert_s3code 'error code InvalidRange' 'InvalidRange'

# If-Match with correct ETag → 200
s3req GET "/$BKT/r.bin" -H "If-Match: $ETAG"
assert_eq 'If-Match correct ETag → 200' 200 "$S3_STATUS"

# If-Match with wrong ETag → 412 PreconditionFailed
s3req GET "/$BKT/r.bin" -H 'If-Match: "deadbeef"'
assert_eq 'If-Match wrong ETag → 412' 412 "$S3_STATUS"
assert_s3code 'error code PreconditionFailed' 'PreconditionFailed'

# If-None-Match with current ETag → 304 (body empty)
s3req GET "/$BKT/r.bin" -H "If-None-Match: $ETAG"
assert_eq 'If-None-Match current ETag → 304' 304 "$S3_STATUS"

# plain GET → 200 (ETag still valid)
s3req GET "/$BKT/r.bin"
assert_eq 'plain GET → 200' 200 "$S3_STATUS"

rm -f "$TMP"
aws s3 rb "s3://$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl --force >/dev/null 2>&1
