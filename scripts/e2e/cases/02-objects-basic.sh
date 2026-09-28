# 02-objects-basic.sh — put/get/head/delete round-trips, nested keys, unicode
# keys, default content-type binary/octet-stream, x-amz-meta propagation.
set -u
BKT='e2e-02-objects'
aws s3api create-bucket --bucket "$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

TMP=$(mktemp /tmp/e2e02.XXXXXX)
printf 'round-trip-payload-02\n' > "$TMP"

# simple round trip via aws CLI
aws_ok 'put object' s3 cp "$TMP" "s3://$BKT/plain.txt"
aws s3 cp "s3://$BKT/plain.txt" "$TMP.out" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
assert_eq 'get round-trip byte-exact' "$(cat "$TMP")" "$(cat "$TMP.out")"

# nested key
aws_ok 'put nested key' s3 cp "$TMP" "s3://$BKT/a/b/c/nested.txt"
aws s3 cp "s3://$BKT/a/b/c/nested.txt" "$TMP.nest" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
assert_eq 'nested key round-trip' "$(cat "$TMP")" "$(cat "$TMP.nest")"

# unicode key
UNI=$'kéy-ünïcode-✓.txt'
aws_ok 'put unicode key' s3 cp "$TMP" "s3://$BKT/$UNI"
aws_ok 'get unicode key' s3 cp "s3://$BKT/$UNI" "$TMP.uni"
assert_eq 'unicode round-trip' "$(cat "$TMP")" "$(cat "$TMP.uni")"

# head: default content-type + explicit content-type + metadata
aws_ok 'head object' s3api head-object --bucket "$BKT" --key 'plain.txt'
CT=$(aws s3api head-object --bucket "$BKT" --key 'plain.txt' --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["ContentType"])')
assert_eq 'default content-type binary/octet-stream' 'binary/octet-stream' "$CT"

aws_ok 'put with content-type text/x-custom' \
	s3 cp "$TMP" "s3://$BKT/typed.txt" --content-type 'text/x-custom'
CT2=$(aws s3api head-object --bucket "$BKT" --key 'typed.txt' --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["ContentType"])')
assert_eq 'explicit content-type preserved' 'text/x-custom' "$CT2"

aws_ok 'put with metadata' \
	s3 cp "$TMP" "s3://$BKT/meta.txt" --metadata 'owner=e2e,team=harden'
# NOTE: header canonicalization capitalizes metadata keys (Owner/Team) — a
# documented server divergence from real S3 (which preserves client case);
# assert value propagation case-insensitively on the key.
META=$(aws s3api head-object --bucket "$BKT" --key 'meta.txt' --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;m=json.load(sys.stdin).get("Metadata",{});print(next((v for k,v in m.items() if k.lower()=="owner"),""))')
assert_eq 'x-amz-meta propagation' 'e2e' "$META"

# delete round-trip (status via raw SigV4 request)
s3req DELETE "/$BKT/plain.txt"
assert_eq 'delete object → 204' 204 "$S3_STATUS"
s3req GET "/$BKT/plain.txt"
assert_eq 'get deleted key → 404' 404 "$S3_STATUS"
assert_s3code 'error code NoSuchKey' 'NoSuchKey'

rm -f "$TMP" "$TMP.out" "$TMP.nest" "$TMP.uni"
aws s3 rb "s3://$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl --force >/dev/null 2>&1
