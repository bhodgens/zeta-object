# 07-presigned.sh — aws s3 presign → curl (200, correct bytes), expired URL → 403
# (--expires-in 1 + sleep 2), tampered URL → 403.
set -u
BKT='e2e-07-presign'
aws s3api create-bucket --bucket "$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

TMP=$(mktemp /tmp/e2e07.XXXXXX)
printf 'presign-me-07\n' > "$TMP"
aws s3 cp "$TMP" "s3://$BKT/ps.txt" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

# valid presigned URL → 200 + byte-exact
PURL=$(aws s3 presign "s3://$BKT/ps.txt" --expires-in 300 --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | head -1)
assert_contains 'presign produced https URL' "$PURL" 'https://'
CODE=$(curl -ks -o "$TMP.out" -w '%{http_code}' "$PURL" 2>/dev/null)
assert_eq 'presigned GET → 200' 200 "$CODE"
assert_eq 'presigned GET byte-exact' "$(cat "$TMP")" "$(cat "$TMP.out")"

# tampered signature → 403
TAMPERED="${PURL%X-Amz-Signature=*}X-Amz-Signature=0000000000000000000000000000000000000000000000000000000000000000"
CODE=$(curl -ks -o /dev/null -w '%{http_code}' "$TAMPERED" 2>/dev/null)
assert_eq 'tampered signature → 403' 403 "$CODE"

# expired URL → 403 (expires-in 1, sleep 2)
EPURL=$(aws s3 presign "s3://$BKT/ps.txt" --expires-in 1 --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | head -1)
sleep 2
CODE=$(curl -ks -o /dev/null -w '%{http_code}' "$EPURL" 2>/dev/null)
assert_eq 'expired presigned URL → 403' 403 "$CODE"

rm -f "$TMP" "$TMP.out"
aws s3 rb "s3://$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl --force >/dev/null 2>&1
