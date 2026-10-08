# 30-tagging.sh — object tagging (tagging tree leaf 04): x-amz-tagging on
# PUT, TagCount on HEAD, ?tagging GET/PUT/DELETE round-trip, COPY tag
# directives (default COPY + REPLACE), InvalidTag on the reserved aws:
# prefix. Wire-level asserts via s3req (the aws CLI cannot surface S3
# error codes — see lib.sh); CLI success paths via aws_ok.
set -u
BKT='e2e-30-tagging'
ZT_TMP=$(mktemp /tmp/e2e30-zt.XXXXXX)  # ZFS-gated section body file (trap-named cleanup)
printf 'ztags-payload\n' > "$ZT_TMP"
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

# --- ZFS-gated section: zfs_native_tags (zfs-metadata#13 consumer) -------------
# When the host has zfs AND the shared server runs with zfs_bucket_datasets +
# zfs_native_tags on a ZFS dataDir, the dataset-backed bucket's tags must live
# in zmetad (NOT the sidecar): PUT ?tagging -> GET round-trip -> REPLACE ->
# CLEAR, asserting .meta sidecar absence for tagged objects. Skips gracefully
# (the documented skip convention, case 35e's shape) when zfs/zmetad are
# absent or the shared server has the feature off — live coverage then comes
# from scripts/zfs-validate/run-zfs-validation.sh (exact-tally gate 4).
if ! command -v zfs >/dev/null 2>&1 || ! command -v zmetad >/dev/null 2>&1; then
	echo '  (zfs_native_tags: requires zfs+zmetad on the host — skipping group; live coverage in scripts/zfs-validate/run-zfs-validation.sh)'
elif ! zfs list -H -o name -t filesystem "$E2E_DATA_DIR" >/dev/null 2>&1; then
	echo '  (zfs_native_tags: dataDir is not a ZFS mountpoint — skipping group; live coverage in scripts/zfs-validate/run-zfs-validation.sh)'
else
	ZT_PARENT=$(zfs list -H -o name -t filesystem "$E2E_DATA_DIR" 2>/dev/null | head -1 | tr -d '[:space:]')
	ZT_BKT='e2e-30-ztags'
	ZT_DS="$ZT_PARENT/$ZT_BKT"
	zfs create "$ZT_DS" >/dev/null 2>&1
	# The S3 server discovers buckets under dataDir by directory presence;
	# a created (mounted) dataset IS a directory there.
	ZT_TAGGED='.ztag-roundtrip.txt'
	printf 'ztags-payload\n' > "$ZT_TMP"
	ZT_TAGXML='<Tagging><TagSet><Tag><Key>team</Key><Value>zfs</Value></Tag><Tag><Key>tier</Key><Value>native</Value></Tag></TagSet></Tagging>'
	ZT_REPLXML='<Tagging><TagSet><Tag><Key>team</Key><Value>replaced</Value></Tag></TagSet></Tagging>'
	if [ ! -d "$ZT_DS" ]; then
		echo '  (zfs_native_tags: dataset create failed (unprivileged create not permitted) — skipping group)'
	else
		# PUT x-amz-tagging, then GET ?tagging round-trip.
		s3req PUT "/$ZT_BKT/$ZT_TAGGED" -H 'x-amz-tagging: team=zfs&tier=native' --data-binary "@$ZT_TMP"
		assert_eq 'ztags PUT with x-amz-tagging -> 200' 200 "$S3_STATUS"
		s3req GET "/$ZT_BKT/$ZT_TAGGED?tagging"
		assert_eq 'ztags GET ?tagging -> 200' 200 "$S3_STATUS"
		assert_contains 'ztags round-trip key team' "$S3_BODY" '<Key>team</Key>'
		assert_contains 'ztags round-trip value zfs' "$S3_BODY" '<Value>zfs</Value>'
		assert_contains 'ztags round-trip value native' "$S3_BODY" '<Value>native</Value>'
		# REPLACE via PUT ?tagging.
		s3req PUT "/$ZT_BKT/$ZT_TAGGED?tagging" -H 'Content-Type: application/xml' --data-binary "$ZT_REPLXML"
		assert_eq 'ztags PUT ?tagging replace -> 204' 204 "$S3_STATUS"
		s3req GET "/$ZT_BKT/$ZT_TAGGED?tagging"
		assert_contains 'ztags replaced tag visible' "$S3_BODY" '<Value>replaced</Value>'
		assert_eq 'ztags old tag set gone after replace' 0 "$(printf '%s' "$S3_BODY" | grep -c '<Value>zfs</Value>')"
		# THE ZFS-NATIVE PROOF: no sidecar tags field for the tagged object.
		if [ -f "$ZT_DS/.metadata/$ZT_TAGGED.meta" ]; then
			if grep -q '"tags"' "$ZT_DS/.metadata/$ZT_TAGGED.meta" 2>/dev/null; then
				assert_eq 'ztags sidecar carries NO tags field (zmetad owns tags)' 0 1
			else
				assert_eq 'ztags sidecar carries NO tags field (zmetad owns tags)' 0 0
			fi
		else
			assert_eq 'ztags sidecar carries NO tags field (zmetad owns tags)' 0 0
		fi
		# CLEAR, then NoSuchTagSet again.
		s3req DELETE "/$ZT_BKT/$ZT_TAGGED?tagging"
		assert_eq 'ztags DELETE ?tagging -> 204' 204 "$S3_STATUS"
		s3req GET "/$ZT_BKT/$ZT_TAGGED?tagging"
		assert_eq 'ztags GET ?tagging after clear -> 404' 404 "$S3_STATUS"
		assert_s3code 'ztags after clear -> NoSuchTagSet' 'NoSuchTagSet'
	fi
	zfs destroy "$ZT_DS" >/dev/null 2>&1 || true
fi

rm -f "$TMP" "$ZT_TMP" 2>/dev/null
aws s3 rb "s3://$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl --force >/dev/null 2>&1
