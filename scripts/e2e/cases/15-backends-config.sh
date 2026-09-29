# 15-backends-config.sh — config "backends" key (backend-interface leaf 03):
#   1. an explicit backends:{fs:{root, options}} declaration + a bucket
#      selecting it via the object form ("backend": "fs") must start the
#      server and serve a full object round-trip through that selection.
#   2. Fail-loud contract: a bucket selecting an UNKNOWN backend name must
#      abort startup with the ErrUnknownBackend error text — never a silent
#      fs fallback (grep the startup log; launch_expect_fail helper).
#
# Private-server pattern (case 14): the suite server is left untouched, so
# the harness's case-boundary relaunch logic keeps working for later cases.
set -u
B15_ROOT=$(mktemp -d /tmp/e2e15-backend.XXXXXX)
B15_WORK=$(mktemp -d /tmp/e2e15-work.XXXXXX)
B15_CERT=$(mktemp -d /tmp/e2e15-cert.XXXXXX)
B15_BKT='e2e15-backend-bkt'
B15_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
openssl req -x509 -newkey rsa:2048 -keyout "$B15_CERT/key.pem" -out "$B15_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
mkdir -p "$B15_WORK/data"
# main() only VALIDATES configured bucket paths (warns when missing) — it
# never creates them; case 14 points at the mktemp dir itself, here the
# backend root nests the bucket dir, so pre-create it.
mkdir -p "$B15_ROOT/$B15_BKT"

b15_cleanup() {
	if [ -n "${B15_PID:-}" ] && kill -0 "$B15_PID" 2>/dev/null; then
		kill -TERM "$B15_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$B15_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$B15_PID" 2>/dev/null
	fi
	[ -n "${B15_FAILOG:-}" ] && rm -f "$B15_FAILOG"
	rm -rf "$B15_ROOT" "$B15_WORK" "$B15_CERT"
}
trap b15_cleanup EXIT

# --- part 1: explicit backends:{fs:{...}} + bucket selecting it -------------
cat > "$B15_WORK/config.json" <<EOF
{
  "dataDir": "$B15_WORK/data",
  "listenAddr": "127.0.0.1:$B15_PORT",
  "certFile": "$B15_CERT/cert.pem",
  "keyFile": "$B15_CERT/key.pem",
  "backends": {
    "fs": {
      "root": "$B15_ROOT",
      "options": {"e2e-marker": "case15"}
    }
  },
  "buckets": {
    "$B15_BKT": {"path": "$B15_ROOT/$B15_BKT", "backend": "fs"}
  }
}
EOF

# This server's log is case-local (not the shared suite log) so the
# 'Registered backends:' grep below sees only THIS startup's line.
: > "$B15_WORK/server.log"
ZETAOBJECT_CONFIG="$B15_WORK/config.json" ./zeta-object-server >"$B15_WORK/server.log" 2>&1 &
B15_PID=$!
B15_ENDPOINT="https://127.0.0.1:$B15_PORT"
ENDPOINT="$B15_ENDPOINT"
BASE_URL="$ENDPOINT"
export E2E_ENDPOINT="$ENDPOINT"
if ! wait_for_port 127.0.0.1 "$B15_PORT" 15; then
	echo '  (backends-config server did not start — failing case)'
	E2E_FAIL=$((E2E_FAIL + 1))
	exit 0
fi
if grep -q 'Registered backends:' "$B15_WORK/server.log"; then
	assert_eq 'startup logs registered backends' 0 0
else
	assert_eq 'startup logs registered backends' 0 1
fi

# The custom-bucket path must exist (main validates configured bucket paths).
if [ -d "$B15_ROOT/$B15_BKT" ]; then
	assert_eq 'backend-selected bucket path exists at startup' 0 0
else
	assert_eq 'backend-selected bucket path exists at startup' 0 1
fi

# Full object round-trip THROUGH the explicit backend selection.
printf 'case15-backend-roundtrip-body' > "$B15_WORK/obj.txt"
aws_ok 'PutObject via explicit backends selection' s3api put-object \
	--bucket "$B15_BKT" --key 'case15/obj.txt' --body "$B15_WORK/obj.txt"
aws_ok 'GetObject via explicit backends selection' s3api get-object \
	--bucket "$B15_BKT" --key 'case15/obj.txt' "$B15_WORK/got.txt"
if cmp -s "$B15_WORK/obj.txt" "$B15_WORK/got.txt"; then
	assert_eq 'round-trip byte-exact through explicit backend' same same
else
	assert_eq 'round-trip byte-exact through explicit backend' same DIFFER
fi
# The fs backend's frozen layout: flat under the bucket's configured path.
if [ -f "$B15_ROOT/$B15_BKT/case15/obj.txt" ]; then
	assert_eq 'object data flat under selected backend root' flat flat
else
	assert_eq 'object data flat under selected backend root' flat MISSING
fi

# Stop the good-config server before the failure probe reuses the port.
kill -TERM "$B15_PID" 2>/dev/null
for _ in 1 2 3 4 5 6 7 8 9 10; do
	kill -0 "$B15_PID" 2>/dev/null || break
	sleep 0.5
done
kill -9 "$B15_PID" 2>/dev/null
wait "$B15_PID" 2>/dev/null
B15_PID=''

# --- part 2: unknown backend name must abort startup (fail-loud) -----------
B15_FAILOG="$B15_WORK/fail.log"
cat > "$B15_WORK/config-bad.json" <<EOF
{
  "dataDir": "$B15_WORK/data",
  "listenAddr": "127.0.0.1:$B15_PORT",
  "certFile": "$B15_CERT/cert.pem",
  "keyFile": "$B15_CERT/key.pem",
  "buckets": {
    "$B15_BKT": {"path": "$B15_ROOT/$B15_BKT", "backend": "no-such-backend"}
  }
}
EOF
launch_expect_fail "$B15_WORK/config-bad.json" "$B15_FAILOG" 10
assert_eq 'unknown backend name aborts startup (process exits)' 0 "$FAILSTART_EXIT"
assert_eq 'server exits non-zero on unknown backend' 1 "$(( FAILSTART_RC > 0 ? 1 : 0 ))"
assert_contains 'startup log names the unknown backend' "$(cat "$B15_FAILOG")" 'unknown backend type "no-such-backend"'
assert_contains 'startup log lists registered backend names' "$(cat "$B15_FAILOG")" 'registered: [fs]'
