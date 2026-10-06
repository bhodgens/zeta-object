# 23-owncloud-ocs.sh — ownCloud OCS negotiation surface (owncloud-2026-09
# tree / GH issue #3, leaf 05 Task 2):
#   23a. v1+v2 capabilities/config endpoints: HTTP 200, <status>ok</status>,
#        version block present, bigfilechunking explicitly false (the
#        desktop client treats a missing flag as chunking-enabled —
#        owncloud/client#7862), NO versioning capability (provider-less
#        baseline — the honest contract).
#   23b. cloud/user: id == the authenticated access key.
#   23c. auth gate: unauthenticated v2 → HTTP 401 with OCS 997 envelope.
#   23d. degradation: unimplemented OCS endpoint → 404 + OCS envelope
#        naming the minimal-subset degradation.
#   23e. method mismatch: POST /config → 405 (v2 mapping) + envelope.
#
# REAL-CLIENT NOTE (deliberately NOT automated here): the official
# ownCloud desktop sync client cannot run in CI. The manual procedure —
# install, point at the listener, connect → sync up → sync down → delete
# — lives in docs/plans/owncloud-2026-09/decision.md §6. The curl
# assertions below pin the WIRE protocol only; do not "fix" this gap by
# faking a client in the harness.
#
# Private-server pattern (cases 14–22): the suite server is left untouched.
set -u
OC23_ROOT=$(mktemp -d /tmp/e2e23-owncloud.XXXXXX)
OC23_WORK=$(mktemp -d /tmp/e2e23-work.XXXXXX)
OC23_CERT=$(mktemp -d /tmp/e2e23-cert.XXXXXX)
OC23_BKT='e2e23-owncloud-bkt'
OC23_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
# S3 gets its own port: the owncloud frontend takes a dedicated HTTPS listener,
# so sharing the default listenAddr made two servers race for one port (whichever
# bound second died EADDRINUSE — it passed only on goroutine-scheduling luck).
OC23_S3_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
openssl req -x509 -newkey rsa:2048 -keyout "$OC23_CERT/key.pem" -out "$OC23_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
mkdir -p "$OC23_WORK/data"

OC23_USER='oc-user'
OC23_PASS='oc-pass'

oc23_cleanup() {
	if [ -n "${OC23_PID:-}" ] && kill -0 "$OC23_PID" 2>/dev/null; then
		kill -TERM "$OC23_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$OC23_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$OC23_PID" 2>/dev/null
	fi
	rm -rf "$OC23_ROOT" "$OC23_WORK" "$OC23_CERT"
}
trap oc23_cleanup EXIT

# oc23_req <url-path> [extra curl args...] — sets OC23_STATUS / OC23_BODY.
oc23_req() {
	local path=$1
	shift
	local body_file
	body_file=$(mktemp /tmp/e2e23-req.XXXXXX)
	OC23_STATUS=$(curl -sk -o "$body_file" -w '%{http_code}' \
		--user "${OC23_USER}:${OC23_PASS}" \
		"https://127.0.0.1:$OC23_PORT$path" "$@" 2>/dev/null)
	OC23_BODY=$(cat "$body_file")
	rm -f "$body_file"
}

# --- launch the private server: s3 + owncloud on one port --------------------
cat > "$OC23_WORK/config.json" <<EOF
{
  "dataDir": "$OC23_WORK/data",
  "listenAddr": "127.0.0.1:$OC23_S3_PORT",
  "certFile": "$OC23_CERT/cert.pem",
  "keyFile": "$OC23_CERT/key.pem",
  "frontends": [
    {"type": "s3"},
    {"type": "owncloud", "listenAddr": "127.0.0.1:$OC23_PORT"}
  ],
  "identities": [
    {"name": "oc-rw", "accessKey": "$OC23_USER", "secretKey": "$OC23_PASS", "grants": {"*": "readwrite"}}
  ]
}
EOF

ZETAOBJECT_ACCESS_KEY=zetaadmin ZETAOBJECT_SECRET_KEY=zetaadmin \
	ZETAOBJECT_CONFIG="$OC23_WORK/config.json" ./zeta-object-server >"$OC23_WORK/server.log" 2>&1 &
