# 30-region.sh — SigV4 credential-scope region verification (region-config-2026-10
# leaf 03, AGENTS.md e2e rule for the `region` config key):
#   30a. accept: a server configured with "region": "eu-west-1" serves a
#        HEAD-bucket request signed with scope region eu-west-1 -> 200.
#   30b. reject: the same request signed with scope region us-east-1 ->
#        403 with <Code>SignatureDoesNotMatch</Code> and an error message
#        naming the expected region (eu-west-1) in the XML body.
#   30c. cross-check: an explicit region OTHER than the default (here
#        us-east-1 set explicitly) is likewise strict — a eu-west-1-signed
#        request fails naming the expected region. (The unset/default-mode
#        permissive escape hatch is unit-covered in
#        internal/frontend/s3/region_sigv4_test.go; over the wire the
#        config loader always resolves a concrete region, so default mode
#        is not reachable from a launched server — see the leaf-03 report.)
#
# Private-server pattern (cases 14–16, 18): the suite server is left
# untouched, so the harness's case-boundary relaunch logic keeps working for
# later cases. Signing uses curl --aws-sigv4 with an explicit
# "aws:amz:<region>:s3" scope (the lib.sh s3req helper hardcodes us-east-1,
# so region-varying requests sign inline with curl). Reject-path body
# asserts use GET, not HEAD: HEAD responses carry no XML error body.
set -u
R30_ROOT=$(mktemp -d /tmp/e2e30-region.XXXXXX)
R30_CERT=$(mktemp -d /tmp/e2e30-cert.XXXXXX)
R30_BKT='e2e30-region-bkt'
R30_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
openssl req -x509 -newkey rsa:2048 -keyout "$R30_CERT/key.pem" -out "$R30_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
mkdir -p "$R30_ROOT/data"

r30_cleanup() {
	if [ -n "${R30_PID:-}" ] && kill -0 "$R30_PID" 2>/dev/null; then
		kill -TERM "$R30_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$R30_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$R30_PID" 2>/dev/null
	fi
	rm -rf "$R30_ROOT" "$R30_CERT"
}
trap r30_cleanup EXIT

# r30_req <scope-region> <method> <path> — signed raw request against the
# private server; sets R30_STATUS / R30_BODY (keeps lib.sh's S3_* vars,
# which are pinned to us-east-1, untouched).
r30_req() {
	local scope=$1 method=$2 path=$3 body
	body=$(mktemp /tmp/e2e30-req.XXXXXX)
	R30_STATUS=$(curl -sk -o "$body" -w '%{http_code}' \
		--aws-sigv4 "aws:amz:$scope:s3" \
		--user "${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}" \
		-X "$method" "$R30_URL$path" 2>/dev/null)
	R30_BODY=$(cat "$body")
	rm -f "$body"
}

# r30_expect_code <label> <want-code> — assert the <Code> element of the
# last reject body (XML-escaped apostrophes arrive as &#39;).
r30_expect_code() {
	assert_contains "$1" "$R30_BODY" "<Code>$2</Code>"
}

export AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin
export AWS_DEFAULT_REGION=us-east-1 AWS_EC2_METADATA_DISABLED=true

# --- 30a/30b: server pinned to eu-west-1 (strict) -----------------------------
cat > "$R30_ROOT/config.json" <<EOF
{
  "dataDir": "$R30_ROOT/data",
  "listenAddr": "127.0.0.1:$R30_PORT",
  "certFile": "$R30_CERT/cert.pem",
  "keyFile": "$R30_CERT/key.pem",
  "region": "eu-west-1"
}
EOF
: > "$R30_ROOT/server.log"
ZETAOBJECT_CONFIG="$R30_ROOT/config.json" ./zeta-object-server >"$R30_ROOT/server.log" 2>&1 &
R30_PID=$!
R30_URL="https://127.0.0.1:$R30_PORT"
if ! wait_for_port 127.0.0.1 "$R30_PORT" 15; then
	echo '  (region server did not start — failing case)'
	assert_eq '30 region server started' started missing
	exit 0
fi

# 30a accept path: matching scope region -> bucket create + HEAD 200
# (create/cleanup pairing; the DELETE happens after the mode-2 restart).
r30_req eu-west-1 PUT "/$R30_BKT"
assert_eq '30a eu-west-1-signed CreateBucket accepted' 200 "$R30_STATUS"
r30_req eu-west-1 HEAD "/$R30_BKT"
assert_eq '30a eu-west-1-signed HEAD bucket -> 200' 200 "$R30_STATUS"

# 30b reject path: mismatched scope region -> 403 SignatureDoesNotMatch with
# the expected region named in the error message (GET for the body).
r30_req us-east-1 GET "/$R30_BKT"
assert_eq '30b us-east-1-signed request -> 403' 403 "$R30_STATUS"
r30_expect_code '30b error code is SignatureDoesNotMatch' 'SignatureDoesNotMatch'
assert_contains '30b error message names the expected region' "$R30_BODY" "expected &#39;eu-west-1&#39;"

# Cross-check the other direction: a third region also fails strict mode.
r30_req ap-southeast-2 GET "/$R30_BKT"
assert_eq '30b third-region scope also rejected' 403 "$R30_STATUS"
r30_expect_code '30b third-region error code is SignatureDoesNotMatch' 'SignatureDoesNotMatch'

kill -TERM "$R30_PID" 2>/dev/null
for _ in 1 2 3 4 5 6 7 8 9 10; do
	kill -0 "$R30_PID" 2>/dev/null || break
	sleep 0.5
done
kill -9 "$R30_PID" 2>/dev/null
wait "$R30_PID" 2>/dev/null
R30_PID=''

# --- 30c: explicit default region (us-east-1) is strict too -------------------
cat > "$R30_ROOT/config-use1.json" <<EOF
{
  "dataDir": "$R30_ROOT/data",
  "listenAddr": "127.0.0.1:$R30_PORT",
  "certFile": "$R30_CERT/cert.pem",
  "keyFile": "$R30_CERT/key.pem",
  "region": "us-east-1"
}
EOF
: > "$R30_ROOT/server-use1.log"
ZETAOBJECT_CONFIG="$R30_ROOT/config-use1.json" ./zeta-object-server >"$R30_ROOT/server-use1.log" 2>&1 &
R30_PID=$!
if ! wait_for_port 127.0.0.1 "$R30_PORT" 15; then
	echo '  (explicit-us-east-1 server did not start — failing case)'
	assert_eq '30c explicit-us-east-1 server started' started missing
	exit 0
fi

r30_req eu-west-1 GET "/$R30_BKT"
assert_eq '30c explicit us-east-1: eu-west-1-signed request -> 403' 403 "$R30_STATUS"
r30_expect_code '30c error code is SignatureDoesNotMatch' 'SignatureDoesNotMatch'
assert_contains '30c error message names the expected region' "$R30_BODY" "expected &#39;us-east-1&#39;"

# us-east-1-signed traffic still works on this server (control).
r30_req us-east-1 HEAD "/$R30_BKT"
assert_eq '30c explicit us-east-1: matching-scope HEAD -> 200' 200 "$R30_STATUS"

# Cleanup: drop the case bucket (create/cleanup pairing) through this
# server with its own scope region.
r30_req us-east-1 DELETE "/$R30_BKT"
assert_eq 'cleanup: case bucket removed' 204 "$R30_STATUS"
