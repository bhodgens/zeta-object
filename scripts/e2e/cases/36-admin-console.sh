#!/usr/bin/env bash
# 36-admin-console.sh — wire-level e2e for the web console (admin-server-2026-10
# leaf 07; master.md Contract 2). No browser: the case drives the console's own
# HTTP surface, which proxies the gateway's management API over mTLS.
#
#   36a. Shell: GET / without a session returns the HTML shell carrying the four
#        rail tabs (data-screen dashboard/config/buckets/danger).
#   36b. Assets: GET /assets/app.css returns the theme CSS and the accent colour
#        literal from the logo palette (#2f8fc4).
#   36c. Session gate: every /api/... route without a session -> 401 with the
#        JSON error envelope {error:{code,message}}.
#   36d. Login: a wrong operator token -> 401; the right token sets the
#        zeta_session cookie (cookie jar).
#   36e. Proxying with the session: GET /api/status returns the gateway's status
#        JSON (asserts a field the gateway really sends); GET /api/config leaks
#        no unmasked secret; POST/GET/DELETE /api/buckets round-trips a plain
#        bucket, asserting the on-disk directory exists then is gone (the way
#        case 35 does).
#   36f. CSRF: a mutating call without the X-CSRF-Token header -> 403 with the
#        envelope.
#   36g. Dataset-delete refusal (ZFS only; skipped otherwise): DELETE a
#        dataset-backed bucket -> 409 with DatasetBucketNotDeletable.
#   36h. Logout: POST /logout clears the session; a following /api/status -> 401.
#
# Reuses the harness's client-certificate fixtures (run-e2e.sh): the console
# talks mTLS to the gateway's admin listener with the e2e-admin client pair, and
# trusts the gateway's own listener certificate as its root. The console's own
# listener runs plain HTTP on loopback (no certFile/keyFile), which is what the
# case wants. BKT= convention, create/cleanup pairing, an EXIT trap that kills
# the console and removes the work dir, and the standard SKIP line shape.
set -u
BKT='e2e36-console'
C36_WORK=''
C36_CONSOLE_PID=''
C36_BKT_DIR=''
C36_JAR=''
C36_CSRF=''

# --- whole-case guard: the console binary must have been built by the harness
# and the client-certificate fixtures (openssl) must exist. Otherwise print the
# standard SKIP line and leave cleanly (0 pass / 0 fail).
if [ "${E2E_ADMIN_CONSOLE_AVAILABLE:-0}" != 1 ] || [ ! -x "${E2E_ADMIN_CONSOLE_BIN:-}" ]; then
	echo '  (admin console: the zeta-object-admin binary is unavailable (not built) — skipping case; unit + wiring coverage in internal/adminserver carries the contract)'
	e2e_finish
	return 0 2>/dev/null || exit 0
fi
if [ "${E2E_ADMIN_AVAILABLE:-0}" != 1 ] || [ -z "${E2E_ADMIN_URL:-}" ] \
	|| [ -z "${E2E_ADMIN_CLIENT_CERT:-}" ] || [ -z "${E2E_ADMIN_CLIENT_KEY:-}" ] \
	|| [ -z "${E2E_SERVER_CERT:-}" ] || [ ! -s "${E2E_SERVER_CERT:-/nonexistent}" ]; then
	echo '  (admin console: client-certificate fixtures unavailable (openssl missing) — skipping case; unit + wiring coverage in internal/adminserver carries the contract)'
	e2e_finish
	return 0 2>/dev/null || exit 0
fi

# --- work dir, free port, config ---------------------------------------------
C36_WORK=$(mktemp -d /tmp/e2e36-console.XXXXXX)
C36_JAR="$C36_WORK/cookies.txt"
C36_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
if [ -z "$C36_PORT" ]; then
	assert_eq '36 setup: could not detect a free loopback port for the console' 0 1
	e2e_finish
	return 0 2>/dev/null || exit 0
fi
C36_BASE="http://127.0.0.1:$C36_PORT"
C36_TOKEN='e2e36-operator-token'
C36_WRONG_TOKEN='e2e36-not-the-token'

