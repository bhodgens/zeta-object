# 27-rich-grants.sh — rich grant expressions (auth extensions leaf 09):
#   27a. legacy coexistence: one config carries a legacy string grant and a
#        rich object grant; both identities behave per v1 / rich semantics.
#   27b. prefix scoping: "photos/2024/*" — PUT under the prefix succeeds,
#        PUT outside it → 403 AccessDenied, GET under it succeeds.
#   27c. op scoping: read+list-only identity — GET succeeds, DELETE → 403.
#   27d. time windows (wide margins, wall clock): not-after in the past →
#        403; not-before in the future → 403; window containing now → 200.
#   27e. fail-loud validation: unknown op, not-after <= not-before, empty
#        ops, unknown JSON key → launch_expect_fail naming the offender.
#   27f. SFTP floor round-trip: an SFTP login for a rich-grant identity
#        authenticates and the session enforces the floor at minimum
#        (extends case 21's pattern); CriticalOptions carry only the floor.
#
# Private-server pattern (cases 14–21): the suite server is left untouched.
set -u
E27_ROOT=$(mktemp -d /tmp/e2e27-rich.XXXXXX)
E27_WORK=$(mktemp -d /tmp/e2e27-work.XXXXXX)
E27_CERT=$(mktemp -d /tmp/e2e27-cert.XXXXXX)
E27_BKT='e2e27-rich-bkt'
E27_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
E27_S3_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
openssl req -x509 -newkey rsa:2048 -keyout "$E27_CERT/key.pem" -out "$E27_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
mkdir -p "$E27_WORK/data" "$E27_WORK/keys"

E27_SFTP_USER='e2e27-sftp'
E27_SFTP_PASS='e2e27-sftp-pass'

e27_cleanup() {
	if [ -n "${E27_PID:-}" ] && kill -0 "$E27_PID" 2>/dev/null; then
		kill -TERM "$E27_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$E27_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$E27_PID" 2>/dev/null
	fi
	[ -n "${E27_FAILOG:-}" ] && rm -f "$E27_FAILOG"
	rm -rf "$E27_ROOT" "$E27_WORK" "$E27_CERT"
}
trap e27_cleanup EXIT

# e27_start_server <config.json> <logfile> — launch a private server (env
# pair = the seed/wildcard identity) and wait for its S3 port.
e27_start_server() {
	local cfg=$1 logf=$2
	ZETAOBJECT_ACCESS_KEY=e2e27-env-ak ZETAOBJECT_SECRET_KEY=e2e27-env-sk \
		ZETAOBJECT_CONFIG="$cfg" ./zeta-object-server >"$logf" 2>&1 &
	E27_PID=$!
	ENDPOINT="https://127.0.0.1:$E27_S3_PORT"
	BASE_URL="$ENDPOINT"
	export E2E_ENDPOINT="$ENDPOINT"
	if ! wait_for_port 127.0.0.1 "$E27_S3_PORT" 15; then
		echo '  (rich-grants server did not start — failing case)'
		E2E_FAIL=$((E2E_FAIL + 1))
		exit 0
	fi
}

# e27_stop_server — TERM-then-KILL the current private server.
e27_stop_server() {
	kill -TERM "$E27_PID" 2>/dev/null
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		kill -0 "$E27_PID" 2>/dev/null || break
		sleep 0.5
	done
	kill -9 "$E27_PID" 2>/dev/null
	wait "$E27_PID" 2>/dev/null
	E27_PID=''
}

# e27_as <access_key> <secret_key> — point the s3req/aws credentials at an
# identity.
e27_as() {
	export AWS_ACCESS_KEY_ID=$1 AWS_SECRET_ACCESS_KEY=$2
	export AWS_DEFAULT_REGION=us-east-1 AWS_EC2_METADATA_DISABLED=true
}

printf 'e2e27-payload-body\n' > "$E27_WORK/payload.txt"

# --- 27a: legacy coexistence --------------------------------------------------
# One config: a legacy string grant identity (v1 semantics) and a rich
# object-grant identity (prefix-scoped). Both coexist with the env pair.
cat > "$E27_WORK/config-27a.json" <<EOF
{
  "dataDir": "$E27_WORK/data",
  "listenAddr": "127.0.0.1:$E27_S3_PORT",
  "certFile": "$E27_CERT/cert.pem",
  "keyFile": "$E27_CERT/key.pem",
  "identities": [
    {
      "name": "legacy-rw",
      "accessKey": "e2e27-ak-legacy",
      "secretKey": "e2e27-sk-legacy",
      "grants": {"*": "readwrite"}
    },
    {
      "name": "rich-prefix",
      "accessKey": "e2e27-ak-rich",
      "secretKey": "e2e27-sk-rich",
      "grants": {"$E27_BKT/2024/*": {"ops": ["read", "write"]}}
    }
  ]
}
EOF
: > "$E27_WORK/a.log"
e27_start_server "$E27_WORK/config-27a.json" "$E27_WORK/a.log"

