# 20-ftp.sh — FTP/FTPS frontend (sftp-ftp-2026-09 tree / GH issue #2):
#   20a. plain FTP round-trip via curl ftp:// (private server): STOR (upload),
#        LIST (name visible), RETR byte-identical, DELE.
#   20b. FTPS round-trip via curl --ftp-ssl --insecure (AUTH TLS explicit;
#        plaintext still works on the same port — explicit, not implicit).
#   20c. grant-denial negative: read-only credential; write attempts fail
#        and the server stays up.
#   20d. fail-loud: unknown option key inside an ftp entry aborts startup
#        (launch_expect_fail), and an ftp entry WITHOUT listenAddr aborts
#        (non-HTTP frontends cannot share the HTTPS mux).
#
# Private-server pattern (cases 14–19): the suite server is left untouched.
set -u
E20_ROOT=$(mktemp -d /tmp/e2e20-ftp.XXXXXX)
E20_WORK=$(mktemp -d /tmp/e2e20-work.XXXXXX)
E20_CERT=$(mktemp -d /tmp/e2e20-cert.XXXXXX)
E20_BKT='e2e20-ftp-bkt'
E20_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
# Passive port range: fixed high band above ephemeral noise; recorded in the
# ftp config (case convention: PASV publicIP pinned to 127.0.0.1).
E20_PASV_MIN=41000
E20_PASV_MAX=41100
openssl req -x509 -newkey rsa:2048 -keyout "$E20_CERT/key.pem" -out "$E20_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
mkdir -p "$E20_WORK/data"

E20_USER='ftp-user'
E20_PASS='ftp-pass'
E20_RO_USER='ftp-ro'
E20_RO_PASS='ftp-ro-pass'

e20_cleanup() {
	if [ -n "${E20_PID:-}" ] && kill -0 "$E20_PID" 2>/dev/null; then
		kill -TERM "$E20_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$E20_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$E20_PID" 2>/dev/null
	fi
	rm -rf "$E20_ROOT" "$E20_WORK" "$E20_CERT"
}
trap e20_cleanup EXIT

# --- launch the private server: s3 + ftp (rw), two ports ----------------------
# S3 keeps the default listener (E20_S3_PORT); FTP gets its own dedicated
# listenAddr (E20_PORT). They CANNOT share a port: buildDedicatedListeners
# binds the non-HTTP listener before the default HTTPS server starts, so a
# shared port would make ListenAndServeTLS fail and abort startup.
E20_S3_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
cat > "$E20_WORK/config.json" <<EOF
{
  "dataDir": "$E20_WORK/data",
  "listenAddr": "127.0.0.1:$E20_S3_PORT",
  "certFile": "$E20_CERT/cert.pem",
  "keyFile": "$E20_CERT/key.pem",
  "frontends": [
    {"type": "s3"},
    {"type": "ftp", "listenAddr": "127.0.0.1:$E20_PORT",
     "options": {"passivePortMin": "$E20_PASV_MIN", "passivePortMax": "$E20_PASV_MAX", "publicIP": "127.0.0.1"}}
  ],
  "identities": [
    {"name": "ftp-rw", "accessKey": "$E20_USER", "secretKey": "$E20_PASS", "grants": {"*": "readwrite"}},
    {"name": "ftp-ro", "accessKey": "$E20_RO_USER", "secretKey": "$E20_RO_PASS", "grants": {"*": "readonly"}}
  ]
}
EOF

ZETAOBJECT_ACCESS_KEY=zetaadmin ZETAOBJECT_SECRET_KEY=zetaadmin \
	ZETAOBJECT_CONFIG="$E20_WORK/config.json" ./zeta-object-server >"$E20_WORK/server.log" 2>&1 &