cat > "$C36_WORK/admin-config.json" <<EOF
{
  "listenAddr": "127.0.0.1:$C36_PORT",
  "gatewayUrl": "$E2E_ADMIN_URL",
  "caFile": "$E2E_SERVER_CERT",
  "clientCert": "$E2E_ADMIN_CLIENT_CERT",
  "clientKey": "$E2E_ADMIN_CLIENT_KEY",
  "operatorToken": "$C36_TOKEN"
}
EOF

# c36_cleanup — best-effort removal of everything this case created: the plain
# bucket (via the API while the session lives), its directory, the console
# process, and the work dir (config, cookie jar, logs). It NEVER touches the
# shared gateway server.
c36_cleanup() {
	if [ -n "$C36_JAR" ] && [ -s "$C36_JAR" ] && [ -n "$C36_CSRF" ]; then
		curl -s -b "$C36_JAR" -X DELETE "$C36_BASE/api/buckets/$BKT" \
			-H "X-CSRF-Token: $C36_CSRF" >/dev/null 2>&1 || true
	fi
	[ -n "$C36_BKT_DIR" ] && rm -rf "$C36_BKT_DIR"
	if [ -n "$C36_CONSOLE_PID" ] && kill -0 "$C36_CONSOLE_PID" 2>/dev/null; then
		kill -TERM "$C36_CONSOLE_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$C36_CONSOLE_PID" 2>/dev/null || break
			sleep 0.2
		done
		kill -9 "$C36_CONSOLE_PID" 2>/dev/null
	fi
	if [ -n "$C36_WORK" ]; then
		rm -rf "$C36_WORK"
	fi
}
trap c36_cleanup EXIT

# --- launch the console -------------------------------------------------------
ZETAOBJECT_ADMIN_CONFIG="$C36_WORK/admin-config.json" "$E2E_ADMIN_CONSOLE_BIN" \
	>"$C36_WORK/console.log" 2>&1 &
C36_CONSOLE_PID=$!
if ! wait_for_port 127.0.0.1 "$C36_PORT" 15; then
	echo '         console did not start; log follows:'
	sed 's/^/         /' "$C36_WORK/console.log"
	assert_eq '36 setup: console started listening' listening dead
	e2e_finish
	return 0 2>/dev/null || exit 0
fi

# --- console-request helpers --------------------------------------------------
# creq <curl args...> — sets C_STATUS (HTTP code) and C_BODY. Always carries the
# cookie jar so a session persists across calls.
C_STATUS=''
C_BODY=''
creq() {
	local bf
	bf=$(mktemp /tmp/e2e36-creq.XXXXXX)
	C_STATUS=$(curl -s -b "$C36_JAR" -c "$C36_JAR" -o "$bf" -w '%{http_code}' "$@" 2>/dev/null)
	C_BODY=$(cat "$bf")
	rm -f "$bf"
}

