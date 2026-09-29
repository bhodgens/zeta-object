# 14-custom-bucket.sh — bughunt D1 regression: a custom bucket configured via
# config "buckets" must place objects FLAT at <custom>/key — the exact path
# the handler-side staging math uses. Pins Put/Get/Stat/Delete/List plus a
# full multipart round-trip through the custom bucket (pre-fix: seam Put
# landed nested at <custom>/<bucket>/key and multipart-complete wrote flat
# while GET read nested → 404).
#
# The suite server runs WITHOUT a custom bucket configured (run-e2e.sh owns
# the config), so this case launches its OWN short-lived server on a fresh
# port with a config that maps e2e-custom onto a tmpdir, runs the proofs,
# then kills it. The suite server is left untouched, so the harness's
# own case-boundary relaunch logic keeps working for later cases.
set -u
CUSTOM_ROOT=$(mktemp -d /tmp/e2e14-custom.XXXXXX)
CUSTOM_BKT='e2e-custom'
CUSTOM_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
CUSTOM_CERT=$(mktemp -d /tmp/e2e14-cert.XXXXXX)
openssl req -x509 -newkey rsa:2048 -keyout "$CUSTOM_CERT/key.pem" -out "$CUSTOM_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1

CUSTOM_WORK=$(mktemp -d /tmp/e2e14-work.XXXXXX)
mkdir -p "$CUSTOM_WORK/data"
cat > "$CUSTOM_WORK/config.json" <<EOF
{
  "dataDir": "$CUSTOM_WORK/data",
  "listenAddr": "127.0.0.1:$CUSTOM_PORT",
  "certFile": "$CUSTOM_CERT/cert.pem",
  "keyFile": "$CUSTOM_CERT/key.pem",
  "buckets": {
    "$CUSTOM_BKT": "$CUSTOM_ROOT"
  }
}
EOF

# Launch a private server for this case; the SUITE server stays untouched.
MINIS3_CONFIG="$CUSTOM_WORK/config.json" ./mini-s3-server >>"$E2E_SERVER_LOG" 2>&1 &
CUSTOM_PID=$!
CUSTOM_ENDPOINT="https://127.0.0.1:$CUSTOM_PORT"
cleanup_custom() {
	if kill -0 "$CUSTOM_PID" 2>/dev/null; then
		kill -TERM "$CUSTOM_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$CUSTOM_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$CUSTOM_PID" 2>/dev/null
	fi
	rm -rf "$CUSTOM_ROOT" "$CUSTOM_CERT" "$CUSTOM_WORK"
}
trap cleanup_custom EXIT

# Point the shared helpers at the custom server for this case.
ENDPOINT="$CUSTOM_ENDPOINT"
BASE_URL="$ENDPOINT"
export E2E_ENDPOINT="$ENDPOINT"
if ! wait_for_port 127.0.0.1 "$CUSTOM_PORT" 15; then
	echo '  (custom-bucket server did not start — failing case)'
	E2E_FAIL=$((E2E_FAIL + 1))
	exit 0
fi

# Custom buckets are config-controlled: PUT bucket is idempotent when the
# path exists on disk.
aws_ok 'HeadBucket via aws ls (custom bucket listed)' s3api head-bucket --bucket "$CUSTOM_BKT"

# --- simple object round trip: must land FLAT at <custom>/key --------------
printf 'custom-flat-body' > "$CUSTOM_WORK/obj.txt"
aws_ok 'PutObject into custom bucket' s3api put-object --bucket "$CUSTOM_BKT" \
	--key 'plain/obj.txt' --body "$CUSTOM_WORK/obj.txt" --content-type text/plain

# Byte-exact Get back through the API (pre-fix: NoSuchKey — nested split).
aws_ok 'GetObject from custom bucket' s3api get-object --bucket "$CUSTOM_BKT" \
	--key 'plain/obj.txt' "$CUSTOM_WORK/got.txt"
if cmp -s "$CUSTOM_WORK/obj.txt" "$CUSTOM_WORK/got.txt"; then
	assert_eq 'custom bucket GetObject byte-exact' same same
else
	assert_eq 'custom bucket GetObject byte-exact' same DIFFER
fi

# THE D1 pin: the data file exists at <custom>/plain/obj.txt — flat, never
# nested under <custom>/<bucket>/.
if [ -f "$CUSTOM_ROOT/plain/obj.txt" ]; then
	assert_eq 'object data flat at <custom>/plain/obj.txt' flat flat
else
	assert_eq 'object data flat at <custom>/plain/obj.txt' flat MISSING
fi
if [ -e "$CUSTOM_ROOT/$CUSTOM_BKT" ]; then
	assert_eq 'no nested <custom>/<bucket>/ directory' none NESTED_DIR_EXISTS
else
	assert_eq 'no nested <custom>/<bucket>/ directory' none none
fi
# Sidecar also flat under <custom>/.metadata/.
if [ -f "$CUSTOM_ROOT/.metadata/plain/obj.txt.meta" ]; then
	assert_eq 'sidecar flat at <custom>/.metadata/...' flat flat
else
	assert_eq 'sidecar flat at <custom>/.metadata/...' flat MISSING
fi

# Stat + List see the object.
STAT_OUT=$(aws s3api head-object --bucket "$CUSTOM_BKT" --key 'plain/obj.txt' \
	--endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null)
assert_eq 'HeadObject content-length' 16 "$(printf '%s' "$STAT_OUT" | python3 -c 'import json,sys;print(json.load(sys.stdin)["ContentLength"])')"
LIST_KEYS=$(aws s3api list-objects-v2 --bucket "$CUSTOM_BKT" \
	--endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | \
	python3 -c 'import json,sys;d=json.load(sys.stdin);print(" ".join(o["Key"] for o in d.get("Contents",[])))')
assert_contains 'ListObjectsV2 lists custom-bucket key' "$LIST_KEYS" 'plain/obj.txt'

# --- multipart round trip THROUGH the custom bucket -------------------------
python3 - "$CUSTOM_WORK" <<'PY'
import os, sys, random
d = sys.argv[1]
rng = random.Random(7)
with open(os.path.join(d, 'p1'), 'wb') as f:
	f.write(bytes(rng.getrandbits(8) for _ in range(5*1024*1024)))
with open(os.path.join(d, 'p2'), 'wb') as f:
	f.write(b'custom-tail')
PY
cat "$CUSTOM_WORK/p1" "$CUSTOM_WORK/p2" > "$CUSTOM_WORK/mp-expected"

MPID=$(aws s3api create-multipart-upload --bucket "$CUSTOM_BKT" --key 'mp/assembled.bin' \
	--endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["UploadId"])')
assert_eq 'custom multipart id is 32-hex' 32 "${#MPID}"
E1=$(aws s3api upload-part --bucket "$CUSTOM_BKT" --key 'mp/assembled.bin' --upload-id "$MPID" \
	--part-number 1 --body "$CUSTOM_WORK/p1" --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["ETag"])')
E2=$(aws s3api upload-part --bucket "$CUSTOM_BKT" --key 'mp/assembled.bin' --upload-id "$MPID" \
	--part-number 2 --body "$CUSTOM_WORK/p2" --endpoint-url "$ENDPOINT" --no-verify-ssl 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["ETag"])')
cat > "$CUSTOM_WORK/mp-comp.json" <<EOF
{"Parts": [{"ETag": $E1, "PartNumber": 1}, {"ETag": $E2, "PartNumber": 2}]}
EOF
aws_ok 'CompleteMultipartUpload in custom bucket' s3api complete-multipart-upload \
	--bucket "$CUSTOM_BKT" --key 'mp/assembled.bin' --upload-id "$MPID" \
	--multipart-upload "file://$CUSTOM_WORK/mp-comp.json"

# Pre-fix failure mode: complete wrote FLAT (handler staging) but GET read
# the NESTED path → NoSuchKey. The GET must succeed now.
aws_ok 'GET after custom multipart complete (D1 split-brain pin)' s3api get-object \
	--bucket "$CUSTOM_BKT" --key 'mp/assembled.bin' "$CUSTOM_WORK/mp-got"
if cmp -s "$CUSTOM_WORK/mp-expected" "$CUSTOM_WORK/mp-got"; then
	assert_eq 'custom multipart assembly byte-exact' same same
else
	assert_eq 'custom multipart assembly byte-exact' same DIFFER
fi
if [ -f "$CUSTOM_ROOT/mp/assembled.bin" ]; then
	assert_eq 'multipart final object flat at <custom>/mp/assembled.bin' flat flat
else
	assert_eq 'multipart final object flat at <custom>/mp/assembled.bin' flat MISSING
fi

# --- copy-with-metadata replacement through the custom bucket ---------------
aws_ok 'CopyObject within custom bucket' s3api copy-object --bucket "$CUSTOM_BKT" \
	--key 'plain/obj-copy.txt' --copy-source "$CUSTOM_BKT/plain/obj.txt"
aws_ok 'GetObject copied object' s3api get-object --bucket "$CUSTOM_BKT" \
	--key 'plain/obj-copy.txt' "$CUSTOM_WORK/copy-got"
if cmp -s "$CUSTOM_WORK/obj.txt" "$CUSTOM_WORK/copy-got"; then
	assert_eq 'custom bucket copy byte-exact' same same
else
	assert_eq 'custom bucket copy byte-exact' same DIFFER
fi

# --- Delete removes the flat data file ---------------------------------------
aws_ok 'DeleteObject in custom bucket' s3api delete-object --bucket "$CUSTOM_BKT" \
	--key 'plain/obj.txt'
if [ -e "$CUSTOM_ROOT/plain/obj.txt" ]; then
	assert_eq 'deleted object data gone from <custom>' gone PRESENT
else
	assert_eq 'deleted object data gone from <custom>' gone gone
fi

# Tear down the private server; the suite server continues for later cases.
cleanup_custom
trap - EXIT
