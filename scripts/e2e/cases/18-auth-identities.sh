# 18-auth-identities.sh — pluggable-authentication config surface (auth tree
# leaf 06 / GH issue #4): the "identities" + "auth" config keys, e2e:
#   18a. env-pair back-compat: a server launched with ONLY the env
#        credentials (no "identities" key) serves a full object round-trip —
#        byte-identical to pre-tree behavior.
#   18b. multi-identity: env pair + configured identities ("ak-rw" wildcard
#        readwrite, "ak-ro" readonly on the case bucket) coexist; both keys
#        authenticate; env identity still works alongside them.
#   18c. grant enforcement over the wire: the readonly key's PUT is rejected
#        with 403 AccessDenied while its GET succeeds.
#   18d. unknown access key: an unregistered key is rejected with 403
#        InvalidAccessKeyId (the pre-tree error is unchanged).
#   18e. dev mode loudness: auth.mode "none" accepts an UNSIGNED request and
#        logs the "AUTHENTICATION DISABLED" startup banner plus a per-request
#        WARNING line.
#   18f. fail-loud duplicates: a duplicate accessKey across identities aborts
#        startup naming the offender (launch_expect_fail helper).
#
# Private-server pattern (cases 14–16): the suite server is left untouched,
# so the harness's case-boundary relaunch logic keeps working for later cases.
set -u
A18_ROOT=$(mktemp -d /tmp/e2e18-auth.XXXXXX)
A18_WORK=$(mktemp -d /tmp/e2e18-work.XXXXXX)
A18_CERT=$(mktemp -d /tmp/e2e18-cert.XXXXXX)
A18_BKT='e2e18-auth-bkt'
A18_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
openssl req -x509 -newkey rsa:2048 -keyout "$A18_CERT/key.pem" -out "$A18_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
mkdir -p "$A18_WORK/data"

a18_cleanup() {
	if [ -n "${A18_PID:-}" ] && kill -0 "$A18_PID" 2>/dev/null; then
		kill -TERM "$A18_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$A18_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$A18_PID" 2>/dev/null
	fi
	[ -n "${A18_FAILOG:-}" ] && rm -f "$A18_FAILOG"
	rm -rf "$A18_ROOT" "$A18_WORK" "$A18_CERT"
}
trap a18_cleanup EXIT

# a18_start_server <config.json> <logfile> <access_key> <secret_key> — launch
# a private server whose ENV PAIR is <access_key>/<secret_key> (the server
# resolves its env-pair identity from ZETAOBJECT_ACCESS_KEY/SECRET_KEY, like
# any real deployment; AWS_* creds for the aws CLI/curl are exported by the
# caller) and wait for its port.
a18_start_server() {
	local cfg=$1 logf=$2 ak=$3 sk=$4
	ZETAOBJECT_ACCESS_KEY="$ak" ZETAOBJECT_SECRET_KEY="$sk" \
		ZETAOBJECT_CONFIG="$cfg" ./zeta-object-server >"$logf" 2>&1 &
	A18_PID=$!
	ENDPOINT="https://127.0.0.1:$A18_PORT"
	BASE_URL="$ENDPOINT"
	export E2E_ENDPOINT="$ENDPOINT"
	if ! wait_for_port 127.0.0.1 "$A18_PORT" 15; then
		echo '  (auth-identities server did not start — failing case)'
		E2E_FAIL=$((E2E_FAIL + 1))
		exit 0
	fi
}

# a18_stop_server — TERM-then-KILL the current private server (case 15 style).
a18_stop_server() {
	kill -TERM "$A18_PID" 2>/dev/null
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		kill -0 "$A18_PID" 2>/dev/null || break
		sleep 0.5
	done
	kill -9 "$A18_PID" 2>/dev/null
	wait "$A18_PID" 2>/dev/null
	A18_PID=''
}