# assert_envelope <label> <body> — the body is the JSON error envelope
# {error:{code:<non-empty string>,message:<non-empty string>}}.
assert_envelope() {
	local shape
	shape=$(printf '%s' "$2" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print("bad"); sys.exit(0)
e = d.get("error")
ok = (isinstance(e, dict) and isinstance(e.get("code"), str) and e.get("code")
      and isinstance(e.get("message"), str) and e.get("message"))
print("ok" if ok else "bad")
')
	assert_eq "$1" ok "$shape"
}

# --- 36a: the shell and its rail tabs ----------------------------------------
creq "$C36_BASE/"
assert_eq '36a GET / without a session -> 200' 200 "$C_STATUS"
assert_contains '36a shell carries the dashboard rail tab' "$C_BODY" 'data-screen="dashboard"'
assert_contains '36a shell carries the config rail tab' "$C_BODY" 'data-screen="config"'
assert_contains '36a shell carries the buckets rail tab' "$C_BODY" 'data-screen="buckets"'
assert_contains '36a shell carries the danger rail tab' "$C_BODY" 'data-screen="danger"'

# --- 36b: the theme asset -----------------------------------------------------
creq "$C36_BASE/assets/app.css"
assert_eq '36b GET /assets/app.css -> 200' 200 "$C_STATUS"
assert_contains '36b app.css carries the accent token' "$C_BODY" '--accent'
assert_contains '36b app.css carries the logo accent literal' "$C_BODY" '#2f8fc4'

# --- 36c: the session gate on every /api route -------------------------------
# (method path) pairs covering every Contract 2 /api row. No session is present.
for route in \
	'GET /api/status' \
	'GET /api/config' \
	'PUT /api/config' \
	'POST /api/config/save' \
	'POST /api/auth/reload' \
	'GET /api/buckets' \
	'POST /api/buckets' \
	'GET /api/buckets/e2e36-none' \
	'DELETE /api/buckets/e2e36-none' \
	'PUT /api/buckets/e2e36-none/settings' \
	'POST /api/purge'; do
	m=${route%% *}
	p=${route#* }
	creq -X "$m" "$C36_BASE$p" -H 'Content-Type: application/json' --data '{}'
	assert_eq "36c no session: $m $p -> 401" 401 "$C_STATUS"
	assert_envelope "36c no session: $m $p carries the error envelope" "$C_BODY"
done

# --- 36d: login ---------------------------------------------------------------
creq -X POST "$C36_BASE/login" -H 'Content-Type: application/json' \
	--data "{\"token\":\"$C36_WRONG_TOKEN\"}"
assert_eq '36d POST /login with a wrong token -> 401' 401 "$C_STATUS"
assert_envelope '36d wrong-token rejection carries the error envelope' "$C_BODY"

creq -X POST "$C36_BASE/login" -H 'Content-Type: application/json' \
	--data "{\"token\":\"$C36_TOKEN\"}"
assert_eq '36d POST /login with the right token -> 200' 200 "$C_STATUS"
if grep -q 'zeta_session' "$C36_JAR" 2>/dev/null; then
	assert_eq '36d login sets the zeta_session cookie' set set
else
	assert_eq '36d login sets the zeta_session cookie' set missing
fi
C36_CSRF=$(printf '%s' "$C_BODY" | python3 -c '
import json, sys
try:
    print(json.load(sys.stdin).get("csrfToken", ""))
except Exception:
    print("")
')
if [ -n "$C36_CSRF" ]; then
	assert_eq '36d login returns a CSRF token for mutating calls' present present
else
	assert_eq '36d login returns a CSRF token for mutating calls' present missing
fi

# --- 36e: proxying with the session ------------------------------------------
creq "$C36_BASE/api/status"
assert_eq '36e GET /api/status (session) -> 200' 200 "$C_STATUS"
assert_contains '36e status body is the gateway status JSON (version field)' "$C_BODY" '"version"'
assert_contains '36e status body carries the gateway frontends list' "$C_BODY" '"frontends"'

creq "$C36_BASE/api/config"
assert_eq '36e GET /api/config (session) -> 200' 200 "$C_STATUS"
assert_contains '36e config body is the gateway config document' "$C_BODY" '"dataDir"'
if printf '%s' "$C_BODY" | grep -qF "${E2E_ADMIN_CONFIG_SECRET:-__none__}"; then
	assert_eq '36e /api/config never contains the literal secretKey' absent LEAKED
else
	assert_eq '36e /api/config never contains the literal secretKey' absent absent
fi
assert_contains '36e /api/config masks the configured secret' "$C_BODY" '"secretKey":"********"'

C36_BKT_DIR="$E2E_DATA_DIR/$BKT"
creq -X POST "$C36_BASE/api/buckets" -H "X-CSRF-Token: $C36_CSRF" \
	-H 'Content-Type: application/json' --data "{\"name\":\"$BKT\"}"
assert_eq '36e POST /api/buckets (session+CSRF) -> 200' 200 "$C_STATUS"
assert_contains '36e create body confirms the bucket' "$C_BODY" '"created":true'
if [ -d "$C36_BKT_DIR" ]; then
	assert_eq '36e created bucket directory exists on disk' exists exists
else
	assert_eq '36e created bucket directory exists on disk' exists missing
fi

creq "$C36_BASE/api/buckets"
assert_eq '36e GET /api/buckets (session) -> 200' 200 "$C_STATUS"
assert_contains '36e new bucket appears in the list' "$C_BODY" "$BKT"

# --- 36f: CSRF gate on a mutating call (session present, header absent) -------
creq -X POST "$C36_BASE/api/buckets" -H 'Content-Type: application/json' \
	--data '{"name":"e2e36-csrf-should-not-exist"}'
assert_eq '36f mutating call without X-CSRF-Token -> 403' 403 "$C_STATUS"
assert_envelope '36f CSRF rejection carries the error envelope' "$C_BODY"

# Delete the plain bucket created in 36e (directory gone).
creq -X DELETE "$C36_BASE/api/buckets/$BKT" -H "X-CSRF-Token: $C36_CSRF"
assert_eq '36e DELETE /api/buckets/{name} (session+CSRF) -> 200' 200 "$C_STATUS"
assert_contains '36e delete body confirms the deletion' "$C_BODY" '"deleted":true'
if [ -d "$C36_BKT_DIR" ]; then
	assert_eq '36e bucket directory gone after delete' gone present
else
	assert_eq '36e bucket directory gone after delete' gone gone
fi

# --- 36g: dataset-delete refusal (ZFS only) ----------------------------------
if ! command -v zfs >/dev/null 2>&1; then
	echo '  (admin console dataset-delete refusal: requires ZFS; no zfs binary on this host — skipping group; live coverage in scripts/zfs-validate/run-zfs-validation.sh)'
elif ! zfs list -H -o name "$E2E_DATA_DIR" >/dev/null 2>&1; then
	echo '  (admin console dataset-delete refusal: dataDir is not a ZFS mountpoint — skipping group; live coverage in scripts/zfs-validate/run-zfs-validation.sh)'
else
	C36_DS_BKT='e2e36-ds'
	C36_PARENT=$(zfs list -H -o name -t filesystem "$E2E_DATA_DIR" 2>/dev/null | head -1 | tr -d '[:space:]')
	creq -X POST "$C36_BASE/api/buckets" -H "X-CSRF-Token: $C36_CSRF" \
		-H 'Content-Type: application/json' --data "{\"name\":\"$C36_DS_BKT\"}"
	C36_DS=''
	if [ -n "$C36_PARENT" ]; then
		C36_DS=$(zfs list -H -o name "$C36_PARENT/$C36_DS_BKT" 2>/dev/null | head -1 | tr -d '[:space:]')
	fi
	if [ -z "$C36_DS" ]; then
		echo '  (admin console dataset-delete refusal: the shared server did not provision a dataset (zfs_bucket_datasets off) — skipping group; live coverage in scripts/zfs-validate/run-zfs-validation.sh)'
	else
		creq -X DELETE "$C36_BASE/api/buckets/$C36_DS_BKT" -H "X-CSRF-Token: $C36_CSRF"
		assert_eq '36g DELETE dataset-backed bucket -> 409' 409 "$C_STATUS"
		assert_contains '36g body carries DatasetBucketNotDeletable' "$C_BODY" 'DatasetBucketNotDeletable'
		if zfs list -H -o name "$C36_DS" >/dev/null 2>&1; then
			assert_eq '36g dataset still exists after the refusal' present present
		else
			assert_eq '36g dataset still exists after the refusal' present gone
		fi
		# Cleanup (the case owns this dataset; the API never destroys it).
		zfs destroy "$C36_DS" >/dev/null 2>&1 || true
		C36_DS=''
		rm -rf "$E2E_DATA_DIR/$C36_DS_BKT"
		curl -s -b "$C36_JAR" -X DELETE "$C36_BASE/api/buckets/$C36_DS_BKT" \
			-H "X-CSRF-Token: $C36_CSRF" >/dev/null 2>&1 || true
	fi
fi

# --- 36h: logout invalidates the session -------------------------------------
creq -X POST "$C36_BASE/logout" -H "X-CSRF-Token: $C36_CSRF"
case "$C_STATUS" in
	200|204) assert_eq '36h POST /logout -> 2xx' 2xx 2xx ;;
	*) assert_eq '36h POST /logout -> 2xx' 2xx "$C_STATUS" ;;
esac
creq "$C36_BASE/api/status"
assert_eq '36h /api/status after logout -> 401' 401 "$C_STATUS"
assert_envelope '36h post-logout 401 carries the error envelope' "$C_BODY"

printf '\n'
