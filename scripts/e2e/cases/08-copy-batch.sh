# 08-copy-batch.sh — s3api copy-object, metadata directive REPLACE,
# delete-objects batch of 3 (2 exist, 1 missing) → Deleted count 3.
set -u
BA='e2e-08-copy-a'
BB='e2e-08-copy-b'
aws s3api create-bucket --bucket "$BA" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
aws s3api create-bucket --bucket "$BB" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

TMP=$(mktemp /tmp/e2e08.XXXXXX)
printf 'copy-payload-08\n' > "$TMP"
aws s3 cp "$TMP" "s3://$BA/src.txt" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
aws s3 cp "$TMP" "s3://$BA/src2.txt" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

# plain copy-object across buckets
aws_ok 'copy-object cross-bucket' s3api copy-object --bucket "$BB" --key 'dst.txt' \
	--copy-source "$BA/src.txt"
aws s3 cp "s3://$BB/dst.txt" "$TMP.out" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
assert_eq 'copy round-trip byte-exact' "$(cat "$TMP")" "$(cat "$TMP.out")"

# copy with metadata REPLACE — CLI success path, verify via head-object
aws_ok 'copy-object with REPLACE metadata' s3api copy-object --bucket "$BB" \
	--key 'dst-meta.txt' --copy-source "$BA/src.txt" \
	--metadata 'copied=yes' --metadata-directive REPLACE
M=$(aws s3api head-object --bucket "$BB" --key 'dst-meta.txt' \
	--endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null \
	| python3 -c 'import json,sys;m=json.load(sys.stdin).get("Metadata",{});print(next((v for k,v in m.items() if k.lower()=="copied"),""))')
assert_eq 'REPLACE metadata propagated' 'yes' "$M"

# batch delete: 2 exist (src.txt, src2.txt) + 1 missing (ghost.txt) → Deleted=3
cat > "$TMP.del" <<EOF
{"Objects": [{"Key": "src.txt"}, {"Key": "src2.txt"}, {"Key": "ghost.txt"}], "Quiet": false}
EOF
DELJ=$(aws s3api delete-objects --bucket "$BA" --delete "file://$TMP.del" \
	--endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null)
DC=$(printf '%s' "$DELJ" | python3 -c 'import json,sys;print(len(json.load(sys.stdin).get("Deleted",[])))')
assert_eq 'batch delete Deleted count=3 (missing included)' 3 "$DC"

rm -f "$TMP" "$TMP.out" "$TMP.del"
aws s3 rb "s3://$BA" --endpoint-url "$ENDPOINT" --no-verify-ssl --force >/dev/null 2>&1
aws s3 rb "s3://$BB" --endpoint-url "$ENDPOINT" --no-verify-ssl --force >/dev/null 2>&1