# --- 18a: env-pair back-compat (no "identities" key) -------------------------
cat > "$A18_WORK/config-env.json" <<EOF
{
  "dataDir": "$A18_WORK/data",
  "listenAddr": "127.0.0.1:$A18_PORT",
  "certFile": "$A18_CERT/cert.pem",
  "keyFile": "$A18_CERT/key.pem"
}
EOF
: > "$A18_WORK/env.log"
export AWS_ACCESS_KEY_ID=e2e18-env-ak AWS_SECRET_ACCESS_KEY=e2e18-env-sk-not-real
export AWS_DEFAULT_REGION=us-east-1 AWS_EC2_METADATA_DISABLED=true
a18_start_server "$A18_WORK/config-env.json" "$A18_WORK/env.log" \
	"$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY"

aws_ok '18a env-only: CreateBucket with env pair' s3api create-bucket --bucket "$A18_BKT"
printf 'case18-env-roundtrip-body' > "$A18_WORK/obj.txt"
aws_ok '18a env-only: PutObject with env pair' s3api put-object \
	--bucket "$A18_BKT" --key 'case18/obj.txt' --body "$A18_WORK/obj.txt"
aws_ok '18a env-only: GetObject with env pair' s3api get-object \
	--bucket "$A18_BKT" --key 'case18/obj.txt' "$A18_WORK/got.txt"
if cmp -s "$A18_WORK/obj.txt" "$A18_WORK/got.txt"; then
	assert_eq '18a env-only round-trip byte-exact (pre-tree behavior)' same same
else
	assert_eq '18a env-only round-trip byte-exact (pre-tree behavior)' same DIFFER
fi
a18_stop_server

# --- 18b–18d: multi-identity config: env pair + ak-rw + ak-ro ----------------
cat > "$A18_WORK/config-ids.json" <<EOF
{
  "dataDir": "$A18_WORK/data",
  "listenAddr": "127.0.0.1:$A18_PORT",
  "certFile": "$A18_CERT/cert.pem",
  "keyFile": "$A18_CERT/key.pem",
  "identities": [
    {
      "name": "ci-rw",
      "accessKey": "e2e18-ak-rw",
      "secretKey": "e2e18-sk-rw-not-real",
      "grants": {"*": "readwrite"}
    },
    {
      "name": "ci-ro",
      "accessKey": "e2e18-ak-ro",
      "secretKey": "e2e18-sk-ro-not-real",
      "grants": {"$A18_BKT": "readonly"}
    }
  ]
}
EOF
: > "$A18_WORK/ids.log"
# The env-pair identity rides in the server env; ak-rw/ak-ro come from the
# config identities array. The env pair's grants stay wildcard readwrite.
a18_start_server "$A18_WORK/config-ids.json" "$A18_WORK/ids.log" \
	e2e18-env-ak e2e18-env-sk-not-real

# 18b: every configured identity authenticates; env identity still works.
export AWS_ACCESS_KEY_ID=e2e18-ak-rw AWS_SECRET_ACCESS_KEY=e2e18-sk-rw-not-real
aws_ok '18b ak-rw (wildcard readwrite): PutObject' s3api put-object \
	--bucket "$A18_BKT" --key 'case18/rw.txt' --body "$A18_WORK/obj.txt"
aws_ok '18b ak-rw (wildcard readwrite): GetObject' s3api get-object \
	--bucket "$A18_BKT" --key 'case18/rw.txt' "$A18_WORK/got-rw.txt"

export AWS_ACCESS_KEY_ID=e2e18-env-ak AWS_SECRET_ACCESS_KEY=e2e18-env-sk-not-real
aws_ok '18b env pair still authenticates alongside identities' s3api list-objects-v2 \
	--bucket "$A18_BKT"

# 18c: grant enforcement — readonly key reads fine, write is 403 AccessDenied.
export AWS_ACCESS_KEY_ID=e2e18-ak-ro AWS_SECRET_ACCESS_KEY=e2e18-sk-ro-not-real
s3req GET "/$A18_BKT/case18/rw.txt"
assert_eq '18c ak-ro (readonly): GetObject allowed' 200 "$S3_STATUS"
s3req PUT "/$A18_BKT/case18/ro-denied.txt" --data-binary 'denied-body'
assert_eq '18c ak-ro (readonly): PutObject rejected' 403 "$S3_STATUS"
assert_s3code '18c readonly denial returns AccessDenied' 'AccessDenied'

