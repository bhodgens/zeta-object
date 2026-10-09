# 40-zeta-cache.sh - zeta-cache daemon (client-cache-2026-10 leaves
# 01+02+04+06). The daemon is a SEPARATE Go module (zeta-cache/); this
# case shells out to its built binary.
#
#   40a. --version works and prints the zeta-cache prefix.
#   40b. fail-loud config: an unknown key aborts startup (exit != 0) with
#        the offending key named in stderr.
#   40c. config smoke over a live private gateway: a valid config starts,
#        the startup probe (PROPFIND through the REAL transport, leaf 06)
#        authenticates against the gateway, and the IPC socket answers
#        {"v":1,"type":"status"} with the leaf 01 shape (state idle,
#        server/bucket echoed, counters zero). The probe client is a tiny
#        inline python3 (stdlib socket) - documented here as THE status
#        client until leaf 08 ships the real one.
#   40d. SIGTERM shutdown exits 0 and removes the socket file.
#   40e. mount section (leaf 02 + leaf 06): detects FUSE availability and
#        either exercises the LIVE mount round-trip over the real webdav
#        transport (TCP Basic) or SKIPS gracefully. CI linux runners have
#        no /dev/fuse and macFUSE is not installed on dev macOS boxes by
#        default - the graceful skip IS the CI path (leaf 02 acceptance 2).
#   40f. h3 section: the daemon's transport is generated a CA + client
#        cert (case 35's pattern); when FUSE is PRESENT the mounted daemon
#        is verified to serve reads over TCP while the gateway advertises
#        alt-svc (the h3 upgrade is mTLS-gated and arms lazily). The full
#        h3 DATA-PLANE round-trip is pinned by case 38's h3probe; here the
#        asserted surface is the daemon-side wiring: the daemon starts,
#        authenticates (TCP Basic), and reports no transport errors with
#        the client cert configured alongside Basic.
#
# MANUAL MOUNT PROCEDURE (for running 40e's green path by hand):
#
#   macOS:  install macFUSE (brew install --cask macfuse), then
#               ./zeta-cache --config zeta-cache.json
#           (go-fuse drives mount_osxfuse; the mountpoint must exist and
#           be empty; allow_other needs macFUSE's allow_other enabled.)
#   Linux:  fuse3 installed (/dev/fuse present), then the same command;
#           go-fuse direct-mounts via fusermount3 or /dev/fuse.
#   Then:   write a file in the mount, PROPFIND the bucket (curl -X
#           PROPFIND) to see it server-side; S3 PUT server-side, look for
#           it in the mount; read back through the mount.
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
# h3 frontend UDP port + its client CA (case 35's generation pattern): the
# CA signs a per-device client cert (CN = the identity) the daemon's
# transport WOULD use for the h3 upgrade (mTLS is the only h3 auth).
ZC_PORT_H3=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
printf 'keyUsage=critical,digitalSignature\nextendedKeyUsage=clientAuth\n' > "$ZC_CERT/client.ext"
openssl req -x509 -newkey rsa:2048 -keyout "$ZC_CERT/ca-key.pem" \
	-out "$ZC_CERT/ca.pem" -days 1 -nodes -subj '/CN=e2e40-client-ca' >/dev/null 2>&1
openssl req -newkey rsa:2048 -keyout "$ZC_CERT/client-key.pem" \
	-out "$ZC_CERT/client.csr" -nodes -subj "/CN=$ZC_USER" >/dev/null 2>&1
openssl x509 -req -in "$ZC_CERT/client.csr" -CA "$ZC_CERT/ca.pem" \
	-CAkey "$ZC_CERT/ca-key.pem" -CAcreateserial \
	-out "$ZC_CERT/client.pem" -days 1 -extfile "$ZC_CERT/client.ext" >/dev/null 2>&1