# Seed the bucket + a base object via the env identity (wildcard).
e27_as e2e27-env-ak e2e27-env-sk
aws_ok '27a seed: CreateBucket' s3api create-bucket --bucket "$E27_BKT"
aws_ok '27a seed: PutObject outside prefix' s3api put-object \
	--bucket "$E27_BKT" --key 'other/base.txt' --body "$E27_WORK/payload.txt"

# The LEGACY identity behaves exactly per v1: unrestricted within its grants.
e27_as e2e27-ak-legacy e2e27-sk-legacy
aws_ok '27a legacy identity: PutObject anywhere' s3api put-object \
	--bucket "$E27_BKT" --key 'legacy/obj.txt' --body "$E27_WORK/payload.txt"

# The RICH identity: PUT under the prefix succeeds; outside it is denied.
e27_as e2e27-ak-rich e2e27-sk-rich
aws_ok '27a rich identity: PutObject under prefix' s3api put-object \
	--bucket "$E27_BKT" --key '2024/a.jpg' --body "$E27_WORK/payload.txt"
assert_status '27a rich identity: PutObject outside prefix denied' 403 PUT \
	"/$E27_BKT/other/outside.txt" --data-binary 'denied'
assert_s3code '27a prefix denial returns AccessDenied' 'AccessDenied'
e27_stop_server

# --- 27b: prefix scoping (fresh server, dedicated identity) -------------------
cat > "$E27_WORK/config-27b.json" <<EOF
{
  "dataDir": "$E27_WORK/data",
  "listenAddr": "127.0.0.1:$E27_S3_PORT",
  "certFile": "$E27_CERT/cert.pem",
  "keyFile": "$E27_CERT/key.pem",
  "identities": [
    {
      "name": "ci-2024",
      "accessKey": "e2e27-ak-ci24",
      "secretKey": "e2e27-sk-ci24",
      "grants": {"$E27_BKT/2024/*": {"ops": ["read", "write"]}}
    }
  ]
}
EOF
: > "$E27_WORK/b.log"
e27_start_server "$E27_WORK/config-27b.json" "$E27_WORK/b.log"
e27_as e2e27-ak-ci24 e2e27-sk-ci24
aws_ok '27b PUT photos/2024-ish under prefix' s3api put-object \
	--bucket "$E27_BKT" --key '2024/a.jpg' --body "$E27_WORK/payload.txt"
assert_status '27b boundary: 20240/ is NOT under 2024/*' 403 PUT \
	"/$E27_BKT/20240/x.jpg" --data-binary 'denied'
assert_status '27b boundary: bare 2024 key not under 2024/*' 403 PUT \
	"/$E27_BKT/2024" --data-binary 'denied'
assert_status '27b other year denied' 403 PUT \
	"/$E27_BKT/2023/x.jpg" --data-binary 'denied'
aws_ok '27b GET under prefix succeeds' s3api get-object \
	--bucket "$E27_BKT" --key '2024/a.jpg' "$E27_WORK/got.txt"
e27_stop_server

# --- 27c: op scoping (read+list only) ------------------------------------------
cat > "$E27_WORK/config-27c.json" <<EOF
{
  "dataDir": "$E27_WORK/data",
  "listenAddr": "127.0.0.1:$E27_S3_PORT",
  "certFile": "$E27_CERT/cert.pem",
  "keyFile": "$E27_CERT/key.pem",
  "identities": [
    {
      "name": "auditor",
      "accessKey": "e2e27-ak-audit",
      "secretKey": "e2e27-sk-audit",
      "grants": {"$E27_BKT/*": {"ops": ["read", "list"]}}
    }
  ]
}
EOF
: > "$E27_WORK/c.log"
e27_start_server "$E27_WORK/config-27c.json" "$E27_WORK/c.log"
e27_as e2e27-ak-audit e2e27-sk-audit
aws_ok '27c read-only-op identity: GetObject succeeds' s3api get-object \
	--bucket "$E27_BKT" --key '2024/a.jpg' "$E27_WORK/audit.txt"
assert_status '27c read-only-op identity: DeleteObject denied' 403 DELETE \
	"/$E27_BKT/2024/a.jpg"