# 18d: an unregistered key is rejected with the unchanged pre-tree error.
export AWS_ACCESS_KEY_ID=e2e18-ak-unknown AWS_SECRET_ACCESS_KEY=e2e18-sk-unknown
s3req GET "/$A18_BKT/case18/rw.txt"
assert_eq '18d unknown access key rejected' 403 "$S3_STATUS"
assert_s3code '18d unknown key error is InvalidAccessKeyId (unchanged)' 'InvalidAccessKeyId'
a18_stop_server

# --- 18e: dev mode (auth.mode "none") is loud and accepts unsigned requests --
cat > "$A18_WORK/config-dev.json" <<EOF
{
  "dataDir": "$A18_WORK/data",
  "listenAddr": "127.0.0.1:$A18_PORT",
  "certFile": "$A18_CERT/cert.pem",
  "keyFile": "$A18_CERT/key.pem",
  "auth": {"mode": "none"}
}
EOF
: > "$A18_WORK/dev.log"
a18_start_server "$A18_WORK/config-dev.json" "$A18_WORK/dev.log" \
	e2e18-env-ak e2e18-env-sk-not-real

# Unsigned request (no SigV4, no credentials) must succeed in dev mode.
DEV_STATUS=$(curl -sk -o "$A18_WORK/dev-body.xml" -w '%{http_code}' \
	-X GET "$BASE_URL/$A18_BKT/case18/rw.txt" 2>/dev/null)
assert_eq '18e dev mode: unsigned GetObject accepted' 200 "$DEV_STATUS"
DEV_STATUS=$(curl -sk -o /dev/null -w '%{http_code}' \
	-X PUT "$BASE_URL/$A18_BKT/case18/dev-upload.txt" \
	--data-binary 'dev-mode-body' 2>/dev/null)
assert_eq '18e dev mode: unsigned PutObject accepted' 200 "$DEV_STATUS"

if grep -q 'AUTHENTICATION DISABLED' "$A18_WORK/dev.log"; then
	assert_eq '18e dev mode: loud startup banner present' 0 0
else
	assert_eq '18e dev mode: loud startup banner present' 0 1
fi
if grep -q 'WARNING: dev mode (auth.mode=none): accepting request' "$A18_WORK/dev.log"; then
	assert_eq '18e dev mode: per-request WARNING line present' 0 0
else
	assert_eq '18e dev mode: per-request WARNING line present' 0 1
fi
a18_stop_server

# --- 18f: duplicate accessKey across identities aborts startup (fail-loud) ---
A18_FAILOG="$A18_WORK/fail.log"
cat > "$A18_WORK/config-dup.json" <<EOF
{
  "dataDir": "$A18_WORK/data",
  "listenAddr": "127.0.0.1:$A18_PORT",
  "certFile": "$A18_CERT/cert.pem",
  "keyFile": "$A18_CERT/key.pem",
  "identities": [
    {"name": "dup-one", "accessKey": "e2e18-ak-dup", "secretKey": "e2e18-sk-dup-one"},
    {"name": "dup-two", "accessKey": "e2e18-ak-dup", "secretKey": "e2e18-sk-dup-two"}
  ]
}
EOF
launch_expect_fail "$A18_WORK/config-dup.json" "$A18_FAILOG" 10
assert_eq '18f duplicate accessKey aborts startup (process exits)' 0 "$FAILSTART_EXIT"
assert_eq '18f server exits non-zero on duplicate accessKey' 1 "$(( FAILSTART_RC > 0 ? 1 : 0 ))"
assert_contains '18f startup log names the duplicate access key identity' "$(cat "$A18_FAILOG")" 'configured more than once'

# Restore the suite credentials so later cases keep using the harness pair.
export AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin
