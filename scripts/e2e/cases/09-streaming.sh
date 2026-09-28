# 09-streaming.sh — large PUT via AWS CLI (forces STREAMING-UNSIGNED-PAYLOAD-TRAILER
# aws-chunked framing) round-trip byte-exact.
# NOTE: the corrupted-chunk negative case (bad chunk signature → request rejected,
# stream aborted) is unit-covered in the Go test suite — not reproducible through
# the stock AWS CLI, which always frames correctly. Skipped here.
set -u
BKT='e2e-09-streaming'
aws s3api create-bucket --bucket "$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

# 16 MiB pseudo-random payload — well over the CLI's aws-chunked threshold
TMPD=$(mktemp -d /tmp/e2e09.XXXXXX)
python3 - "$TMPD/big" <<'PY'
import sys, random
rng = random.Random(7)
with open(sys.argv[1], 'wb') as f:
	for _ in range(16 * 64):  # 16 MiB in 16 KiB chunks
		f.write(bytes(rng.getrandbits(8) for _ in range(16 * 1024)))
PY
EXPECT_MD5=$(md5 -q "$TMPD/big")

aws_ok 'large streaming PUT (16MiB)' s3 cp "$TMPD/big" "s3://$BKT/big.bin"

aws s3 cp "s3://$BKT/big.bin" "$TMPD/got" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
GOT_MD5=$(md5 -q "$TMPD/got")
assert_eq 'streaming round-trip md5 matches' "$EXPECT_MD5" "$GOT_MD5"

# second PUT to same key (overwrite through streaming path)
aws_ok 'overwrite via streaming PUT' s3 cp "$TMPD/got" "s3://$BKT/big.bin"
aws s3 cp "s3://$BKT/big.bin" "$TMPD/got2" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
assert_eq 'overwrite round-trip md5 matches' "$EXPECT_MD5" "$(md5 -q "$TMPD/got2")"

aws s3 rb "s3://$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl --force >/dev/null 2>&1
rm -rf "$TMPD"
