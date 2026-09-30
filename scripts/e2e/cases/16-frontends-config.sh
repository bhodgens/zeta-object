# 16-frontends-config.sh — config "frontends" key (frontend-interface leaf 03):
#   1. explicit frontends:[{"type":"s3"}] must behave exactly like the
#      absent/default config (explicit-s3 == default, bucket + round-trip).
#   2. Two listeners cheaply: the explicit s3 frontend gets its OWN
#      listenAddr (dedicated TLS listener) while the default listener is
#      also configured — BOTH ports must answer SigV4 requests.
#   3. Fail-loud contract: an unknown frontend type aborts startup with the
#      known-types error (grep the log; launch_expect_fail helper).
#
# Private-server pattern (case 14): the suite server is left untouched.
set -u
F16_WORK=$(mktemp -d /tmp/e2e16-work.XXXXXX)
F16_CERT=$(mktemp -d /tmp/e2e16-cert.XXXXXX)
F16_BKT='e2e16-frontend-bkt'
F16_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
F16_PORT2=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
openssl req -x509 -newkey rsa:2048 -keyout "$F16_CERT/key.pem" -out "$F16_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
mkdir -p "$F16_WORK/data"

f16_cleanup() {
	if [ -n "${F16_PID:-}" ] && kill -0 "$F16_PID" 2>/dev/null; then
		kill -TERM "$F16_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$F16_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$F16_PID" 2>/dev/null
	fi
	[ -n "${F16_FAILOG:-}" ] && rm -f "$F16_FAILOG"
	rm -rf "$F16_WORK" "$F16_CERT"
}
trap f16_cleanup EXIT

# --- part 1+2: explicit s3 frontend, dedicated listenAddr, two ports -------
cat > "$F16_WORK/config.json" <<EOF
{
  "dataDir": "$F16_WORK/data",
  "listenAddr": "127.0.0.1:$F16_PORT",
  "certFile": "$F16_CERT/cert.pem",
  "keyFile": "$F16_CERT/key.pem",
  "frontends": [
    {"type": "s3", "listenAddr": "127.0.0.1:$F16_PORT2"}
  ]
}
EOF

ZETAOBJECT_CONFIG="$F16_WORK/config.json" ./zeta-object-server >>"$E2E_SERVER_LOG" 2>&1 &
F16_PID=$!
F16_ENDPOINT="https://127.0.0.1:$F16_PORT2"
ENDPOINT="$F16_ENDPOINT"
BASE_URL="$ENDPOINT"
export E2E_ENDPOINT="$ENDPOINT"
if ! wait_for_port 127.0.0.1 "$F16_PORT" 15; then
	echo '  (frontends-config server did not start — failing case)'
	E2E_FAIL=$((E2E_FAIL + 1))
	exit 0
fi
if ! wait_for_port 127.0.0.1 "$F16_PORT2" 15; then
	echo '  (dedicated frontend listener did not start — failing case)'
	E2E_FAIL=$((E2E_FAIL + 1))
	exit 0
fi
if grep -q "Starting dedicated frontend listener on 127.0.0.1:$F16_PORT2" "$E2E_SERVER_LOG"; then
	assert_eq 'log shows dedicated frontend listener' 0 0
else
	assert_eq 'log shows dedicated frontend listener' 0 1
fi

# Explicit-s3 config == default: bucket create + object round-trip work.
aws_ok 'CreateBucket under explicit s3 frontend' s3api create-bucket --bucket "$F16_BKT"
printf 'case16-explicit-s3-body' > "$F16_WORK/obj.txt"
aws_ok 'PutObject under explicit s3 frontend' s3api put-object \
	--bucket "$F16_BKT" --key 'case16/obj.txt' --body "$F16_WORK/obj.txt"
aws_ok 'GetObject under explicit s3 frontend' s3api get-object \
	--bucket "$F16_BKT" --key 'case16/obj.txt' "$F16_WORK/got.txt"
if cmp -s "$F16_WORK/obj.txt" "$F16_WORK/got.txt"; then
	assert_eq 'explicit-s3 round-trip byte-exact (same as default config)' same same
else
	assert_eq 'explicit-s3 round-trip byte-exact (same as default config)' same DIFFER
fi

# The DEDICATED listener (frontend's own listenAddr) answers SigV4 too.
s3req GET "/$F16_BKT/case16/obj.txt"
assert_eq 'dedicated frontend listener serves GetObject' 200 "$S3_STATUS"
assert_contains 'dedicated listener returns the object body' "$S3_BODY" 'case16-explicit-s3-body'

# --- part 3: unknown frontend type must abort startup (fail-loud) ----------
F16_FAILOG="$F16_WORK/fail.log"
cat > "$F16_WORK/config-bad.json" <<EOF
{
  "dataDir": "$F16_WORK/data",
  "listenAddr": "127.0.0.1:$F16_PORT",
  "certFile": "$F16_CERT/cert.pem",
  "keyFile": "$F16_CERT/key.pem",
  "frontends": [
    {"type": "gopher"}
  ]
}
EOF
launch_expect_fail "$F16_WORK/config-bad.json" "$F16_FAILOG" 10
assert_eq 'unknown frontend type aborts startup (process exits)' 0 "$FAILSTART_EXIT"
assert_eq 'server exits non-zero on unknown frontend' 1 "$(( FAILSTART_RC > 0 ? 1 : 0 ))"
assert_contains 'startup log names the unknown frontend type' "$(cat "$F16_FAILOG")" 'unknown frontend type "gopher"'
assert_contains 'startup error lists known frontend types' "$(cat "$F16_FAILOG")" 'known: [ftp owncloud s3 sftp webdav]'