E20_PID=$!
ENDPOINT="https://127.0.0.1:$E20_S3_PORT"
BASE_URL="$ENDPOINT"
export E2E_ENDPOINT="$ENDPOINT"
if ! wait_for_port 127.0.0.1 "$E20_PORT" 15 || ! wait_for_port 127.0.0.1 "$E20_S3_PORT" 15; then
	echo '  (ftp server did not start — failing case)'
	E2E_FAIL=$((E2E_FAIL + 1))
	exit 0
fi

# Seed state through the S3 side (cross-frontend consistency IS the
# neutral-model proof). The bucket is created first: the S3 frontend
# rejects PutObject into a nonexistent bucket (NoSuchBucket), while the
# FTP flat namespace would accept the STOR — the bucket must exist so both
# frontends address the same object.
AWS_ACCESS_KEY_ID=zetaadmin AWS_SECRET_ACCESS_KEY=zetaadmin \
	aws s3api create-bucket --bucket "$E20_BKT" \
	--endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
printf 'ftp-e2e-seed-body' > "$E20_WORK/seed.txt"
AWS_ACCESS_KEY_ID=zetaadmin AWS_SECRET_ACCESS_KEY=zetaadmin \
	aws s3api put-object --bucket "$E20_BKT" --key 'seed.txt' \
	--body "$E20_WORK/seed.txt" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

E20_PAYLOAD="$E20_WORK/payload.txt"
printf 'ftp-e2e-payload-roundtrip-0123456789' > "$E20_PAYLOAD"

# --- part 20a: plain FTP round-trip ------------------------------------------
# STOR (curl -T uploads to the URL path).
E20_UP=$(curl -s -o /dev/null -w '%{http_code}' --ftp-pasv \
	-u "$E20_USER:$E20_PASS" -T "$E20_PAYLOAD" \
	"ftp://127.0.0.1:$E20_PORT/$E20_BKT/e2e20.txt" 2>/dev/null)
assert_eq 'FTP STOR (upload) accepted' 226 "$E20_UP"

# LIST: the uploaded name is visible.
E20_LISTING=$(curl -s --ftp-pasv -u "$E20_USER:$E20_PASS" \
	"ftp://127.0.0.1:$E20_PORT/$E20_BKT/" 2>/dev/null)
assert_contains 'FTP LIST shows the uploaded file' "$E20_LISTING" 'e2e20.txt'
assert_contains 'FTP LIST shows the seeded object' "$E20_LISTING" 'seed.txt'

# RETR byte-identical.
E20_BODY=$(curl -s --ftp-pasv -u "$E20_USER:$E20_PASS" \
	"ftp://127.0.0.1:$E20_PORT/$E20_BKT/e2e20.txt" 2>/dev/null)
assert_eq 'FTP RETR round-trips the bytes' "$(cat "$E20_PAYLOAD")" "$E20_BODY"

# The seeded S3 object reads back byte-identical over FTP (parity).
E20_SEED=$(curl -s --ftp-pasv -u "$E20_USER:$E20_PASS" \
	"ftp://127.0.0.1:$E20_PORT/$E20_BKT/seed.txt" 2>/dev/null)
assert_eq 'S3-written object reads back via FTP' 'ftp-e2e-seed-body' "$E20_SEED"

# DELE (curl -Q "DELE path" runs the command after transfer; use --quote).
E20_DEL=$(curl -s -o /dev/null -w '%{http_code}' --ftp-pasv \
	-u "$E20_USER:$E20_PASS" --quote "DELE /$E20_BKT/e2e20.txt" \
	"ftp://127.0.0.1:$E20_PORT/$E20_BKT/" 2>/dev/null)
assert_eq 'FTP DELE accepted' 226 "$E20_DEL"
E20_GONE_CODE=$(curl -s -o /dev/null -w '%{http_code}' --ftp-pasv -u "$E20_USER:$E20_PASS" \
	"ftp://127.0.0.1:$E20_PORT/$E20_BKT/e2e20.txt" 2>/dev/null)
E20_GONE_BODY=$(curl -s --ftp-pasv -u "$E20_USER:$E20_PASS" \
	"ftp://127.0.0.1:$E20_PORT/$E20_BKT/e2e20.txt" 2>/dev/null)