cat > "$ZC_WORK/config.json" <<EOF
{
  "dataDir": "$ZC_WORK/data",
  "listenAddr": "127.0.0.1:$ZC_PORT",
  "certFile": "$ZC_CERT/cert.pem",
  "keyFile": "$ZC_CERT/key.pem",
  "frontends": [
    {"type": "s3"},
    {"type": "webdav", "listenAddr": "127.0.0.1:$ZC_PORT_WD"},
    {"type": "h3", "listenAddr": "127.0.0.1:$ZC_PORT_H3", "bucket": "$ZC_BKT",
     "options": {"clientCAFile": "$ZC_CERT/ca.pem"}}
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
# Seeding goes over the S3 listener (the webdav frontend does not speak
# SigV4; awscli against it would silently fail).
AWS_ACCESS_KEY_ID="$ZC_USER" AWS_SECRET_ACCESS_KEY="$ZC_PASS" \
	aws s3api create-bucket --bucket "$ZC_BKT" --endpoint-url "https://127.0.0.1:$ZC_PORT" --no-verify-ssl >/dev/null 2>&1
sleep 1

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
  "caFile": "$ZC_CERT/cert.pem",
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
# lastSync is now stamped by the startup sync (leaf 04): a unix-seconds
# number, never null, on a daemon whose probe succeeded.
# The startup sync stamps lastSync when it completes; on a 2-core CI
# runner it may still be running when the first status lands - poll.
ZC_LS=''
for _ in $(seq 1 10); do
	ZC_RESP=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"status"}')
	ZC_LS=$(printf '%s' "$ZC_RESP" | grep -o '"lastSync":[0-9]*' | head -1)
	[ -n "$ZC_LS" ] && break
	sleep 1
done
assert_contains 'status lastSync stamped by initial sync' "$ZC_RESP" '"lastSync":1'
assert_contains 'status carries protocol v1' "$ZC_RESP" '"v":1'

ZC_RESP=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"frobnicate"}')
assert_contains 'unknown type answers ok:false' "$ZC_RESP" '"ok":false'
assert_contains 'unknown type names the error' "$ZC_RESP" 'unknown request type'

# --- 40g: FileProvider IPC surface (fileprovider-2026-10 leaf 01) ------
# IPC-ONLY asserts over the LIVE daemon: enumerate/item/download/dehydrate
# (+ mark/delete/move). No FUSE required - these run on CI too.
#
# State is built through the SUPPORTED mutation surface (mark = the
# extension's data-via-FILE contract): bytes are written straight into the
# daemon's cache dir, then flagged dirty over IPC. This exercises the
# leaf-01 contract end to end without depending on the sync engine's
# walk of remote keys (the live remote round-trip is pinned by 40e's FUSE
# section when FUSE is present).
ZC_FP_CACHE="$ZC_ROOT/cache/files/fp"
mkdir -p "$ZC_FP_CACHE/deep"
printf 'ext alpha body' > "$ZC_FP_CACHE/alpha.txt"
printf 'ext nested body' > "$ZC_FP_CACHE/deep/nested.txt"
printf 'ext clean body' > "$ZC_FP_CACHE/clean.txt"
ZC_MARK_A=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"mark","path":"fp/alpha.txt"}')
assert_contains 'mark answers ok dirty true (alpha)' "$ZC_MARK_A" '"dirty":true'
ZC_MARK_N=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"mark","path":"fp/deep/nested.txt"}')
assert_contains 'mark answers ok dirty true (nested)' "$ZC_MARK_N" '"dirty":true'
ZC_MARK_C=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"mark","path":"fp/clean.txt"}')
assert_contains 'mark answers ok dirty true (clean)' "$ZC_MARK_C" '"dirty":true'
# mark without the cache file on disk: honest error (bytes BEFORE mark).
ZC_MARK_BAD=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"mark","path":"fp/ghost.txt"}')
assert_contains 'mark without cache file answers ok:false' "$ZC_MARK_BAD" '"ok":false'

# item: the Lookup analog over a marked row (disk truth: size/materialized).
ZC_ITEM=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"item","path":"fp/alpha.txt"}')
assert_contains 'item answers ok' "$ZC_ITEM" '"ok":true'
assert_contains 'item carries the server key' "$ZC_ITEM" '"key":"fp/alpha.txt"'
assert_contains 'item carries the cache-relative path' "$ZC_ITEM" '"cachePath":"files/fp/alpha.txt"'
assert_contains 'item names the leaf' "$ZC_ITEM" '"name":"alpha.txt"'
assert_contains 'item reports materialized' "$ZC_ITEM" '"materialized":true'
assert_contains 'item carries the marked bytes size' "$ZC_ITEM" '"size":14'
# item of an unknown path: ok:false naming the path (the extension maps it
# to NSFileProviderError.noSuchItem).
ZC_ITEM_BAD=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"item","path":"fp/absent.txt"}')
assert_contains 'item unknown path answers ok:false' "$ZC_ITEM_BAD" '"ok":false'
# missing path field: validation error.
ZC_ITEM_NOARG=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"item"}')
assert_contains 'item without path answers ok:false' "$ZC_ITEM_NOARG" '"ok":false'