assert_s3code '27c delete denial returns AccessDenied' 'AccessDenied'
assert_status '27c read-only-op identity: PutObject denied' 403 PUT \
	"/$E27_BKT/2024/new.txt" --data-binary 'denied'
e27_stop_server

# --- 27d: time windows (wide margins, wall clock) -------------------------------
# past-window (not-after 2026-01-01 → expired), future-window
# (not-before 2099-01-01 → not yet active), live-window (2015→2065 → open).
cat > "$E27_WORK/config-27d.json" <<EOF
{
  "dataDir": "$E27_WORK/data",
  "listenAddr": "127.0.0.1:$E27_S3_PORT",
  "certFile": "$E27_CERT/cert.pem",
  "keyFile": "$E27_CERT/key.pem",
  "identities": [
    {
      "name": "expired-key",
      "accessKey": "e2e27-ak-expired",
      "secretKey": "e2e27-sk-expired",
      "grants": {"$E27_BKT/*": {"ops": ["read"], "not-after": "2026-01-01T00:00:00Z"}}
    },
    {
      "name": "staged-key",
      "accessKey": "e2e27-ak-staged",
      "secretKey": "e2e27-sk-staged",
      "grants": {"$E27_BKT/*": {"ops": ["read"], "not-before": "2099-01-01T00:00:00Z"}}
    },
    {
      "name": "live-key",
      "accessKey": "e2e27-ak-live",
      "secretKey": "e2e27-sk-live",
      "grants": {"$E27_BKT/*": {"ops": ["read"], "not-before": "2015-01-01T00:00:00Z", "not-after": "2065-01-01T00:00:00Z"}}
    }
  ]
}
EOF
: > "$E27_WORK/d.log"
e27_start_server "$E27_WORK/config-27d.json" "$E27_WORK/d.log"
e27_as e2e27-ak-expired e2e27-sk-expired
assert_status '27d expired not-after: GetObject denied' 403 GET "/$E27_BKT/2024/a.jpg"
e27_as e2e27-ak-staged e2e27-sk-staged
assert_status '27d future not-before: GetObject denied' 403 GET "/$E27_BKT/2024/a.jpg"
e27_as e2e27-ak-live e2e27-sk-live
aws_ok '27d live window containing now: GetObject succeeds' s3api get-object \
	--bucket "$E27_BKT" --key '2024/a.jpg' "$E27_WORK/live.txt"
e27_stop_server

# --- 27e: fail-loud validation ---------------------------------------------------
E27_FAILOG="$E27_WORK/fail.log"
# 27e-1: unknown op
cat > "$E27_WORK/config-bad-op.json" <<EOF
{
  "dataDir": "$E27_WORK/data",
  "listenAddr": "127.0.0.1:$E27_S3_PORT",
  "certFile": "$E27_CERT/cert.pem",
  "keyFile": "$E27_CERT/key.pem",
  "identities": [
    {"name": "bad-op", "accessKey": "e2e27-ak-bad", "secretKey": "e2e27-sk-bad",
     "grants": {"$E27_BKT/*": {"ops": ["admin"]}}}
  ]
}
EOF
launch_expect_fail "$E27_WORK/config-bad-op.json" "$E27_FAILOG" 10
assert_eq '27e unknown op aborts startup' 0 "$FAILSTART_EXIT"
assert_contains '27e unknown op named in the log' "$(cat "$E27_FAILOG")" 'unknown op'

# 27e-2: not-after <= not-before
cat > "$E27_WORK/config-bad-window.json" <<EOF
{
  "dataDir": "$E27_WORK/data",
  "listenAddr": "127.0.0.1:$E27_S3_PORT",
  "certFile": "$E27_CERT/cert.pem",
  "keyFile": "$E27_CERT/key.pem",
  "identities": [
    {"name": "bad-window", "accessKey": "e2e27-ak-bad", "secretKey": "e2e27-sk-bad",
     "grants": {"$E27_BKT/*": {"ops": ["read"], "not-before": "2026-10-02T00:00:00Z", "not-after": "2026-10-01T00:00:00Z"}}}
  ]
}
EOF
launch_expect_fail "$E27_WORK/config-bad-window.json" "$E27_FAILOG" 10
assert_eq '27e inverted window aborts startup' 0 "$FAILSTART_EXIT"
assert_contains '27e inverted window named in the log' "$(cat "$E27_FAILOG")" 'must be after'

