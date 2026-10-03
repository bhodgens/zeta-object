# 29-multirange.sh — multi-span Range GET serves 206 multipart/byteranges
# with each span's Content-Range header (RFC 9110); single-range and
# malformed headers keep their existing behavior; over-cap falls back to 200.
set -u
BKT='e2e-29-multirange'
aws s3api create-bucket --bucket "$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

# 4KB object with known content (byte i = i mod 256)
TMP=$(mktemp /tmp/e2e29.XXXXXX)
python3 -c "import sys; sys.stdout.buffer.write(bytes(i % 256 for i in range(4096)))" > "$TMP"
s3req PUT "/$BKT/m.bin" --data-binary @"$TMP" -H 'Content-Type: application/octet-stream'
assert_eq 'setup: put 4096-byte object' 200 "$S3_STATUS"

# 3-span Range → 206 multipart/byteranges; s3req stores only the body, so
# capture the headers with a direct curl -D for the Content-Type assertion.
HDRS=$(mktemp /tmp/e2e29.XXXXXX)
BODY=$(mktemp /tmp/e2e29.XXXXXX)
STATUS=$(curl -sk -o "$BODY" -D "$HDRS" -w '%{http_code}' \
	--aws-sigv4 "aws:amz:us-east-1:s3" \
	--user "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" \
	-H 'Range: bytes=0-99,200-299,1000-1099' "$BASE_URL/$BKT/m.bin" 2>/dev/null)
assert_eq '3-span range → 206' 206 "$STATUS"
S3_CT=$(sed -n 's/^[Cc]ontent-[Tt]ype: //p' "$HDRS" | head -1 | tr -d '\r')
assert_contains 'Content-Type is multipart/byteranges' "$S3_CT" 'multipart/byteranges'
assert_contains 'boundary parameter present' "$S3_CT" 'boundary='
# The body is binary — grep the file directly (command substitution would
# strip null bytes and warn).
for want in 'Content-Range: bytes 0-99/4096' 'Content-Range: bytes 200-299/4096' \
	'Content-Range: bytes 1000-1099/4096' 'Content-Type: application/octet-stream'; do
	if grep -qF "$want" "$BODY"; then
		assert_eq "multipart body contains $want" 0 0
	else
		assert_eq "multipart body contains $want" 0 1
	fi
done
# Exactly one part per requested span (grep -cF counts whole lines).
assert_eq '3-span response has exactly 3 parts' 3 "$(grep -cF 'Content-Range:' "$BODY" | tr -d ' ')"

# Span bytes verified: each requested window must appear verbatim in the body.
for win in 0 200 1000; do
	if python3 - "$BODY" "$TMP" "$win" <<'PYEOF'
import sys
resp = open(sys.argv[1], 'rb').read()
src = open(sys.argv[2], 'rb').read()
start = int(sys.argv[3])
sys.exit(0 if src[start:start+100] in resp else 1)
PYEOF
	then
		assert_eq "span bytes=$win-$((win+99)) present in response" 0 0
	else
		assert_eq "span bytes=$win-$((win+99)) present in response" 0 1
	fi
done
rm -f "$HDRS" "$BODY"

# Single span unchanged → plain 206 (not multipart)
HDRS2=$(mktemp /tmp/e2e29.XXXXXX)
STATUS2=$(curl -sk -o /dev/null -D "$HDRS2" -w '%{http_code}' \
	--aws-sigv4 "aws:amz:us-east-1:s3" \
	--user "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" \
	-H 'Range: bytes=0-99' "$BASE_URL/$BKT/m.bin" 2>/dev/null)
assert_eq 'single span → 206' 206 "$STATUS2"
S3_CT=$(sed -n 's/^[Cc]ontent-[Tt]ype: //p' "$HDRS2" | head -1 | tr -d '\r')
case "$S3_CT" in
	multipart/*) assert_eq 'single span is not multipart' not-multipart multipart ;;
	*) assert_eq 'single span is not multipart' not-multipart not-multipart ;;
esac
rm -f "$HDRS2"

# Malformed Range unchanged → 200 full body (capture via file: the earlier
# PUT response body may contain binary padding — avoid null-byte warnings by
# not command-substituting a binary body through s3req)
MFST=$(mktemp /tmp/e2e29.XXXXXX)
MFS=$(curl -sk -o "$MFST" -w '%{http_code}' \
	--aws-sigv4 "aws:amz:us-east-1:s3" \
	--user "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" \
	-H 'Range: bytes=abc' "$BASE_URL/$BKT/m.bin" 2>/dev/null)
assert_eq 'malformed range → 200' 200 "$MFS"
assert_eq 'malformed range → full 4096-byte body' 4096 "$(wc -c < "$MFST" | tr -d ' ')"
rm -f "$MFST"

# Zero valid spans (all beyond EOF) → 416, not a 200/206 fallback.
# (RFC 9110 §14.2: when NO byte-range-spec overlaps, the server MUST
# respond 416. Fixed 2026-10-02: ParseMultiRange now distinguishes
# "no VALID spec" (200) from "no SATISFIABLE span" (416).
# Single-span `bytes=5000-` (case 06) already 416'd.)
ZVS=$(curl -sk -o /dev/null -w '%{http_code}' \
	--aws-sigv4 "aws:amz:us-east-1:s3" \
	--user "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" \
	-H 'Range: bytes=5000-5100,6000-6100' "$BASE_URL/$BKT/m.bin" 2>/dev/null)
assert_eq 'zero valid spans → 416' 416 "$ZVS"

# Over-cap (101 disjoint spans) → 200 full body
BIGSPEC=$(python3 -c "print(','.join(f'{i*40}-{i*40+1}' for i in range(101)))")
OCT=$(mktemp /tmp/e2e29.XXXXXX)
OCS=$(curl -sk -o "$OCT" -w '%{http_code}' \
	--aws-sigv4 "aws:amz:us-east-1:s3" \
	--user "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" \
	-H "Range: bytes=$BIGSPEC" "$BASE_URL/$BKT/m.bin" 2>/dev/null)
assert_eq '101 spans (over cap) → 200 full body' 200 "$OCS"
assert_eq 'over cap → full 4096-byte body' 4096 "$(wc -c < "$OCT" | tr -d ' ')"
rm -f "$OCT"

# Exact cap (100 disjoint 2-byte spans) → 206 multipart/byteranges.
CAPSPEC=$(python3 -c "print(','.join(f'{i*40}-{i*40+1}' for i in range(100)))")
CHDRS=$(mktemp /tmp/e2e29.XXXXXX)
CCS=$(curl -sk -o /dev/null -D "$CHDRS" -w '%{http_code}' \
	--aws-sigv4 "aws:amz:us-east-1:s3" \
	--user "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" \
	-H "Range: bytes=$CAPSPEC" "$BASE_URL/$BKT/m.bin" 2>/dev/null)
assert_eq '100 spans (exact cap) → 206' 206 "$CCS"
CAP_CT=$(sed -n 's/^[Cc]ontent-[Tt]ype: //p' "$CHDRS" | head -1 | tr -d '\r')
assert_contains 'exact cap response is multipart/byteranges' "$CAP_CT" 'multipart/byteranges'
rm -f "$CHDRS"

rm -f "$TMP"
aws s3 rb "s3://$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl --force >/dev/null 2>&1