# enumerate the fp dir: alpha.txt + the deep/ subdirectory entry (the
# deeper marked row collapses into its immediate subdir).
ZC_ENUM=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"enumerate","path":"fp"}')
assert_contains 'enumerate answers ok' "$ZC_ENUM" '"ok":true'
assert_contains 'enumerate lists the file entry' "$ZC_ENUM" '"name":"alpha.txt"'
assert_contains 'enumerate collapses deep children into the dir entry' "$ZC_ENUM" '"name":"deep"'
assert_contains 'enumerate dir entry is isDir' "$ZC_ENUM" '"name":"deep","key":"fp/deep/","isDir":true'
# enumerate the root: fp/ visible.
ZC_ENUM_ROOT=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"enumerate"}')
assert_contains 'root enumerate answers ok' "$ZC_ENUM_ROOT" '"ok":true'
assert_contains 'root enumerate lists the fp dir' "$ZC_ENUM_ROOT" '"key":"fp/"'
# enumerate the subdir: the nested file.
ZC_ENUM_DEEP=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"enumerate","path":"fp/deep"}')
assert_contains 'subdir enumerate lists the nested file' "$ZC_ENUM_DEEP" '"name":"nested.txt"'
# enumerate of an unknown dir: ok with EMPTY entries (never null).
ZC_ENUM_NONE=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"enumerate","path":"zzz-none"}')
assert_contains 'enumerate unknown dir answers ok with empty entries' "$ZC_ENUM_NONE" '"ok":true,"extra":{"entries":[]}'

# download: an already-materialized path answers without touching the
# network (the hydration branch over the server copy is pinned by the
# live mount round-trip in 40e when FUSE is present).
ZC_DL=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"download","path":"fp/alpha.txt"}')
assert_contains 'download answers materialized true' "$ZC_DL" '"materialized":true'
assert_contains 'download names the cache path' "$ZC_DL" '"cachePath":"files/fp/alpha.txt"'
# download an unknown path: honest error after the (real) sync attempt.
ZC_DL_BAD=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"download","path":"fp/absent.txt"}')
assert_contains 'download unknown path answers ok:false' "$ZC_DL_BAD" '"ok":false'
# missing path field: validation error.
ZC_DL_NOARG=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"download"}')
assert_contains 'download without path answers ok:false' "$ZC_DL_NOARG" '"ok":false'

# delete: tombstone + cache file gone.
ZC_DEL=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"delete","path":"fp/alpha.txt"}')
assert_contains 'delete answers ok' "$ZC_DEL" '"ok":true'
if [ -e "$ZC_FP_CACHE/alpha.txt" ]; then
\t_e2e_record 1 'delete removed the cache file'
else
\t_e2e_record 0 'delete removed the cache file'
fi
ZC_ITEM_DEL=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"item","path":"fp/alpha.txt"}')
assert_contains 'deleted item is no longer addressable' "$ZC_ITEM_DEL" '"ok":false'

# move: cache rename + index row re-point.
ZC_MV=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"move","path":"fp/clean.txt","to":"fp/clean-renamed.txt"}')
assert_contains 'move answers ok from/to' "$ZC_MV" '"from":"fp/clean.txt","to":"fp/clean-renamed.txt"'
ZC_ITEM_MV=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"item","path":"fp/clean-renamed.txt"}')
assert_contains 'moved item is addressable at the new key' "$ZC_ITEM_MV" '"ok":true'
if [ -e "$ZC_FP_CACHE/clean.txt" ]; then
\t_e2e_record 1 'move removed the old cache file'
else
\t_e2e_record 0 'move removed the old cache file'
fi
# move unknown source -> honest error; move without to -> validation error.
ZC_MV_BAD=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"move","path":"fp/ghost.txt","to":"fp/x.txt"}')
assert_contains 'move unknown source answers ok:false' "$ZC_MV_BAD" '"ok":false'
ZC_MV_NOARG=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"move","path":"fp/clean-renamed.txt"}')
assert_contains 'move without to answers ok:false' "$ZC_MV_NOARG" '"ok":false'

