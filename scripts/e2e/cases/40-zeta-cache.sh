# 40-zeta-cache.sh - zeta-cache daemon skeleton (client-cache-2026-10 leaf
# 01). The daemon is a SEPARATE Go module (zeta-cache/); this case shells
# out to its built binary.
#
#   40a. --version works and prints the zeta-cache prefix.
#   40b. fail-loud config: an unknown key aborts startup (exit != 0) with
#        the offending key named in stderr.
#   40c. config smoke over a live private gateway: a valid config starts,
#        the startup OPTIONS probe authenticates against the gateway, and
#        the IPC socket answers {"v":1,"type":"status"} with the leaf 01
#        shape (state idle, server/bucket echoed, counters zero). The probe
#        client is a tiny inline python3 (stdlib socket) - documented here
#        as THE status client until leaf 08 ships the real one.
#   40d. SIGTERM shutdown exits 0 and removes the socket file.
#   40e. mount + sync sections SKIP gracefully until leaves 02 + 04 land.
#
# Private-server pattern (case 19): the suite server is left untouched.
set -u
E2E_ROOT="${BASH_SOURCE[0]%/*}/.."
E2E_ROOT=$(cd "$E2E_ROOT" && pwd)
REPO_ROOT=$(cd "$E2E_ROOT/../.." && pwd)
ZC_ROOT=$(mktemp -d /tmp/e2e40-zc.XXXXXX)
ZC_WORK=$(mktemp -d /tmp/e2e40-work.XXXXXX)
ZC_CERT=$(mktemp -d /tmp/e2e40-cert.XXXXXX)
ZC_BKT='e2e40-cache-bkt'
ZC_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
openssl req -x509 -newkey rsa:2048 -keyout "$ZC_CERT/key.pem" -out "$ZC_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
mkdir -p "$ZC_WORK/data"

ZC_USER='zc-user'
ZC_PASS='zc-pass'
ZC_BIN="$REPO_ROOT/zeta-cache-bin"
ZC_DAEMON_PID=''
ZC_FAILOG=''

zc_cleanup() {
	if [ -n "$ZC_DAEMON_PID" ] && kill -0 "$ZC_DAEMON_PID" 2>/dev/null; then
		kill -9 "$ZC_DAEMON_PID" 2>/dev/null
		wait "$ZC_DAEMON_PID" 2>/dev/null
	fi
	[ -n "$ZC_FAILOG" ] && rm -f "$ZC_FAILOG"
	rm -rf "$ZC_ROOT" "$ZC_WORK" "$ZC_CERT"
}
trap zc_cleanup EXIT

# --- build the zeta-cache binary (separate module; CGO banned repo-wide) ---
if ! (cd "$REPO_ROOT/zeta-cache" && CGO_ENABLED=0 go build -o "$ZC_BIN" .); then
	echo '  (zeta-cache build failed - failing case)'
	E2E_FAIL=$((E2E_FAIL + 1))
	exit 0
fi

# --- launch the private gateway: webdav frontend (Basic) + s3. The
# zeta-cache startup probe authenticates with Basic over the webdav
# frontend (the client protocol per the plan tree; s3 alone is SigV4).
# The webdav entry needs its OWN listenAddr: it cannot share the global
# listener with s3 (same rule case 19 documents).
ZC_PORT_WD=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
cat > "$ZC_WORK/config.json" <<EOF
{
  "dataDir": "$ZC_WORK/data",
  "listenAddr": "127.0.0.1:$ZC_PORT",
  "certFile": "$ZC_CERT/cert.pem",
  "keyFile": "$ZC_CERT/key.pem",
  "frontends": [
    {"type": "s3"},
    {"type": "webdav", "listenAddr": "127.0.0.1:$ZC_PORT_WD"}
  ],
  "identities": [
    {"name": "zc-identity", "accessKey": "$ZC_USER", "secretKey": "$ZC_PASS", "grants": {"*": "readwrite"}}
  ]
}
EOF

ZETAOBJECT_ACCESS_KEY=zetaadmin ZETAOBJECT_SECRET_KEY=zetaadmin \
	ZETAOBJECT_CONFIG="$ZC_WORK/config.json" ./zeta-object-server >"$ZC_WORK/server.log" 2>&1 &
