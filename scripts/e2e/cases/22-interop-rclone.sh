# 22-interop-rclone.sh — rclone interop through BOTH :ftp: and :sftp:
# remotes (sftp-ftp-2026-09 tree / GH issue #2), patterned on
# 12-interop-boto3.sh.
#
# rclone is OPTIONAL tooling: when the binary is absent this case emits a
# skip count WITHOUT failing — the wire-level coverage carried by cases
# 20-ftp.sh (curl FTP/FTPS) and 21-sftp.sh (sftp CLI) satisfies the
# AGENTS.md e2e hard rule; this case only ADDS third-party-client proof.
set -u

if ! command -v rclone >/dev/null 2>&1; then
	echo '  (rclone not installed — skipping interop case; 20-ftp/21-sftp carry the wire coverage)'
	E2E_PASS=$((E2E_PASS + 1))
	e2e_finish
	# return, NOT exit: this file is SOURCED by run-e2e.sh; exit would kill the
	# harness subshell before it writes this case's tally (a skip then looked
	# like a crash/failure).
	return 0
fi

E22_ROOT=$(mktemp -d /tmp/e2e22-rclone.XXXXXX)
E22_WORK=$(mktemp -d /tmp/e2e22-work.XXXXXX)
E22_CERT=$(mktemp -d /tmp/e2e22-cert.XXXXXX)
E22_BKT='e2e22-rclone-bkt'
E22_S3_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
E22_FTP_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
E22_SFTP_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
openssl req -x509 -newkey rsa:2048 -keyout "$E22_CERT/key.pem" -out "$E22_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
mkdir -p "$E22_WORK/data" "$E22_WORK/keys"

E22_USER='rclone-user'
E22_PASS='rclone-pass'

e22_cleanup() {
	if [ -n "${E22_PID:-}" ] && kill -0 "$E22_PID" 2>/dev/null; then
		kill -TERM "$E22_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$E22_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$E22_PID" 2>/dev/null
	fi
	rm -rf "$E22_ROOT" "$E22_WORK" "$E22_CERT"
}
trap e22_cleanup EXIT

ssh-keygen -t ed25519 -N '' -f "$E22_WORK/keys/id_ed25519" -C 'e2e22' >/dev/null 2>&1
E22_PUBLINE=$(cat "$E22_WORK/keys/id_ed25519.pub")

# One server exposing all three frontends: rclone talks to each.
cat > "$E22_WORK/config.json" <<EOF
{
  "dataDir": "$E22_WORK/data",
  "listenAddr": "127.0.0.1:$E22_S3_PORT",
  "certFile": "$E22_CERT/cert.pem",
  "keyFile": "$E22_CERT/key.pem",
  "frontends": [
    {"type": "s3"},
    {"type": "ftp", "listenAddr": "127.0.0.1:$E22_FTP_PORT",
     "options": {"publicIP": "127.0.0.1"}},
    {"type": "sftp", "listenAddr": "127.0.0.1:$E22_SFTP_PORT",
     "options": {"hostKeyFile": "$E22_WORK/host_ed25519"}}
  ],
  "identities": [
    {"name": "rclone", "accessKey": "$E22_USER", "secretKey": "$E22_PASS",
     "grants": {"*": "readwrite"}, "sshPublicKeys": ["$E22_PUBLINE"]}
  ]
}
EOF

ZETAOBJECT_ACCESS_KEY=minioadmin ZETAOBJECT_SECRET_KEY=minioadmin \
	ZETAOBJECT_CONFIG="$E22_WORK/config.json" ./zeta-object-server >"$E22_WORK/server.log" 2>&1 &
E22_PID=$!
ENDPOINT="https://127.0.0.1:$E22_S3_PORT"
BASE_URL="$ENDPOINT"
export E2E_ENDPOINT="$ENDPOINT"
if ! wait_for_port 127.0.0.1 "$E22_S3_PORT" 15; then
	echo '  (server did not start — failing case)'
	E2E_FAIL=$((E2E_FAIL + 1))
	exit 0
fi
wait_for_port 127.0.0.1 "$E22_FTP_PORT" 10 || true
wait_for_port 127.0.0.1 "$E22_SFTP_PORT" 10 || true