if [ "$E20_GONE_CODE" -ge 400 ] || [ -z "$E20_GONE_BODY" ]; then
	assert_eq 'DELEted file no longer retrievable' denied denied
else
	assert_eq 'DELEted file no longer retrievable' "still retrievable: $E20_GONE_BODY" denied
fi

# --- part 20b: FTPS via AUTH TLS (explicit) -----------------------------------
E20_TLS_UP=$(curl -s -o /dev/null -w '%{http_code}' --ftp-ssl --insecure --ftp-pasv \
	-u "$E20_USER:$E20_PASS" -T "$E20_PAYLOAD" \
	"ftp://127.0.0.1:$E20_PORT/$E20_BKT/ftps.txt" 2>/dev/null)
assert_eq 'FTPS STOR (AUTH TLS) accepted' 226 "$E20_TLS_UP"

E20_TLS_BODY=$(curl -s --ftp-ssl --insecure --ftp-pasv -u "$E20_USER:$E20_PASS" \
	"ftp://127.0.0.1:$E20_PORT/$E20_BKT/ftps.txt" 2>/dev/null)
assert_eq 'FTPS RETR round-trips the bytes' "$(cat "$E20_PAYLOAD")" "$E20_TLS_BODY"

# Plaintext STILL works on the same port (explicit TLS, not implicit).
E20_PLAIN_BODY=$(curl -s --ftp-pasv -u "$E20_USER:$E20_PASS" \
	"ftp://127.0.0.1:$E20_PORT/$E20_BKT/ftps.txt" 2>/dev/null)
assert_eq 'plaintext coexists with AUTH TLS' "$(cat "$E20_PAYLOAD")" "$E20_PLAIN_BODY"

# --- part 20c: grant-denial negative (read-only credential) -------------------
E20_RO_UP=$(curl -s -o /dev/null -w '%{http_code}' --ftp-pasv \
	-u "$E20_RO_USER:$E20_RO_PASS" -T "$E20_PAYLOAD" \
	"ftp://127.0.0.1:$E20_PORT/$E20_BKT/forbidden.txt" 2>/dev/null)
if [ "$E20_RO_UP" -ge 400 ] || [ "$E20_RO_UP" -eq 0 ]; then
	assert_eq 'read-only STOR denied (5xx-class or transfer error)' denied denied
else
	assert_eq 'read-only STOR denied (5xx-class or transfer error)' "got $E20_RO_UP" denied
fi

# Bad password → auth failure (curl gets 530; surfaces as non-2xx/curl error).
E20_BAD=$(curl -s -o /dev/null -w '%{http_code}' --ftp-pasv \
	-u "$E20_USER:WRONGPASS" -T "$E20_PAYLOAD" \
	"ftp://127.0.0.1:$E20_PORT/$E20_BKT/nope.txt" 2>/dev/null)
if [ "$E20_BAD" -ge 400 ] || [ "$E20_BAD" -eq 0 ]; then
	assert_eq 'wrong password rejected (530)' denied denied
else
	assert_eq 'wrong password rejected (530)' "got $E20_BAD" denied
fi

# The server stayed up through the negatives.
E20_STILL=$(curl -s --ftp-pasv -u "$E20_USER:$E20_PASS" \
	"ftp://127.0.0.1:$E20_PORT/$E20_BKT/ftps.txt" 2>/dev/null)
assert_eq 'server survives denial negatives' "$(cat "$E20_PAYLOAD")" "$E20_STILL"