ZC_SERVER_PID=$!
ZC_ENDPOINT="https://127.0.0.1:$ZC_PORT_WD"
if ! wait_for_port 127.0.0.1 "$ZC_PORT_WD" 15; then
	echo '  (gateway did not start - failing case)'
	E2E_FAIL=$((E2E_FAIL + 1))
	exit 0
fi

# Seed the bucket the daemon points at (the probe hits its bucket path).
AWS_ACCESS_KEY_ID="$ZC_USER" AWS_SECRET_ACCESS_KEY="$ZC_PASS" \
	aws s3api create-bucket --bucket "$ZC_BKT" --endpoint-url "$ZC_ENDPOINT" --no-verify-ssl >/dev/null 2>&1

# --- 40a: --version ---
ZC_OUT=$("$ZC_BIN" --version 2>&1)
ZC_RC=$?
assert_eq '--version exits 0' 0 "$ZC_RC"
assert_contains '--version prints zeta-cache prefix' "$ZC_OUT" 'zeta-cache'

# --- 40b: unknown config key aborts, naming the key ---
cat > "$ZC_WORK/bad.json" <<EOF
{
  "serverUrl": "$ZC_ENDPOINT",
  "bucket": "$ZC_BKT",
  "mountpoint": "$ZC_ROOT/mnt",
  "auth": {"accessKey": "$ZC_USER", "secretKey": "$ZC_PASS"},
  "serverUrlr": "typo"
}
EOF
ZC_FAILOG="$ZC_WORK/bad.log"
ZETAOBJECT_TEST_TMPDIR="$ZC_ROOT" "$ZC_BIN" --config "$ZC_WORK/bad.json" >"$ZC_FAILOG" 2>&1 &
ZC_BAD_PID=$!
ZC_BAD_RC=''
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
	if ! kill -0 "$ZC_BAD_PID" 2>/dev/null; then
		wait "$ZC_BAD_PID" 2>/dev/null
		ZC_BAD_RC=$?
		break
	fi
	sleep 0.2
done
if [ -z "$ZC_BAD_RC" ]; then
	kill -9 "$ZC_BAD_PID" 2>/dev/null
	wait "$ZC_BAD_PID" 2>/dev/null
fi
if [ -n "$ZC_BAD_RC" ] && [ "$ZC_BAD_RC" -ne 0 ]; then
	_e2e_record 0 'unknown config key aborts non-zero'
else
	_e2e_record 1 'unknown config key aborts non-zero'
fi
ZC_BAD_LOG=$(cat "$ZC_FAILOG" 2>/dev/null)
assert_contains 'abort names the offending key' "$ZC_BAD_LOG" 'serverUrlr'
ZC_FAILOG=''

# --- 40c: valid config starts; probe authenticates; status over IPC ---
mkdir -p "$ZC_ROOT/cache" "$ZC_ROOT/mnt" "$ZC_ROOT/run"
cat > "$ZC_WORK/good.json" <<EOF
{
  "serverUrl": "$ZC_ENDPOINT",
  "bucket": "$ZC_BKT",
  "auth": {"accessKey": "$ZC_USER", "secretKey": "$ZC_PASS"},
  "cacheDir": "$ZC_ROOT/cache",
  "mountpoint": "$ZC_ROOT/mnt",
  "ipcSocket": "$ZC_ROOT/run/z.ipc"
}
EOF
ZC_LOG="$ZC_WORK/daemon.log"
"$ZC_BIN" --config "$ZC_WORK/good.json" >"$ZC_LOG" 2>&1 &
ZC_DAEMON_PID=$!
ZC_UP=1
for _ in $(seq 1 25); do
	[ -S "$ZC_ROOT/run/z.ipc" ] && break
	if ! kill -0 "$ZC_DAEMON_PID" 2>/dev/null; then
		ZC_UP=0
		break
	fi
	sleep 0.2