# 27e-3: empty ops
cat > "$E27_WORK/config-bad-empty.json" <<EOF
{
  "dataDir": "$E27_WORK/data",
  "listenAddr": "127.0.0.1:$E27_S3_PORT",
  "certFile": "$E27_CERT/cert.pem",
  "keyFile": "$E27_CERT/key.pem",
  "identities": [
    {"name": "bad-empty", "accessKey": "e2e27-ak-bad", "secretKey": "e2e27-sk-bad",
     "grants": {"$E27_BKT/*": {"ops": []}}}
  ]
}
EOF
launch_expect_fail "$E27_WORK/config-bad-empty.json" "$E27_FAILOG" 10
assert_eq '27e empty ops aborts startup' 0 "$FAILSTART_EXIT"
assert_contains '27e empty ops named in the log' "$(cat "$E27_FAILOG")" 'non-empty'

# 27e-4: unknown JSON key
cat > "$E27_WORK/config-bad-key.json" <<EOF
{
  "dataDir": "$E27_WORK/data",
  "listenAddr": "127.0.0.1:$E27_S3_PORT",
  "certFile": "$E27_CERT/cert.pem",
  "keyFile": "$E27_CERT/key.pem",
  "identities": [
    {"name": "bad-key", "accessKey": "e2e27-ak-bad", "secretKey": "e2e27-sk-bad",
     "grants": {"$E27_BKT/*": {"ops": ["read"], "prifix": "x"}}}
  ]
}
EOF
launch_expect_fail "$E27_WORK/config-bad-key.json" "$E27_FAILOG" 10
assert_eq '27e unknown JSON key aborts startup' 0 "$FAILSTART_EXIT"

# --- 27f: SFTP floor round-trip ---------------------------------------------------
# The rich identity logs in over SFTP; the CriticalOptions carry only the
# floor, so the session must AT MINIMUM enforce the floor (write under the
# prefix's bucket works because the unbounded rich entry unions write into
# the floor; the session re-resolves rich grants from the registry at
# session start).
ssh-keygen -t ed25519 -N '' -f "$E27_WORK/keys/id_ed25519" -C 'e2e27' >/dev/null 2>&1
E27_PUBLINE=$(cat "$E27_WORK/keys/id_ed25519.pub")
cat > "$E27_WORK/config-27f.json" <<EOF
{
  "dataDir": "$E27_WORK/data",
  "listenAddr": "127.0.0.1:$E27_S3_PORT",
  "certFile": "$E27_CERT/cert.pem",
  "keyFile": "$E27_CERT/key.pem",
  "frontends": [
    {"type": "s3"},
    {"type": "sftp", "listenAddr": "127.0.0.1:$E27_PORT",
     "options": {"hostKeyFile": "$E27_WORK/host_ed25519"}}
  ],
  "identities": [
    {
      "name": "sftp-rich",
      "accessKey": "$E27_SFTP_USER",
      "secretKey": "$E27_SFTP_PASS",
      "grants": {"$E27_BKT/2024/*": {"ops": ["read", "write", "list"]}},
      "sshPublicKeys": ["$E27_PUBLINE"]
    }
  ]
}
EOF
: > "$E27_WORK/f.log"
e27_start_server "$E27_WORK/config-27f.json" "$E27_WORK/f.log"

E27_SSHOPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes -o LogLevel=ERROR"
# Seed a base object inside the prefix via S3 first (the env identity).
e27_as e2e27-env-ak e2e27-env-sk
aws_ok '27f seed: PutObject under prefix via S3' s3api put-object \
	--bucket "$E27_BKT" --key '2024/sftp-seed.txt' --body "$E27_WORK/payload.txt"

cat > "$E27_WORK/batch-27f" <<EOF
get /$E27_BKT/2024/sftp-seed.txt "$E27_WORK/sftp-got.txt"
EOF
sftp $E27_SSHOPTS -i "$E27_WORK/keys/id_ed25519" -P "$E27_PORT" \
	-b "$E27_WORK/batch-27f" "$E27_SFTP_USER@127.0.0.1" >/dev/null 2>&1
E27_SFTP_RC=$?
assert_eq '27f SFTP rich identity authenticates + reads floor-readable object' 0 "$E27_SFTP_RC"
if [ -f "$E27_WORK/sftp-got.txt" ]; then
	assert_eq '27f SFTP round-trip byte-identical' "$(cat "$E27_WORK/payload.txt")" "$(cat "$E27_WORK/sftp-got.txt")"
else
	assert_contains '27f SFTP get produced the file' 'missing-sftp-got.txt' 'present'
fi
e27_stop_server

# Restore the suite credentials so later cases keep using the harness pair.
export AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin
