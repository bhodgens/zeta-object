# 30-tagging.sh — object tagging (tagging tree leaf 04): x-amz-tagging on
# PUT, TagCount on HEAD, ?tagging GET/PUT/DELETE round-trip, COPY tag
# directives (default COPY + REPLACE), InvalidTag on the reserved aws:
# prefix. Wire-level asserts via s3req (the aws CLI cannot surface S3
# error codes — see lib.sh); CLI success paths via aws_ok.
set -u
BKT='e2e-30-tagging'
aws s3api create-bucket --bucket "$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

TMP=$(mktemp /tmp/e2e30.XXXXXX)
printf 'tagging-payload-30\n' > "$TMP"

# --- PUT with x-amz-tagging ---------------------------------------------------
s3req PUT "/$BKT/tagged.txt" -H 'x-amz-tagging: team=infra&env=prod' --data-binary "@$TMP"
assert_eq 'put with x-amz-tagging → 200' 200 "$S3_STATUS"

# Untagged control object (also proves TagCount is ABSENT when untagged).
s3req PUT "/$BKT/plain.txt" --data-binary "@$TMP"
assert_eq 'put untagged control → 200' 200 "$S3_STATUS"

# GET ?tagging on a nonexistent key → 404 NoSuchKey (not NoSuchTagSet).
s3req GET "/$BKT/no-such-key.txt?tagging"
assert_eq 'GET ?tagging nonexistent key → 404' 404 "$S3_STATUS"
assert_s3code 'GET ?tagging nonexistent key → NoSuchKey' 'NoSuchKey'

# 0-byte object PUT with x-amz-tagging → 200; tags round-trip via GET ?tagging.
s3req PUT "/$BKT/empty.bin" -H 'x-amz-tagging: kind=empty' --data-binary ''
assert_eq 'put 0-byte object with x-amz-tagging → 200' 200 "$S3_STATUS"
s3req GET "/$BKT/empty.bin?tagging"
assert_eq '0-byte object GET ?tagging → 200' 200 "$S3_STATUS"
assert_contains '0-byte object tag key round-trips' "$S3_BODY" '<Key>kind</Key>'
assert_contains '0-byte object tag value round-trips' "$S3_BODY" '<Value>empty</Value>'

# --- TagCount on HEAD ----------------------------------------------------------
aws_ok 'head tagged object' s3api head-object --bucket "$BKT" --key 'tagged.txt'
TC=$(aws s3api head-object --bucket "$BKT" --key 'tagged.txt' --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin).get("TagCount",""))')
assert_eq 'HEAD TagCount = 2' 2 "$TC"
TC0=$(aws s3api head-object --bucket "$BKT" --key 'plain.txt' --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin).get("TagCount",""))')
assert_eq 'HEAD untagged → no TagCount' '' "$TC0"

# --- GET ?tagging: XML round-trip ----------------------------------------------
s3req GET "/$BKT/tagged.txt?tagging"
assert_eq 'GET ?tagging → 200' 200 "$S3_STATUS"
assert_contains 'GET ?tagging TagSet XML envelope' "$S3_BODY" '<Tagging>'
assert_contains 'GET ?tagging key team' "$S3_BODY" '<Key>team</Key>'
assert_contains 'GET ?tagging value infra' "$S3_BODY" '<Value>infra</Value>'
assert_contains 'GET ?tagging key env' "$S3_BODY" '<Key>env</Key>'
assert_contains 'GET ?tagging value prod' "$S3_BODY" '<Value>prod</Value>'

# --- PUT ?tagging: replace the tag set ------------------------------------------
TAGXML='<Tagging><TagSet><Tag><Key>team</Key><Value>platform</Value></Tag><Tag><Key>tier</Key><Value>one</Value></Tag></TagSet></Tagging>'
s3req PUT "/$BKT/tagged.txt?tagging" -H 'Content-Type: application/xml' --data-binary "$TAGXML"
assert_eq 'PUT ?tagging replace → 204' 204 "$S3_STATUS"
s3req GET "/$BKT/tagged.txt?tagging"
assert_contains 'replaced tag visible (team=platform)' "$S3_BODY" '<Value>platform</Value>'
assert_contains 'replaced tag visible (tier=one)' "$S3_BODY" '<Key>tier</Key>'
assert_eq 'old tag value gone after replace' 0 "$(printf '%s' "$S3_BODY" | grep -c '<Value>infra</Value>')"