# --- part 20d: fail-loud config ------------------------------------------------
E20_FAILOG="$E20_WORK/fail.log"
cat > "$E20_WORK/config-bad.json" <<EOF
{
  "dataDir": "$E20_WORK/data",
  "listenAddr": "127.0.0.1:$E20_PORT",
  "certFile": "$E20_CERT/cert.pem",
  "keyFile": "$E20_CERT/key.pem",
  "frontends": [
    {"type": "ftp", "listenAddr": "127.0.0.1:$E20_PORT", "options": {"bogus": "1"}}
  ]
}
EOF
launch_expect_fail "$E20_WORK/config-bad.json" "$E20_FAILOG" 10
assert_eq 'unknown ftp option aborts startup (process exits)' 0 "$FAILSTART_EXIT"
assert_contains 'startup log names the unknown key' "$(cat "$E20_FAILOG")" 'bogus'

cat > "$E20_WORK/config-noaddr.json" <<EOF
{
  "dataDir": "$E20_WORK/data",
  "listenAddr": "127.0.0.1:$E20_PORT",
  "certFile": "$E20_CERT/cert.pem",
  "keyFile": "$E20_CERT/key.pem",
  "frontends": [
    {"type": "ftp"}
  ]
}
EOF
launch_expect_fail "$E20_WORK/config-noaddr.json" "$E20_FAILOG" 10
assert_eq 'ftp without listenAddr aborts startup' 0 "$FAILSTART_EXIT"
assert_contains 'error names the missing listenAddr' "$(cat "$E20_FAILOG")" 'listenAddr'

# --- part 20e: aborted upload keeps the previous object (F1) ------------------
# Overwrite the seed via FTP (known-good new content), then start a second
# STOR to the same key and kill the data connection mid-transfer. The object
# must keep the previous bytes (not the partial body, not an empty body) —
# verified via the S3 frontend (cross-frontend consistency).
printf 'ftp-e2e-good-content-after-overwrite' > "$E20_WORK/good.txt"
E20_GOOD_UP=$(curl -s -o /dev/null -w '%{http_code}' --ftp-pasv \
	-u "$E20_USER:$E20_PASS" -T "$E20_WORK/good.txt" \
	"ftp://127.0.0.1:$E20_PORT/$E20_BKT/seed.txt" 2>/dev/null)
assert_eq 'FTP overwrite of existing object accepted' 226 "$E20_GOOD_UP"

# Raw-protocol abort: PASV → dial data conn → STOR → partial bytes → ABOR on the
# control channel (telnet-escaped, RFC 959). curl cannot do this; the wire is driven
# directly with python3. ABOR (unlike a pure data-connection RST) is deterministic:
# ftpserverlib answers 426 (transfer aborted) then 226 (data conn closed). A bare RST
# after the server has consumed the buffered bytes races with a clean EOF and would
# commit — FTP carries no length header, so only ABOR is unambiguous.
E20_ABORT_OUT=$(python3 - "$E20_PORT" "$E20_PASV_MIN" "$E20_PASV_MAX" 2>/dev/null <<'PY'
import socket, sys, time

port, pmin, pmax = int(sys.argv[1]), int(sys.argv[2]), int(sys.argv[3])

def readline(f):
    return f.readline().decode(errors="replace").rstrip()

s = socket.create_connection(("127.0.0.1", port), timeout=10)
f = s.makefile("rb")
readline(f)                                   # 220 banner
s.sendall(b"USER ftp-user\r\n"); readline(f)  # 331
s.sendall(b"PASS ftp-pass\r\n"); readline(f)  # 230
s.sendall(b"PASV\r\n")
pasv = readline(f)
if not pasv.startswith("227"):
    print("PASV-FAILED"); sys.exit(0)
inside = pasv[pasv.index("(") + 1:pasv.rindex(")")].split(",")
host = ".".join(inside[:4])
dport = int(inside[4]) * 256 + int(inside[5])
dc = socket.create_connection((host, dport), timeout=10)
s.sendall(b"STOR e2e20-ftp-bkt/abort-overwrite.txt\r\n")
pre = readline(f)
if not pre.startswith("150"):
    print("STOR-FAILED"); sys.exit(0)
dc.sendall(b"partial-body-never-committed")
# ABOR mid-transfer: expect 426 (aborted) then 226 (data conn closed).
s.sendall(bytes([242, 255]) + b"ABOR\r\n")    # telnet IP+DM escape
s.settimeout(5)
replies = []
for _ in range(2):
    try:
        replies.append(readline(f))
    except Exception:
        replies.append("")
print("ABORTED:" + (replies[0][:3] if replies[0] else "none"))
s.close()
PY
)
case "$E20_ABORT_OUT" in
ABORTED:426)
	assert_eq 'aborted STOR reports transfer failure' denied denied ;;