# dehydrate: a DIRTY file is refused (the locked refusal set).
ZC_DEH_DIRTY=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"dehydrate","path":"fp/clean-renamed.txt"}')
assert_contains 'dehydrate dirty file answers ok:false' "$ZC_DEH_DIRTY" '"ok":false'
ZC_DEH_BAD=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"dehydrate","path":"fp/absent.txt"}')
assert_contains 'dehydrate unknown path answers ok:false' "$ZC_DEH_BAD" '"ok":false'
ZC_DEH_NOARG=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"dehydrate"}')
assert_contains 'dehydrate without path answers ok:false' "$ZC_DEH_NOARG" '"ok":false'
# A CLEAN+hydrated row dehydrates end to end: seed the row exactly the way
# the sync's upload pipeline leaves it (clean, hydrated) + the disk file.
python3 - "$ZC_ROOT/cache/index.db" <<'PYEOF'
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
db.execute("INSERT OR REPLACE INTO resources (path, etag, mtime, size, hydrated, dirty, deleted, lastAccess, pinned) VALUES ('fp/dehydrate-me.txt','0123456789abcdef0123456789abcdef',1728211200,3,1,0,0,0,0)")
db.commit(); db.close()
PYEOF
printf 'abc' > "$ZC_FP_CACHE/dehydrate-me.txt"
ZC_DEH=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"dehydrate","path":"fp/dehydrate-me.txt"}')
assert_contains 'dehydrate clean file answers ok' "$ZC_DEH" '"ok":true'
if [ -e "$ZC_FP_CACHE/dehydrate-me.txt" ]; then
\t_e2e_record 1 'dehydrate removed the cache file'
else
\t_e2e_record 0 'dehydrate removed the cache file'
fi
ZC_ITEM_DEH=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"item","path":"fp/dehydrate-me.txt"}')
assert_contains 'dehydrated item reports materialized false' "$ZC_ITEM_DEH" '"materialized":false'
# The old-type rejection is preserved ALONGSIDE the new types.
ZC_RESP=$(zc_status "$ZC_ROOT/run/z.ipc" '{"v":1,"type":"frobnicate2"}')
assert_contains 'unknown type still rejected with the new types live' "$ZC_RESP" '"ok":false'

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

# --- 40e: mount section (leaf 02) -------------------------------------
# FUSE availability probe: /dev/fuse on Linux, mount_osxfuse/mount_macfuse
# on macOS. Absent -> graceful skip with the documented message.
# /dev/fuse existing is NOT sufficient on Linux: an unprivileged mount
# also needs fusermount3 (or fusermount) setuid - CI runners have the
# device but no fuse3 package, and the mount then fails with permission
# denied. Probe the actual mount helper, not just the device node.
ZC_FUSE=0
if [ -e /dev/fuse ] && { command -v fusermount3 >/dev/null 2>&1 || command -v fusermount >/dev/null 2>&1; }; then
	ZC_FUSE=1
elif command -v mount_osxfuse >/dev/null 2>&1 || command -v mount_macfuse >/dev/null 2>&1; then
	ZC_FUSE=1
fi
if [ "$ZC_FUSE" -eq 0 ]; then
	echo '  (SKIP: FUSE unavailable - install macFUSE (macOS) or run on a host with /dev/fuse (Linux); CI runners exercise this skip)'
