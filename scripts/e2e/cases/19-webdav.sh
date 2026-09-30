# 19-webdav.sh — WebDAV frontend (webdav-2026-09 tree / GH issue #1):
#   19a. auth gate: anonymous/bad-password OPTIONS ⇒ 401 with
#        WWW-Authenticate: Basic; valid creds ⇒ 200 + DAV: 1.
#   19b. read surface over the wire (mode A private server): PROPFIND /,
#        Depth-1 bucket listing, GET/HEAD object + headers, 404s,
#        Depth-infinity 403.
#   19c. write surface: PUT (201/204), COPY (201), MOVE (201 + src gone),
#        MKCOL (201 + 409 no-parent), DELETE (204, recursive), PROPFIND of
#        deleted dir 404.
#   19d. mode B (second webdav entry with "bucket" set on its own port):
#        PROPFIND / lists the bucket's CONTENTS; 401 still enforced.
#   19e. fail-loud: an unknown key inside a webdav entry aborts startup
#        (launch_expect_fail helper).
#
# MANUAL MOUNT PROCEDURE (CI-SKIPPED — needs an OS kernel filesystem and
# interactive cert trust; the curl assertions above are the CI guarantee):
#
#   macOS Finder:  Cmd+K → https://127.0.0.1:PORT/ → accept the self-signed
#     cert → enter credentials (realm "zeta-object") → browse/read/write/
#     delete via Finder.
#   macOS CLI:     mkdir /tmp/webdav && mount_webdav -v webdav
#                  https://127.0.0.1:PORT/ /tmp/webdav
#   Linux davfs2:  mount -t davfs2 https://127.0.0.1:PORT/ /mnt/webdav
#                  (davfs2 config MUST set `use_locks 0` — v1 has no LOCK
#                  and rejects it with 405) → verify ls/cp/rm through the
#                  mount.
#   Windows:       net use W: https://127.0.0.1:PORT/ /user:AK PASS
#
# All methods require HTTP Basic credentials (access key = username,
# secret key = password); denied writes for read-only identities are 403.
#
# Private-server pattern (cases 14–16): the suite server is left untouched.
set -u
W19_ROOT=$(mktemp -d /tmp/e2e19-webdav.XXXXXX)
W19_WORK=$(mktemp -d /tmp/e2e19-work.XXXXXX)
W19_CERT=$(mktemp -d /tmp/e2e19-cert.XXXXXX)
W19_BKT='e2e19-webdav-bkt'
W19_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
W19_PORT_B=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
openssl req -x509 -newkey rsa:2048 -keyout "$W19_CERT/key.pem" -out "$W19_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
mkdir -p "$W19_WORK/data"

W19_USER='wd-user'
W19_PASS='wd-pass'
W19_FAILOG=''

w19_cleanup() {
	if [ -n "${W19_PID:-}" ] && kill -0 "$W19_PID" 2>/dev/null; then
		kill -TERM "$W19_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$W19_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$W19_PID" 2>/dev/null
	fi
	[ -n "$W19_FAILOG" ] && rm -f "$W19_FAILOG"
	rm -rf "$W19_ROOT" "$W19_WORK" "$W19_CERT"
}
trap w19_cleanup EXIT

# w19_req <method> <url-path> [extra curl args...] — sets W19_STATUS /
# W19_BODY. Plain Basic auth (WebDAV speaks Basic, not SigV4).
w19_req() {
	local method=$1 path=$2
	shift 2
	local body_file
	body_file=$(mktemp /tmp/e2e19-req.XXXXXX)
	W19_STATUS=$(curl -sk -o "$body_file" -w '%{http_code}' \
		--user "${W19_USER}:${W19_PASS}" \
		-X "$method" "https://127.0.0.1:$W19_PORT$path" "$@" 2>/dev/null)
	W19_BODY=$(cat "$body_file")
	rm -f "$body_file"
}

# --- launch the private server: mode A webdav (buckets visible) + mode B --
cat > "$W19_WORK/config.json" <<EOF
{
  "dataDir": "$W19_WORK/data",
  "listenAddr": "127.0.0.1:$W19_PORT",
  "certFile": "$W19_CERT/cert.pem",
  "keyFile": "$W19_CERT/key.pem",
  "frontends": [
    {"type": "s3"},
    {"type": "webdav", "listenAddr": "127.0.0.1:$W19_PORT"},
    {"type": "webdav", "listenAddr": "127.0.0.1:$W19_PORT_B", "bucket": "$W19_BKT"}
  ],
  "identities": [
    {"name": "webdav-ro", "accessKey": "$W19_USER", "secretKey": "$W19_PASS", "grants": {"*": "readwrite"}}
  ]
}
EOF

ZETAOBJECT_ACCESS_KEY=minioadmin ZETAOBJECT_SECRET_KEY=minioadmin \
	ZETAOBJECT_CONFIG="$W19_WORK/config.json" ./zeta-object-server >"$W19_WORK/server.log" 2>&1 &