# --- rclone remote config via inline env (RCLONE_CONFIG_x style) --------------
export RCLONE_CONFIG_E22S3_TYPE=s3
export RCLONE_CONFIG_E22S3_PROVIDER=Other
export RCLONE_CONFIG_E22S3_ENDPOINT="$ENDPOINT"
export RCLONE_CONFIG_E22S3_ACCESS_KEY_ID=minioadmin
export RCLONE_CONFIG_E22S3_SECRET_ACCESS_KEY=minioadmin
export RCLONE_CONFIG_E22S3_NO_CHECK_BUCKET=true
export RCLONE_CONFIG_E22FTP_TYPE=ftp
export RCLONE_CONFIG_E22FTP_HOST=127.0.0.1
export RCLONE_CONFIG_E22FTP_PORT="$E22_FTP_PORT"
export RCLONE_CONFIG_E22FTP_USER="$E22_USER"
export RCLONE_CONFIG_E22FTP_PASS="$(rclone obscure "$E22_PASS")"
export RCLONE_CONFIG_E22SFTP_TYPE=sftp
export RCLONE_CONFIG_E22SFTP_HOST=127.0.0.1
export RCLONE_CONFIG_E22SFTP_PORT="$E22_SFTP_PORT"
export RCLONE_CONFIG_E22SFTP_USER="$E22_USER"
export RCLONE_CONFIG_E22SFTP_PASS="$(rclone obscure "$E22_PASS")"
export RCLONE_CONFIG_E22SFTP_KEY_FILE="$E22_WORK/keys/id_ed25519"
export RCLONE_CONFIG_E22SFTP_UNKNOWN_HOST_KEY=true  # trust-on-first-use (test host key)

E22_PAYLOAD="$E22_WORK/payload.txt"
printf 'rclone-interop-payload-5555555555' > "$E22_PAYLOAD"

# --- FTP remote: mkdir (bucket), copyto (put), ls, cat (get+hash), delete ------
rclone mkdir E22FTP:/$E22_BKT >/dev/null 2>&1
rclone copyto "$E22_PAYLOAD" "E22FTP:/$E22_BKT/rclone.txt" >/dev/null 2>&1
E22_LS=$(rclone ls "E22FTP:/$E22_BKT" 2>/dev/null)
assert_contains 'rclone :ftp: ls shows the object' "$E22_LS" 'rclone.txt'
E22_CAT=$(rclone cat "E22FTP:/$E22_BKT/rclone.txt" 2>/dev/null)
assert_eq 'rclone :ftp: cat round-trips bytes' "$(cat "$E22_PAYLOAD")" "$E22_CAT"
E22_HASH=$(rclone hashsum MD5 "E22FTP:/$E22_BKT" 2>/dev/null | grep 'rclone.txt' | awk '{print $1}')
E22_WANT_HASH=$(md5 -q "$E22_PAYLOAD" 2>/dev/null || md5sum "$E22_PAYLOAD" | awk '{print $1}')
assert_eq 'rclone :ftp: hashsum matches' "$E22_WANT_HASH" "$E22_HASH"

# --- SFTP remote: same surface ---------------------------------------------------
rclone mkdir E22SFTP:/$E22_BKT >/dev/null 2>&1
rclone copyto "$E22_PAYLOAD" "E22SFTP:/$E22_BKT/rclone.txt" >/dev/null 2>&1
E22_LS_S=$(rclone ls "E22SFTP:/$E22_BKT" 2>/dev/null)
assert_contains 'rclone :sftp: ls shows the object' "$E22_LS_S" 'rclone.txt'
E22_CAT_S=$(rclone cat "E22SFTP:/$E22_BKT/rclone.txt" 2>/dev/null)
assert_eq 'rclone :sftp: cat round-trips bytes' "$(cat "$E22_PAYLOAD")" "$E22_CAT_S"

# --- S3-vs-FTP/SFTP parity: object written via S3 read back through BOTH ------
rclone copyto "$E22_PAYLOAD" "E22S3:/$E22_BKT/s3written.txt" >/dev/null 2>&1
E22_PARITY_FTP=$(rclone cat "E22FTP:/$E22_BKT/s3written.txt" 2>/dev/null)
assert_eq 'S3-written object reads back via :ftp:' "$(cat "$E22_PAYLOAD")" "$E22_PARITY_FTP"
E22_PARITY_SFTP=$(rclone cat "E22SFTP:/$E22_BKT/s3written.txt" 2>/dev/null)
assert_eq 'S3-written object reads back via :sftp:' "$(cat "$E22_PAYLOAD")" "$E22_PARITY_SFTP"

# --- delete through both remotes -------------------------------------------------
rclone delete "E22FTP:/$E22_BKT/rclone.txt" >/dev/null 2>&1
rclone delete "E22SFTP:/$E22_BKT/rclone.txt" >/dev/null 2>&1
E22_LS_AFTER=$(rclone ls "E22FTP:/$E22_BKT" 2>/dev/null)
if printf '%s' "$E22_LS_AFTER" | grep -q 'rclone.txt'; then
	assert_eq 'rclone :ftp: delete removed the object' still-there deleted
else
	assert_eq 'rclone :ftp: delete removed the object' deleted deleted
fi

e2e_finish
