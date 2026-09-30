# 03-objects-errors.sh — NoSuchBucket/NoSuchKey codes, delete missing bucket 404,
# keys with spaces + encoding-type=url listing.
set -u
BKT='e2e-03-errors'
aws s3api create-bucket --bucket "$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
TMP=$(mktemp /tmp/e2e03.XXXXXX)
printf 'err-payload\n' > "$TMP"

# NoSuchBucket on GET into a bucket that never existed
s3req GET '/e2e-no-such-bucket-xyz/k'
assert_eq 'get into missing bucket → 404' 404 "$S3_STATUS"
assert_s3code 'error code NoSuchBucket (GET)' 'NoSuchBucket'

# NoSuchKey on missing object
s3req GET "/$BKT/does-not-exist"
assert_eq 'get missing key → 404' 404 "$S3_STATUS"
assert_s3code 'error code NoSuchKey' 'NoSuchKey'

# delete object in missing bucket → 404 NoSuchBucket
s3req DELETE '/e2e-no-such-bucket-xyz/k'
assert_eq 'delete in missing bucket → 404' 404 "$S3_STATUS"
assert_s3code 'error code NoSuchBucket (DELETE)' 'NoSuchBucket'

# keys with spaces + encoding-type=url listing
aws_ok 'put key with spaces' s3 cp "$TMP" "s3://$BKT/hello world file.txt"
LIST=$(aws s3api list-objects-v2 --bucket "$BKT" --encoding-type url \
	--endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null)
# NOTE: the server emits QueryEscape encoding (space → '+'), real S3 emits
# '%20' — documented divergence; assert the encoding is applied either way.
if printf '%s' "$LIST" | grep -qE 'hello(\+|%20)world(\+|%20)file\.txt'; then
	assert_eq 'url-encoded listing encodes space key' 0 0
else
	assert_eq 'url-encoded listing encodes space key' 0 1
fi
LIST2=$(aws s3api list-objects-v2 --bucket "$BKT" \
	--endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null)
assert_contains 'plain listing shows raw space key' "$LIST2" 'hello world file.txt'

# delete the spaced key over the wire with %20 encoding (raw SigV4 request —
# the CLI re-encodes arguments itself, so it is not a clean encoder probe)
s3req DELETE "/$BKT/hello%20world%20file.txt"
assert_eq 'delete url-encoded spaced key → 204' 204 "$S3_STATUS"

# H1: the bare key "." aliases the bucket directory itself. The backend
# seam rejects it (InvalidArgument), and on the wire the shared mux
# normalizes the "/." segment away (curl without --path-as-is sends
# "/$BKT", i.e. a bucket-level request — never a key write). Pin the
# WIRE-VISIBLE consequence: a dot-key PUT must NEVER materialize data at
# the bucket path — the bucket must still be a working, listable bucket
# with no stray "." object afterward (the bug made it vanish from
# listings entirely).
aws_ok 'dot-key put does not brick the bucket' s3api list-objects-v2 \
	--bucket "$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl
S3_STATUS=$(curl -sk -o /tmp/e2e03-dotbody -w '%{http_code}' \
	--aws-sigv4 "aws:amz:us-east-1:s3" \
	--user "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" \
	-X PUT "$BASE_URL/$BKT/." --data-binary 'brick' 2>/dev/null)
assert_eq 'dot-key put never writes an object → bucket-level response' 409 "$S3_STATUS"
LIST4=$(aws s3api list-objects-v2 --bucket "$BKT" \
	--endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null)
assert_eq 'no stray dot object after dot-key put' 0 \
	"$(printf '%s' "$LIST4" | grep -c 'Key' || true)"
rm -f /tmp/e2e03-dotbody
LIST3=$(aws s3api list-objects-v2 --bucket "$BKT" \
	--endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null)
assert_eq 'spaced key gone after encoded delete' 0 "$(printf '%s' "$LIST3" | grep -c 'hello' || true)"

rm -f "$TMP"
aws s3 rb "s3://$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl --force >/dev/null 2>&1