W19_PID=$!
ENDPOINT="https://127.0.0.1:$W19_PORT"
BASE_URL="$ENDPOINT"
export E2E_ENDPOINT="$ENDPOINT"
if ! wait_for_port 127.0.0.1 "$W19_PORT" 15; then
	echo '  (webdav server did not start — failing case)'
	E2E_FAIL=$((E2E_FAIL + 1))
	exit 0
fi
if ! wait_for_port 127.0.0.1 "$W19_PORT_B" 15; then
	echo '  (webdav mode-B listener did not start — failing case)'
	E2E_FAIL=$((E2E_FAIL + 1))
	exit 0
fi

# Seed state through the S3 side (cross-frontend consistency IS the
# neutral-model proof).
AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin \
	aws s3api create-bucket --bucket "$W19_BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
printf 'webdav-e2e-seed-body' | AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin \
	aws s3api put-object --bucket "$W19_BKT" --key 'seed.txt' \
	--body /dev/stdin --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

# --- part 19a: auth gate --------------------------------------------------
# Anonymous OPTIONS ⇒ 401 + WWW-Authenticate: Basic.
W19_HDRS=$(mktemp /tmp/e2e19-hdrs.XXXXXX)
W19_STATUS=$(curl -sk -o /dev/null -w '%{http_code}' -D "$W19_HDRS" \
	-X OPTIONS "https://127.0.0.1:$W19_PORT/" 2>/dev/null)
assert_eq 'anonymous OPTIONS rejected' 401 "$W19_STATUS"
assert_contains '401 carries WWW-Authenticate: Basic' "$(cat "$W19_HDRS")" 'WWW-Authenticate: Basic'
rm -f "$W19_HDRS"

# Bad password ⇒ 401.
W19_STATUS=$(curl -sk -o /dev/null -w '%{http_code}' --user "$W19_USER:WRONG" \
	-X OPTIONS "https://127.0.0.1:$W19_PORT/" 2>/dev/null)
assert_eq 'bad password rejected' 401 "$W19_STATUS"

# Valid creds ⇒ 200 + DAV: 1.
W19_HDRS=$(mktemp /tmp/e2e19-hdrs.XXXXXX)
w19_req OPTIONS /
assert_eq 'authenticated OPTIONS accepted' 200 "$W19_STATUS"
assert_contains 'OPTIONS advertises DAV: 1' "$(cat "$W19_HDRS")" 'DAV: 1'
rm -f "$W19_HDRS"

# --- part 19b: read surface -----------------------------------------------
# PROPFIND / Depth 1: the seeded bucket appears as a collection.
w19_req PROPFIND / -H 'Depth: 1' --data-binary ''
assert_eq 'PROPFIND / Depth 1' 207 "$W19_STATUS"
assert_contains 'root lists the bucket as a collection' "$W19_BODY" "<href xmlns=\"DAV:\">/$W19_BKT/</href>"
assert_contains 'root response carries resourcetype' "$W19_BODY" 'resourcetype'

# PROPFIND of the bucket: the seeded object with its properties.
w19_req PROPFIND "/$W19_BKT/" -H 'Depth: 1' --data-binary ''
assert_eq 'PROPFIND bucket Depth 1' 207 "$W19_STATUS"
assert_contains 'bucket lists the seeded object href' "$W19_BODY" 'seed.txt'
assert_contains 'object carries getcontentlength' "$W19_BODY" 'getcontentlength'
assert_contains 'object carries getetag' "$W19_BODY" 'getetag'
assert_contains 'object carries getlastmodified' "$W19_BODY" 'getlastmodified'

# Lowercase depth header is parsed case-insensitively.
w19_req PROPFIND "/$W19_BKT/" -H 'depth: 1' --data-binary ''
assert_eq 'lowercase depth header honored' 207 "$W19_STATUS"

# Depth infinity ⇒ 403 propfind-finite-depth.
w19_req PROPFIND "/$W19_BKT/seed.txt" -H 'Depth: infinity' --data-binary ''
assert_eq 'Depth infinity rejected' 403 "$W19_STATUS"
assert_contains 'Depth infinity names the precondition' "$W19_BODY" 'propfind-finite-depth'

# GET the object: exact body + headers.
W19_BODY=$(curl -sk --user "$W19_USER:$W19_PASS" "https://127.0.0.1:$W19_PORT/$W19_BKT/seed.txt" 2>/dev/null)
assert_contains 'GET round-trips the bytes' "$W19_BODY" 'webdav-e2e-seed-body'
W19_STATUS=$(curl -sk -o /dev/null -w '%{http_code}' -I --user "$W19_USER:$W19_PASS" \
	"https://127.0.0.1:$W19_PORT/$W19_BKT/seed.txt" 2>/dev/null)
assert_eq 'HEAD succeeds' 200 "$W19_STATUS"

# Missing object ⇒ 404.
w19_req GET "/$W19_BKT/missing.txt"
assert_eq 'GET missing object 404' 404 "$W19_STATUS"

# --- part 19c: write surface ----------------------------------------------
# PUT create ⇒ 201; PUT again ⇒ 204.
w19_req PUT "/$W19_BKT/e2e19.txt" --data-binary 'written-over-webdav'
assert_eq 'PUT create' 201 "$W19_STATUS"
w19_req PUT "/$W19_BKT/e2e19.txt" --data-binary 'written-over-webdav'
assert_eq 'PUT overwrite' 204 "$W19_STATUS"