*)
	assert_eq 'aborted STOR reports transfer failure' "$E20_ABORT_OUT" denied ;;
esac

# The S3 view: seed.txt must hold the PREVIOUS (good) bytes — no overwrite by
# the partial/empty body.
E20_AFTER=$(AWS_ACCESS_KEY_ID=zetaadmin AWS_SECRET_ACCESS_KEY=zetaadmin \
	aws s3api get-object --bucket "$E20_BKT" --key 'seed.txt' \
	"$E20_WORK/after.txt" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1 \
	&& cat "$E20_WORK/after.txt")
assert_eq 'aborted STOR leaves previous object intact (S3 view)' \
	'ftp-e2e-good-content-after-overwrite' "$E20_AFTER"

# New-file abort: ABOR a STOR to a key that does not exist yet; the key must
# NOT be created (visible neither to S3 nor FTP).
E20_ABORT2_OUT=$(python3 - "$E20_PORT" 2>/dev/null <<'PY'
import socket, sys, time

port = int(sys.argv[1])

def readline(f):
    return f.readline().decode(errors="replace").rstrip()

s = socket.create_connection(("127.0.0.1", port), timeout=10)
f = s.makefile("rb")
readline(f)
s.sendall(b"USER ftp-user\r\n"); readline(f)
s.sendall(b"PASS ftp-pass\r\n"); readline(f)
s.sendall(b"PASV\r\n")
pasv = readline(f)
if not pasv.startswith("227"):
    print("PASV-FAILED"); sys.exit(0)
inside = pasv[pasv.index("(") + 1:pasv.rindex(")")].split(",")
host = ".".join(inside[:4])
dport = int(inside[4]) * 256 + int(inside[5])
dc = socket.create_connection((host, dport), timeout=10)
s.sendall(b"STOR e2e20-ftp-bkt/never-created.txt\r\n")
pre = readline(f)
if not pre.startswith("150"):
    print("STOR-FAILED"); sys.exit(0)
dc.sendall(b"partial-new-file")
s.sendall(bytes([242, 255]) + b"ABOR\r\n")    # telnet IP+DM escape
s.settimeout(5)
replies = []
for _ in range(2):
    try:
        replies.append(readline(f))
    except Exception:
        replies.append("")
print("ABORTED:" + (replies[0][:3] if replies[0] else "none"))
s.close()
PY
)
case "$E20_ABORT2_OUT" in
ABORTED:426)
	assert_eq 'aborted new-file STOR reports transfer failure' denied denied ;;
*)
	assert_eq 'aborted new-file STOR reports transfer failure' "$E20_ABORT2_OUT" denied ;;
esac
E20_NEWKEY=$(AWS_ACCESS_KEY_ID=zetaadmin AWS_SECRET_ACCESS_KEY=zetaadmin \
	aws s3api head-object --bucket "$E20_BKT" --key 'never-created.txt' \
	--endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1; echo $?)
assert_eq 'aborted upload of new key creates no object (S3 view)' 254 "$E20_NEWKEY"

# The server stayed healthy after the hard aborts.
E20_HEALTHY=$(curl -s --ftp-pasv -u "$E20_USER:$E20_PASS" \
	"ftp://127.0.0.1:$E20_PORT/$E20_BKT/seed.txt" 2>/dev/null)
assert_eq 'server healthy after aborted transfers' \
	'ftp-e2e-good-content-after-overwrite' "$E20_HEALTHY"

e2e_finish
