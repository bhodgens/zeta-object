# 04-listing.sh — prefix/delimiter/max-keys/continuation pagination loop,
# KeyCount <= MaxKeys, max-keys=0, start-after.
# NOTE: KeyCount is read with a len(Contents) fallback because the server omits
# KeyCount unless a max-keys param is present (real S3 always includes it —
# reported as a product divergence, not worked around silently).
set -u
BKT='e2e-04-listing'
aws s3api create-bucket --bucket "$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

keycount_py='import json,sys;d=json.load(sys.stdin);print(d.get("KeyCount", len(d.get("Contents",[]))))'

# Layout: 6 files under pfx/ (a..f), 2 loose files at root, 1 nested under pfx/sub/.
TMP=$(mktemp /tmp/e2e04.XXXXXX)
printf 'x' > "$TMP"
for k in a b c d e f; do
	aws s3 cp "$TMP" "s3://$BKT/pfx/$k.txt" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
done
aws s3 cp "$TMP" "s3://$BKT/root-1.txt" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
aws s3 cp "$TMP" "s3://$BKT/root-2.txt" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
aws s3 cp "$TMP" "s3://$BKT/pfx/sub/deep.txt" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

# prefix listing
J=$(aws s3api list-objects-v2 --bucket "$BKT" --prefix 'pfx/' --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null)
KC=$(printf '%s' "$J" | python3 -c "$keycount_py")
assert_eq 'prefix pfx/ KeyCount=7 (6 files + sub/deep)' 7 "$KC"

# delimiter: common prefixes collapse sub/
J=$(aws s3api list-objects-v2 --bucket "$BKT" --prefix 'pfx/' --delimiter '/' --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null)
read -r KC CP <<< "$(printf '%s' "$J" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get("KeyCount", len(d.get("Contents",[]))), len(d.get("CommonPrefixes",[])))')"
assert_eq 'delimiter KeyCount=6 (only pfx/*.txt)' 6 "$KC"
assert_eq 'delimiter CommonPrefixes=1 (pfx/sub/)' 1 "$CP"

# max-keys=0
J=$(aws s3api list-objects-v2 --bucket "$BKT" --max-keys 0 --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null)
KC=$(printf '%s' "$J" | python3 -c "$keycount_py")
assert_eq 'max-keys=0 → KeyCount 0' 0 "$KC"

# start-after
J=$(aws s3api list-objects-v2 --bucket "$BKT" --start-after 'pfx/c.txt' --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null)
FIRST=$(printf '%s' "$J" | python3 -c 'import json,sys;print(json.load(sys.stdin)["Contents"][0]["Key"])')
assert_eq 'start-after skips up to pfx/c.txt' 'pfx/d.txt' "$FIRST"

# pagination loop: max-keys=2, iterate until IsTruncated false,
# assert KeyCount <= MaxKeys on every page and tokens non-empty while truncated.
# NOTE: this server DROPS the boundary object at each page break (the
# continuation token is the first excluded key but is then applied as an
# exclusive marker — see the bug report). With 9 objects and max-keys=2 the
# server returns 3 pages of 2 + an empty page = 6 objects total. Real S3
# returns all 9 across 5 pages. We assert the pagination MECHANICS here
# (IsTruncated/token presence/KeyCount<=MaxKeys) and pin the object loss as
# the reported bug; the total-objects assert documents current behavior.
TOTAL=0
TOKEN=''
PAGES_OK=0
TRUNC_TOKEN_OK=0
PAGES=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
	if [ -n "$TOKEN" ]; then
		J=$(aws s3api list-objects-v2 --bucket "$BKT" --max-keys 2 --continuation-token "$TOKEN" --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null)
	else
		J=$(aws s3api list-objects-v2 --bucket "$BKT" --max-keys 2 --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null)
	fi
	PAGES=$((PAGES + 1))
	read -r KC TR NEXT <<< "$(printf '%s' "$J" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get("KeyCount", len(d.get("Contents",[]))), str(d.get("IsTruncated",False)).lower(), d.get("NextContinuationToken",""))')"
	[ "$KC" -le 2 ] && PAGES_OK=$((PAGES_OK + 1))
	TOTAL=$((TOTAL + KC))
	[ "$TR" = 'true' ] || break
	if [ -n "$NEXT" ]; then
		TRUNC_TOKEN_OK=$((TRUNC_TOKEN_OK + 1))
		TOKEN="$NEXT"
	fi
done
assert_eq 'pagination: KeyCount<=MaxKeys on every page' "$PAGES" "$PAGES_OK"
assert_eq 'pagination: continuation token present whenever truncated' "$((PAGES - 1))" "$TRUNC_TOKEN_OK"
# Real-S3 semantics: all 9 objects across 5 pages (2+2+2+2+1). The boundary
# object is the first key of the next page and MUST be listed — the <=
# marker bug that dropped it is fixed (leaf-3.6 e2e finding).
assert_eq 'pagination total=9 objects (all listed, no boundary drops)' 9 "$TOTAL"

rm -f "$TMP"
aws s3 rb "s3://$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl --force >/dev/null 2>&1
