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
#   18g. per-bucket grant beats wildcard (bughunt A1 semantics over the
#        wire): wildcard-readwrite + one-bucket-readonly identity → PUT into
#        the readonly bucket is 403 AccessDenied while its GET works.
#   18h. CopyObject source-grant enforcement (bughunt S1): an identity may
#        write the destination bucket but holds no grant on the copy-source
#        bucket → 403 AccessDenied and no destination object appears; the
#        fully-granted identity's copy still succeeds.
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

# --- 18g–18h: per-bucket-beats-wildcard grants + CopyObject source checks ----
# Fresh private server: same shared dataDir (buckets already seeded above);
# identities carry the bughunt-relevant grant shapes:
#   e2e18-ak-mixed: wildcard readwrite PLUS readonly on the case bucket
#                   (A1: the per-bucket readonly must override "*" write)
#   e2e18-ak-dst  : readwrite on the copy destination bucket ONLY (S1: no
#                   grant on the copy source → CopyObject must 403)
A18_DST_BKT='e2e18-auth-dst-bkt'
cat > "$A18_WORK/config-mixed.json" <<EOF
{
  "dataDir": "$A18_WORK/data",
  "listenAddr": "127.0.0.1:$A18_PORT",
  "certFile": "$A18_CERT/cert.pem",
  "keyFile": "$A18_CERT/key.pem",
  "identities": [
    {
      "name": "ci-mixed",
      "accessKey": "e2e18-ak-mixed",
      "secretKey": "e2e18-sk-mixed-not-real",
      "grants": {"*": "readwrite", "$A18_BKT": "readonly"}
    },
    {
      "name": "ci-dst",
      "accessKey": "e2e18-ak-dst",
      "secretKey": "e2e18-sk-dst-not-real",
      "grants": {"$A18_DST_BKT": "readwrite"}
    }
  ]
}
EOF
: > "$A18_WORK/mixed.log"
a18_start_server "$A18_WORK/config-mixed.json" "$A18_WORK/mixed.log" \
	e2e18-env-ak e2e18-env-sk-not-real

# Seed: the destination bucket for the copy-denial scenario (env pair,
# wildcard readwrite). The mixed server's env identity has full access.
export AWS_ACCESS_KEY_ID=e2e18-env-ak AWS_SECRET_ACCESS_KEY=e2e18-env-sk-not-real
aws_ok '18g seed: CreateBucket copy-destination bucket' s3api create-bucket --bucket "$A18_DST_BKT"
aws_ok '18g seed: PutObject into copy-destination bucket' s3api put-object \
	--bucket "$A18_DST_BKT" --key 'seed/dst-payload.txt' --body "$A18_WORK/obj.txt"
aws_ok '18g seed: ensure source object exists in case bucket' s3api put-object \
	--bucket "$A18_BKT" --key 'case18/copy-src.txt' --body "$A18_WORK/obj.txt"

# 18g: per-bucket readonly overrides wildcard readwrite (A1 over the wire).
# The mixed identity's PUT into $A18_BKT must be denied even though its "*"
# grant says readwrite; its GET of the same bucket must still work.
export AWS_ACCESS_KEY_ID=e2e18-ak-mixed AWS_SECRET_ACCESS_KEY=e2e18-sk-mixed-not-real
s3req GET "/$A18_BKT/case18/copy-src.txt"
assert_eq '18g mixed identity: GET readonly bucket allowed' 200 "$S3_STATUS"
s3req PUT "/$A18_BKT/case18/mixed-write-denied.txt" --data-binary 'must-not-land'
assert_eq '18g mixed identity: per-bucket readonly overrides wildcard write' 403 "$S3_STATUS"
assert_s3code '18g mixed identity denial returns AccessDenied' 'AccessDenied'

# 18h: CopyObject source-grant enforcement (S1 over the wire). The dst-only
# identity may write $A18_DST_BKT but holds NO grant on $A18_BKT (the copy
# source) → 403 AccessDenied, and no destination object may appear. The env
# identity (wildcard) then proves a granted copy still succeeds.
export AWS_ACCESS_KEY_ID=e2e18-ak-dst AWS_SECRET_ACCESS_KEY=e2e18-sk-dst-not-real
s3req PUT "/$A18_DST_BKT/stolen.txt" \
	--header "x-amz-copy-source: $A18_BKT/case18/copy-src.txt"
assert_eq '18h dst-only identity: copy from ungranted source rejected' 403 "$S3_STATUS"
assert_s3code '18h copy-source denial returns AccessDenied' 'AccessDenied'
s3req GET "/$A18_DST_BKT/stolen.txt"
assert_eq '18h denied copy must not create the destination object' 404 "$S3_STATUS"

export AWS_ACCESS_KEY_ID=e2e18-env-ak AWS_SECRET_ACCESS_KEY=e2e18-env-sk-not-real
s3req PUT "/$A18_DST_BKT/copied-ok.txt" \
	--header "x-amz-copy-source: $A18_BKT/case18/copy-src.txt"
assert_eq '18h granted identity: copy within grants still succeeds' 200 "$S3_STATUS"
s3req GET "/$A18_DST_BKT/copied-ok.txt"
assert_eq '18h granted copy: destination object readable' 200 "$S3_STATUS"
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
export AWS_ACCESS_KEY_ID=zetaadmin AWS_SECRET_ACCESS_KEY=zetaadmin