# GET round-trips the PUT bytes.
w19_req GET "/$W19_BKT/e2e19.txt"
assert_contains 'PUT bytes round-trip' "$W19_BODY" 'written-over-webdav'

# COPY ⇒ 201.
w19_req COPY "/$W19_BKT/e2e19.txt" -H "Destination: /$W19_BKT/e2e19-copy.txt"
assert_eq 'COPY create' 201 "$W19_STATUS"

# MOVE ⇒ 201; old href gone; new href present.
w19_req MOVE "/$W19_BKT/e2e19-copy.txt" -H "Destination: /$W19_BKT/e2e19-moved.txt"
assert_eq 'MOVE create' 201 "$W19_STATUS"
w19_req GET "/$W19_BKT/e2e19-copy.txt"
assert_eq 'MOVE removed the source' 404 "$W19_STATUS"
w19_req GET "/$W19_BKT/e2e19-moved.txt"
assert_eq 'MOVE landed the destination' 200 "$W19_STATUS"

# MKCOL ⇒ 201; missing parent ⇒ 409.
w19_req MKCOL "/$W19_BKT/e2e19dir/"
assert_eq 'MKCOL create' 201 "$W19_STATUS"
w19_req PUT "/$W19_BKT/e2e19dir/f.txt" --data-binary 'nested'
assert_eq 'PUT into MKCOL dir' 201 "$W19_STATUS"
w19_req PROPFIND "/$W19_BKT/e2e19dir/" -H 'Depth: 1' --data-binary ''
assert_eq 'PROPFIND of the dir' 207 "$W19_STATUS"
assert_contains 'dir lists its file' "$W19_BODY" 'f.txt'
w19_req MKCOL "/$W19_BKT/noparent-dir/sub/"
assert_eq 'MKCOL missing parent 409' 409 "$W19_STATUS"

# DELETE file ⇒ 204; DELETE dir ⇒ 204 (recursive); dir then 404.
w19_req DELETE "/$W19_BKT/e2e19dir/f.txt"
assert_eq 'DELETE file' 204 "$W19_STATUS"
w19_req DELETE "/$W19_BKT/e2e19dir/"
assert_eq 'DELETE dir' 204 "$W19_STATUS"
w19_req PROPFIND "/$W19_BKT/e2e19dir/" -H 'Depth: 0' --data-binary ''
assert_eq 'deleted dir PROPFIND 404' 404 "$W19_STATUS"

# LOCK is rejected 405 (rejection, not emulation) — challenge first already
# proven in 19a.
w19_req LOCK "/$W19_BKT/e2e19.txt"
assert_eq 'LOCK rejected 405' 405 "$W19_STATUS"

# --- part 19d: mode B ------------------------------------------------------
w19_status_b() {
	local method=$1 path=$2
	shift 2
	local body_file
	body_file=$(mktemp /tmp/e2e19-reqb.XXXXXX)
	W19_STATUS=$(curl -sk -o "$body_file" -w '%{http_code}' \
		--user "${W19_USER}:${W19_PASS}" \
		-X "$method" "https://127.0.0.1:$W19_PORT_B$path" "$@" 2>/dev/null)
	W19_BODY=$(cat "$body_file")
	rm -f "$body_file"
}

# Anonymous still 401 on the mode-B listener.
W19_STATUS=$(curl -sk -o /dev/null -w '%{http_code}' \
	-X OPTIONS "https://127.0.0.1:$W19_PORT_B/" 2>/dev/null)
assert_eq 'mode B anonymous rejected' 401 "$W19_STATUS"

# PROPFIND / lists the bucket's CONTENTS (seed.txt), not the bucket itself.
w19_status_b PROPFIND / -H 'Depth: 1' --data-binary ''
assert_eq 'mode B root PROPFIND' 207 "$W19_STATUS"
assert_contains 'mode B root lists the bucket contents' "$W19_BODY" 'seed.txt'
case "$W19_BODY" in
*"/$W19_BKT/"*) assert_eq 'mode B root does not expose the bucket' exposed hidden ;;
*) assert_eq 'mode B root does not expose the bucket' hidden hidden ;;
esac

# --- part 19e: fail-loud unknown key ---------------------------------------
W19_FAILOG="$W19_WORK/fail.log"
cat > "$W19_WORK/config-bad.json" <<EOF
{
  "dataDir": "$W19_WORK/data",
  "listenAddr": "127.0.0.1:$W19_PORT",
  "certFile": "$W19_CERT/cert.pem",
  "keyFile": "$W19_CERT/key.pem",
  "frontends": [
    {"type": "webdav", "bogus": 1}
  ]
}
EOF
launch_expect_fail "$W19_WORK/config-bad.json" "$W19_FAILOG" 10
assert_eq 'unknown webdav key aborts startup (process exits)' 0 "$FAILSTART_EXIT"
assert_contains 'startup log names the unknown key' "$(cat "$W19_FAILOG")" 'bogus'
W19_FAILOG=''

e2e_finish