TC2=$(aws s3api head-object --bucket "$BKT" --key 'tagged.txt' --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin).get("TagCount",""))')
assert_eq 'HEAD TagCount tracks replacement = 2' 2 "$TC2"

# --- DELETE ?tagging -------------------------------------------------------------
s3req DELETE "/$BKT/tagged.txt?tagging"
assert_eq 'DELETE ?tagging → 204' 204 "$S3_STATUS"
s3req GET "/$BKT/tagged.txt?tagging"
assert_eq 'GET ?tagging after delete → 404' 404 "$S3_STATUS"
assert_s3code 'GET ?tagging after delete → NoSuchTagSet' 'NoSuchTagSet'
TC3=$(aws s3api head-object --bucket "$BKT" --key 'tagged.txt' --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin).get("TagCount",""))')
assert_eq 'HEAD after delete → no TagCount' '' "$TC3"

# --- COPY default: tags copied ----------------------------------------------------
s3req PUT "/$BKT/copy-src.txt" -H 'x-amz-tagging: origin=alpha' --data-binary "@$TMP"
assert_eq 'put copy source with tags → 200' 200 "$S3_STATUS"
aws_ok 'copy-object default directive' s3api copy-object --bucket "$BKT" --key 'copy-dst.txt' \
	--copy-source "$BKT/copy-src.txt"
s3req GET "/$BKT/copy-dst.txt?tagging"
assert_eq 'COPY default: GET ?tagging → 200' 200 "$S3_STATUS"
assert_contains 'COPY default: origin tag copied' "$S3_BODY" '<Key>origin</Key>'
assert_contains 'COPY default: tag value copied' "$S3_BODY" '<Value>alpha</Value>'
TCC=$(aws s3api head-object --bucket "$BKT" --key 'copy-dst.txt' --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin).get("TagCount",""))')
assert_eq 'COPY default: HEAD TagCount on copy = 1' 1 "$TCC"

# --- COPY REPLACE: tags replaced ---------------------------------------------------
aws_ok 'copy-object REPLACE directive' s3api copy-object --bucket "$BKT" --key 'copy-repl.txt' \
	--copy-source "$BKT/copy-src.txt" --tagging 'origin=beta' --tagging-directive REPLACE
s3req GET "/$BKT/copy-repl.txt?tagging"
assert_eq 'COPY REPLACE: GET ?tagging → 200' 200 "$S3_STATUS"
assert_contains 'COPY REPLACE: new tag present' "$S3_BODY" '<Value>beta</Value>'
assert_eq 'COPY REPLACE: old tag value gone' 0 "$(printf '%s' "$S3_BODY" | grep -c '<Value>alpha</Value>')"
# REPLACE negative (audit item h): the old tag set is REPLACED, not merged —
# the replacement reuses the key `origin`, so exactly ONE origin key may
# remain and HEAD TagCount must be 1 (a merge would yield 2 tags).
assert_eq 'COPY REPLACE: tag set holds exactly one origin key' 1 "$(printf '%s' "$S3_BODY" | grep -c '<Key>origin</Key>')"
TCR=$(aws s3api head-object --bucket "$BKT" --key 'copy-repl.txt' --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin).get("TagCount",""))')
assert_eq 'COPY REPLACE: HEAD TagCount = 1 (no merge)' 1 "$TCR"

# --- InvalidTag: reserved aws: prefix → 400 -----------------------------------------
s3req PUT "/$BKT/bad.txt" -H 'x-amz-tagging: aws:reserved=true' --data-binary "@$TMP"
assert_eq 'PUT with aws: tag prefix → 400' 400 "$S3_STATUS"
assert_s3code 'aws: prefix error code InvalidTag' 'InvalidTag'
s3req GET "/$BKT/bad.txt"
assert_eq 'rejected PUT created no object → 404' 404 "$S3_STATUS"

# InvalidTag through the ?tagging PUT XML entry point too.
BADXML='<Tagging><TagSet><Tag><Key>aws:oops</Key><Value>x</Value></Tag></TagSet></Tagging>'
s3req PUT "/$BKT/tagged.txt?tagging" -H 'Content-Type: application/xml' --data-binary "$BADXML"
assert_eq 'PUT ?tagging with aws: key → 400' 400 "$S3_STATUS"
assert_s3code 'PUT ?tagging aws: error code InvalidTag' 'InvalidTag'

rm -f "$TMP"
aws s3 rb "s3://$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl --force >/dev/null 2>&1