OC23_PID=$!
ENDPOINT="https://127.0.0.1:$OC23_PORT"
BASE_URL="$ENDPOINT"
export E2E_ENDPOINT="$ENDPOINT"
if ! wait_for_port 127.0.0.1 "$OC23_PORT" 15; then
	echo '  (owncloud server did not start — failing case)'
	E2E_FAIL=$((E2E_FAIL + 1))
	exit 0
fi

# --- part 23a: OCS capabilities (v1 + v2), both document paths ----------------
for OC23_EP in '/ocs/v1.php/config' '/ocs/v2.php/config' \
	'/ocs/v1.php/cloud/capabilities' '/ocs/v2.php/cloud/capabilities'; do
	oc23_req "$OC23_EP"
	assert_eq "GET $OC23_EP" 200 "$OC23_STATUS"
	assert_contains "  envelope status ok" "$OC23_BODY" '<status>ok</status>'
	assert_contains "  server version block" "$OC23_BODY" '<version><major>10</major><minor>11</minor>'
	# bigfilechunking must be present and explicitly false: the desktop client
	# treats a MISSING flag as chunking-enabled (owncloud/client#7862), so a
	# bare <files> element would silently enable chunked uploads the server
	# never assembles.
	assert_contains "  bigfilechunking explicitly false" "$OC23_BODY" '<bigfilechunking>false</bigfilechunking>'
	# Provider-less baseline: NO versioning capability may appear.
	if printf '%s' "$OC23_BODY" | grep -q 'versioning'; then
		assert_eq '  no versioning advertised (provider-less)' 'advertised' 'absent'
	else
		assert_eq '  no versioning advertised (provider-less)' 'absent' 'absent'
	fi
done

# Content type is the classic OCS XML type.
OC23_HDRS=$(mktemp /tmp/e2e23-hdrs.XXXXXX)
oc23_req '/ocs/v2.php/config' -D "$OC23_HDRS"
assert_contains 'OCS content type is text/xml' "$(cat "$OC23_HDRS")" 'text/xml; charset=UTF-8'
rm -f "$OC23_HDRS"

# --- part 23b: cloud/user -----------------------------------------------------
oc23_req '/ocs/v2.php/cloud/user'
assert_eq 'GET /ocs/v2.php/cloud/user' 200 "$OC23_STATUS"
assert_contains 'user id is the access key' "$OC23_BODY" '<id>oc-user</id>'

# --- part 23c: auth gate -------------------------------------------------------
OC23_ANON_STATUS=$(curl -sk -o /dev/null -w '%{http_code}' \
	"https://127.0.0.1:$OC23_PORT/ocs/v2.php/config" 2>/dev/null)
assert_eq 'anonymous v2 config rejected (HTTP 401)' 401 "$OC23_ANON_STATUS"
OC23_ANON_BODY=$(curl -sk "https://127.0.0.1:$OC23_PORT/ocs/v2.php/config" 2>/dev/null)
assert_contains '997 statuscode in the envelope' "$OC23_ANON_BODY" '<statuscode>997</statuscode>'
# v1 keeps HTTP 200 with the statuscode in the envelope.
OC23_V1_STATUS=$(curl -sk -o /dev/null -w '%{http_code}' \
	"https://127.0.0.1:$OC23_PORT/ocs/v1.php/config" 2>/dev/null)
assert_eq 'anonymous v1 config keeps HTTP 200 (classic mapping)' 200 "$OC23_V1_STATUS"

# --- part 23d: degradation envelope --------------------------------------------
oc23_req '/ocs/v2.php/apps/files_sharing/api/v1/shares'
assert_eq 'unimplemented endpoint (shares) 404' 404 "$OC23_STATUS"
assert_contains 'shares envelope names the degradation' "$OC23_BODY" 'minimal subset'
oc23_req '/ocs/v1.php/cloud/users'
assert_eq 'unimplemented endpoint (provisioning) v1 keeps HTTP 200' 200 "$OC23_STATUS"
assert_contains 'provisioning envelope carries 404 statuscode' "$OC23_BODY" '<statuscode>404</statuscode>'

# --- part 23e: method mismatch --------------------------------------------------
OC23_POST_STATUS=$(curl -sk -o /dev/null -w '%{http_code}' -u "$OC23_USER:$OC23_PASS" \
	-X POST "https://127.0.0.1:$OC23_PORT/ocs/v2.php/config" 2>/dev/null)
assert_eq 'POST /ocs/v2.php/config rejected (405 mapping)' 405 "$OC23_POST_STATUS"

e2e_finish