done
assert_eq 'daemon starts and creates the IPC socket' 1 "$ZC_UP"
ZC_START_LOG=$(cat "$ZC_LOG" 2>/dev/null)
assert_contains 'daemon log carries the zeta-cache prefix' "$ZC_START_LOG" 'zeta-cache:'
assert_contains 'startup probe authenticated against the gateway' "$ZC_START_LOG" 'connectivity probe ok'
if grep -q 'WARNING connectivity probe failed' "$ZC_LOG" 2>/dev/null; then
	_e2e_record 1 'probe failure is WARNING-only (daemon stayed up)'
else
	_e2e_record 0 'probe failure is WARNING-only (daemon stayed up)'
fi

# zc_status <socket> <request-json> - one-request-per-connection client.
# Inline python3 stdlib; documented as the status client until leaf 08.
zc_status() {
	python3 - "$1" "$2" <<'PY'
import json, socket, sys
path, req = sys.argv[1], sys.argv[2]
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.settimeout(5)
s.connect(path)
s.sendall(req.encode() + b"\n")
buf = b""
while b"\n" not in buf:
    chunk = s.recv(4096)
    if not chunk:
        break
    buf += chunk
print(buf.decode().strip())
PY
}

ZC_RESP=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"status"}')
assert_contains 'status answers ok:true' "$ZC_RESP" '"ok":true'
assert_contains 'status reports state idle' "$ZC_RESP" '"state":"idle"'
assert_contains 'status echoes the bucket' "$ZC_RESP" "\"bucket\":\"$ZC_BKT\""
assert_contains 'status echoes the server url' "$ZC_RESP" "\"server\":\"$ZC_ENDPOINT\""
assert_contains 'status lastSync is null' "$ZC_RESP" '"lastSync":null'
assert_contains 'status carries protocol v1' "$ZC_RESP" '"v":1'

ZC_RESP=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"frobnicate"}')
assert_contains 'unknown type answers ok:false' "$ZC_RESP" '"ok":false'
assert_contains 'unknown type names the error' "$ZC_RESP" 'unknown request type'

# Socket mode 0600 (python stat: portable across BSD/GNU stat).
ZC_MODE=$(python3 -c 'import os,sys; print("%o" % (os.stat(sys.argv[1]).st_mode & 0o777))' "$ZC_ROOT/run/z.ipc" 2>/dev/null)
assert_eq 'socket file mode is 600' 600 "$ZC_MODE"

# --- 40d: SIGTERM shutdown exits 0, socket removed ---
ZC_SHUT_RC=''
kill -TERM "$ZC_DAEMON_PID" 2>/dev/null
for _ in $(seq 1 25); do
	if ! kill -0 "$ZC_DAEMON_PID" 2>/dev/null; then
		wait "$ZC_DAEMON_PID" 2>/dev/null
		ZC_SHUT_RC=$?
		break
	fi
	sleep 0.2
done
if [ -z "$ZC_SHUT_RC" ]; then
	kill -9 "$ZC_DAEMON_PID" 2>/dev/null
	wait "$ZC_DAEMON_PID" 2>/dev/null
fi
ZC_DAEMON_PID=''
assert_eq 'SIGTERM shutdown exits 0' 0 "$ZC_SHUT_RC"
if [ -e "$ZC_ROOT/run/z.ipc" ]; then
	_e2e_record 1 'shutdown removes the socket file'
else
	_e2e_record 0 'shutdown removes the socket file'
fi

# --- 40e: mount + sync sections SKIP until leaves 02 + 04 land ---
# MANUAL MOUNT PROCEDURE (SKIPPED - leaf 02 owns the FUSE layer; until it
# lands zeta-cache has no mountpoint and sync never leaves the idle state):
#
#   macOS:      ./zeta-cache-bin --config zeta-cache.json  (mounts via
#               macFUSE at cfg.mountpoint once leaf 02 lands)
#   Linux:      fuse3/go-fuse, same config key.
#   Then:       write a file in the mount, wait for the ~2s prompt-upload
#               debounce (leaf 07), PROPFIND the bucket to see it.
#
# Sync-surface assertions (leaf 04) will live here: PUT through the mount
# with If-Match, conflict-copy naming, batch delete via ?batch.
echo '  (SKIP: mount/sync assertions wait for leaves 02 and 04)'