else
	# GREEN PATH (manual, on a FUSE-capable host): mount against the live
	# gateway and run the round-trip. The daemon leaves the mountpoint
	# attached, so the case unmounts in cleanup. Steps per the leaf:
	#   mount -> create file via mount -> PROPFIND sees it server-side ->
	#   S3 PUT server-side -> file appears in the mount -> read back.
	echo '  (mount section: FUSE present - running the live round-trip)'
	ZC_MNT="$ZC_ROOT/mnt-manual"
	mkdir -p "$ZC_MNT" "$ZC_ROOT/cache-manual" "$ZC_ROOT/run"
	cat > "$ZC_WORK/fuse.json" <<EOF
{
  "serverUrl": "$ZC_ENDPOINT",
  "bucket": "$ZC_BKT",
  "auth": {"accessKey": "$ZC_USER", "secretKey": "$ZC_PASS"},
  "caFile": "$ZC_CERT/cert.pem",
  "cacheDir": "$ZC_ROOT/cache-manual",
  "mountpoint": "$ZC_MNT",
  "ipcSocket": "$ZC_ROOT/run/f.ipc"
}
EOF
	"$ZC_BIN" --config "$ZC_WORK/fuse.json" >"$ZC_WORK/fuse.log" 2>&1 &
	ZC_FUSE_PID=$!
	ZC_FUSE_OK=0
	for _ in $(seq 1 50); do
		[ -S "$ZC_ROOT/run/f.ipc" ] && mountpoint -q "$ZC_MNT" 2>/dev/null && ZC_FUSE_OK=1 && break
		# macOS has no mountpoint(1); check for a fuse mount in mount(8).
		mount | grep -q "on $ZC_MNT " && ZC_FUSE_OK=1 && break
		kill -0 "$ZC_FUSE_PID" 2>/dev/null || break
		sleep 0.3
	done
	if [ "$ZC_FUSE_OK" -eq 0 ]; then
		# Diagnose-before-fail: the daemon's own log names the mount error.
		echo '  (mount failed - daemon log:)'
		tail -20 "$ZC_WORK/fuse.log" 2>/dev/null || echo '  (no fuse.log)'
		kill -0 "$ZC_FUSE_PID" 2>/dev/null && echo '  (daemon still running)' || echo '  (daemon EXITED)'
	fi
	assert_eq 'daemon mounts the bucket namespace' 1 "$ZC_FUSE_OK"
	if [ "$ZC_FUSE_OK" -eq 1 ]; then
		# create via mount -> server-side PROPFIND
		echo 'e2e via fuse' > "$ZC_MNT/fuse-roundtrip.txt"
		# Prompt-upload: watcher debounce (2s) + PUT. Poll up to 12s before
		# declaring failure - CI runners are slow and the debounce plus a
		# PUT can exceed 4s.
		W40_BODY=''
		for _ in $(seq 1 6); do
			sleep 2
			W40_BODY=$(curl -sk --user "$ZC_USER:$ZC_PASS" \
				-X PROPFIND -H 'Depth: 1' "$ZC_ENDPOINT/$ZC_BKT/" 2>/dev/null)
			printf '%s' "$W40_BODY" | grep -q 'fuse-roundtrip.txt' && break
		done
		if ! printf '%s' "$W40_BODY" | grep -q 'fuse-roundtrip.txt'; then
			echo '  (diag: file not server-side after 12s - daemon log:)'
			tail -25 "$ZC_WORK/fuse.log" 2>/dev/null || echo '  (no fuse.log)'
			echo '  (diag: ipc status:)' ; zc_status "$ZC_ROOT/run/f.ipc" '{"v":1,"type":"status"}' 2>/dev/null || true
			echo "  (diag: cache files dir:)" ; ls -R "$ZC_ROOT/cache-manual" 2>/dev/null | head -20
		fi
		assert_contains 'file written via mount appears server-side' "$W40_BODY" 'fuse-roundtrip.txt'
		# server-side write -> visible in the mount (PROPFIND-driven)
		echo 'server wrote me' | curl -sk --user "$ZC_USER:$ZC_PASS" \
			--data-binary @- -X PUT "$ZC_ENDPOINT/$ZC_BKT/server-seed.txt" >/dev/null
		sleep 2 # sync-scan budget (leaf 04 tightens)
		assert_contains 'server-side write surfaces in the mount' "$(ls "$ZC_MNT" 2>/dev/null)" 'server-seed.txt'
		# read back through the mount. Hydration on first open can lag the
		# listing entry on CI (attr cache + hydrate); retry briefly and
		# dump diagnostics while it settles.
		W40_READ=''
		for _ in $(seq 1 6); do
			sleep 1
			W40_READ=$(cat "$ZC_MNT/server-seed.txt" 2>/dev/null)
			printf '%s' "$W40_READ" | grep -q 'server wrote me' && break
			ls -la "$ZC_MNT" 2>/dev/null | grep server-seed || true
		done
		assert_contains 'server-seeded file reads back through the mount' "$W40_READ" 'server wrote me'
	fi
	kill -TERM "$ZC_FUSE_PID" 2>/dev/null
	for _ in $(seq 1 25); do
		kill -0 "$ZC_FUSE_PID" 2>/dev/null || break
		sleep 0.2
	done
	if kill -0 "$ZC_FUSE_PID" 2>/dev/null; then
		kill -9 "$ZC_FUSE_PID" 2>/dev/null
		wait "$ZC_FUSE_PID" 2>/dev/null
	fi
	umount "$ZC_MNT" 2>/dev/null || umount -f "$ZC_MNT" 2>/dev/null || true
fi

# --- 40f: alt-svc advertisement + the daemon with client-cert material ---
# The gateway advertises h3 on every webdav response (leaf 02 of
# quic-h3-2026-10); the daemon's transport parses it and arms the upgrade
# only when a client cert is configured (mTLS is the ONLY h3 auth; the
# config's auth model is EITHER Basic OR client cert - never both).
# Asserted here: the header shape on the wire, and an mTLS-shaped daemon
# (clientCert/clientKey + caFile) whose transport authenticates over the
# cert path. The full h3 data plane (handshake success + no-cert/wrong-CA
# handshake REJECTION = crypto failure, not an HTTP status) is pinned by
# case 38's h3probe against the same frontend type; the daemon-side h3
# upgrade state machine lives in internal/transport's unit tests.
ZC_ALTSVC=$(curl -sk -o /dev/null -D - -u "$ZC_USER:$ZC_PASS" \
	"$ZC_ENDPOINT/$ZC_BKT/" 2>/dev/null | grep -i '^alt-svc:' | tr -d '\r')
assert_contains 'gateway advertises alt-svc h3 with persist=1' "$ZC_ALTSVC" "h3=\":$ZC_PORT_H3\"; persist=1"

mkdir -p "$ZC_ROOT/cache-cert" "$ZC_ROOT/mnt-cert" "$ZC_ROOT/run"
cat > "$ZC_WORK/cert.json" <<EOF
{
  "serverUrl": "$ZC_ENDPOINT",
  "bucket": "$ZC_BKT",
  "auth": {"clientCert": "$ZC_CERT/client.pem", "clientKey": "$ZC_CERT/client-key.pem"},
  "caFile": "$ZC_CERT/cert.pem",
  "cacheDir": "$ZC_ROOT/cache-cert",
  "mountpoint": "$ZC_ROOT/mnt-cert",
  "ipcSocket": "$ZC_ROOT/run/c.ipc"
}
EOF
ZC_CERT_LOG="$ZC_WORK/cert-daemon.log"
"$ZC_BIN" --config "$ZC_WORK/cert.json" >"$ZC_CERT_LOG" 2>&1 &
ZC_CERT_PID=$!
ZC_CERT_UP=1
for _ in $(seq 1 25); do
	[ -S "$ZC_ROOT/run/c.ipc" ] && break
	if ! kill -0 "$ZC_CERT_PID" 2>/dev/null; then
		ZC_CERT_UP=0
		break
	fi
	sleep 0.2
done
assert_eq 'daemon with clientCert+caFile starts (auth material loads)' 1 "$ZC_CERT_UP"
# Give the initial sync its h3-upgrade window (the probe 401s over TCP
# Basic; the sync rides the mTLS h3 path armed by alt-svc).
for _ in $(seq 1 30); do
	grep -q 'initial sync' "$ZC_CERT_LOG" 2>/dev/null && break
	kill -0 "$ZC_CERT_PID" 2>/dev/null || break
	sleep 0.3
done
ZC_CERT_LOG_BODY=$(cat "$ZC_CERT_LOG" 2>/dev/null)
# mTLS is the ONLY h3 auth; over the TCP webdav (Basic) frontend a cert-only
# daemon correctly gets 401 - the REAL proof of the cert path is the sync
# riding the h3 upgrade (mTLS) that the alt-svc advertisement armed. Assert
# the initial sync COMPLETED over h3 (the TCP probe 401 is the expected
# mode mismatch, WARNING-only per the lifecycle rule).
assert_contains 'cert-daemon initial sync completes over the h3 upgrade' "$ZC_CERT_LOG_BODY" 'initial sync complete'
if grep -q 'initial sync failed\|panic\|ErrAuthFailed\|handshake' "$ZC_CERT_LOG" 2>/dev/null; then
	_e2e_record 1 'cert-daemon log carries no sync/handshake failures'
else
	_e2e_record 0 'cert-daemon log carries no sync/handshake failures'
fi
kill -TERM "$ZC_CERT_PID" 2>/dev/null
for _ in $(seq 1 25); do
	kill -0 "$ZC_CERT_PID" 2>/dev/null || break
	sleep 0.2
done
if kill -0 "$ZC_CERT_PID" 2>/dev/null; then
	kill -9 "$ZC_CERT_PID" 2>/dev/null
	wait "$ZC_CERT_PID" 2>/dev/null
fi

printf '\n'
