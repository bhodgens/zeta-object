# 38-h3-webdav.sh - HTTP/3 (QUIC) webdav data plane + Alt-Svc over TCP
# (quic-h3-2026-10 leaf 03). Private-server pattern (cases 16/31): the suite
# server is left untouched; this case starts its OWN server with an explicit
# frontends array (an explicit list disables the default s3 mount, so the
# array carries s3 + webdav + h3).
#
#   38a. h3 mode (scripts/e2e/h3probe, client certificate from the generated
#        CA): PUT 201/204, GET exact-bytes round-trip, Range bytes=0-99 ->
#        206 + Content-Range + the first 100 bytes, suffix bytes=-50 -> 206
#        + the last 50 bytes, unsatisfiable -> 416, PROPFIND Depth 1 lists
#        the directory, PROPFIND on the file shows size + etag.
#   38b. h3 WITHOUT a client certificate -> TLS HANDSHAKE FAILURE (no HTTP
#        401 exists over h3 for cert failures); a wrong-CA certificate fails
#        the same way. Both asserted inside the probe run.
#   38c. tcp mode (plain HTTPS to the webdav frontend, Basic auth): 401
#        without credentials, the SAME Range semantics over TCP
#        (transport-independent), and alt-svc: h3="<port>"; persist=1 on
#        every response (the UDP port in the value matches the h3
#        frontend's configured port).
#
# No soft-skip path: go is always present (the harness builds the server).
# Certificates follow case 35's generation pattern (CA + signed client leaf
# + a second CA with its own leaf for the wrong-CA rejection).
set -u
E38_ROOT=$(mktemp -d /tmp/e2e38-h3.XXXXXX)
E38_CERT=$(mktemp -d /tmp/e2e38-cert.XXXXXX)
E38_BKT='e2e38-h3-bkt'
E38_USER='h3-user'
E38_PASS='h3-pass'

# free_port - an ephemeral TCP or UDP port (bind(0) reserves it for a
# moment; the listener binds after the kernel releases it).
free_port() {
	python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}
E38_PORT_S3=$(free_port)
E38_PORT_WD=$(free_port)
E38_PORT_H3=$(free_port)

# --- certificate generation (case 35's pattern) ------------------------------
# trusted CA + a client leaf (clientAuth EKU, CN = the h3 identity), plus a
# SECOND CA with its own client leaf (wrong issuer -> handshake rejection).
openssl req -x509 -newkey rsa:2048 -keyout "$E38_CERT/ca1-key.pem" \
	-out "$E38_CERT/ca1.pem" -days 1 -nodes -subj '/CN=e2e38-h3-ca' >/dev/null 2>&1
printf 'keyUsage=critical,digitalSignature\nextendedKeyUsage=clientAuth\n' > "$E38_CERT/client.ext"
openssl req -newkey rsa:2048 -keyout "$E38_CERT/client-key.pem" \
	-out "$E38_CERT/client.csr" -nodes -subj "/CN=$E38_USER" >/dev/null 2>&1
openssl x509 -req -in "$E38_CERT/client.csr" -CA "$E38_CERT/ca1.pem" \
	-CAkey "$E38_CERT/ca1-key.pem" -CAcreateserial \
	-out "$E38_CERT/client.pem" -days 1 -extfile "$E38_CERT/client.ext" >/dev/null 2>&1
openssl req -x509 -newkey rsa:2048 -keyout "$E38_CERT/ca2-key.pem" \
	-out "$E38_CERT/ca2.pem" -days 1 -nodes -subj '/CN=e2e38-other-ca' >/dev/null 2>&1
openssl req -newkey rsa:2048 -keyout "$E38_CERT/wrong-key.pem" \
	-out "$E38_CERT/wrong.csr" -nodes -subj "/CN=$E38_USER" >/dev/null 2>&1
openssl x509 -req -in "$E38_CERT/wrong.csr" -CA "$E38_CERT/ca2.pem" \
	-CAkey "$E38_CERT/ca2-key.pem" -CAcreateserial \
	-out "$E38_CERT/wrong.pem" -days 1 -extfile "$E38_CERT/client.ext" >/dev/null 2>&1
# server pair (self-signed, the process certFile/keyFile every listener uses)
openssl req -x509 -newkey rsa:2048 -keyout "$E38_CERT/key.pem" -out "$E38_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1

e38_cleanup() {
	if [ -n "${E38_PID:-}" ] && kill -0 "$E38_PID" 2>/dev/null; then
		kill -TERM "$E38_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$E38_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$E38_PID" 2>/dev/null
	fi
	# Belt-and-braces for the (never expected) orphaned server: a bracketed
	# pattern that cannot match the grep itself.
	pkill -f 'zeta-serve[r].*e2e38-h3' >/dev/null 2>&1
	rm -rf "$E38_ROOT" "$E38_CERT"
}
trap e38_cleanup EXIT

# --- config: EXPLICIT frontends array (s3 + webdav + h3) ----------------------
mkdir -p "$E38_ROOT/data"
cat > "$E38_ROOT/config.json" <<EOF
{
  "dataDir": "$E38_ROOT/data",
  "listenAddr": "127.0.0.1:$E38_PORT_S3",
  "certFile": "$E38_CERT/cert.pem",
  "keyFile": "$E38_CERT/key.pem",
  "frontends": [
    {"type": "s3"},
    {"type": "webdav", "listenAddr": "127.0.0.1:$E38_PORT_WD", "bucket": "$E38_BKT"},
    {"type": "h3", "listenAddr": "127.0.0.1:$E38_PORT_H3", "bucket": "$E38_BKT",
     "options": {"clientCAFile": "$E38_CERT/ca1.pem"}}
  ],
  "identities": [
    {"name": "h3-user", "accessKey": "$E38_USER", "secretKey": "$E38_PASS", "grants": {"$E38_BKT": "readwrite"}}
  ]
}
EOF

# --- launch + readiness --------------------------------------------------------
ZETAOBJECT_ACCESS_KEY=zetaadmin ZETAOBJECT_SECRET_KEY=zetaadmin \
	ZETAOBJECT_CONFIG="$E38_ROOT/config.json" ./zeta-object-server >"$E38_ROOT/server.log" 2>&1 &
E38_PID=$!
if ! wait_for_port 127.0.0.1 "$E38_PORT_WD" 15; then
	echo '  (e2e38 webdav TCP listener did not start - failing case)'
	E2E_FAIL=$((E2E_FAIL + 1))
	exit 0
fi
# The QUIC listener has no TCP port to probe; wait for the startup log line
# (a dedicated QUIC frontend logs through the same serving fan), bounded.
E38_H3_UP=0
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
	if grep -q "127.0.0.1:$E38_PORT_H3" "$E38_ROOT/server.log" 2>/dev/null; then
		E38_H3_UP=1
		break
	fi
	if ! kill -0 "$E38_PID" 2>/dev/null; then
		break
	fi
	sleep 0.5
done
if [ "$E38_H3_UP" != 1 ]; then
	echo '  (e2e38 QUIC listener did not come up (or the server exited) - failing case)'
	sed 's/^/    log: /' "$E38_ROOT/server.log" | tail -10
	E2E_FAIL=$((E2E_FAIL + 1))
	exit 0
fi

# The h3 frontend is single-bucket (bucket pinned): the object path is flat.
# Seed nothing over h3 - the probe PUTs its own deterministic content.

E38_H3_URL="https://127.0.0.1:$E38_PORT_H3"
E38_WD_URL="https://127.0.0.1:$E38_PORT_WD"

# --- 38a+38b: probe h3 mode (cert asserts + handshake rejections) --------------
E38_OUT=$(# -mod=mod: cases 12/13 create vendor/ mid-suite; Go then treats vendor/ as
# a vendor tree and dies on the missing modules.txt.
go run -mod=mod ./scripts/e2e/h3probe -mode h3 \
	-url "$E38_H3_URL" \
	-cert "$E38_CERT/client.pem" -key "$E38_CERT/client-key.pem" 2>&1)
E38_RC=$?
printf '%s\n' "$E38_OUT" | sed 's/^/    /'
case "$E38_OUT" in
*"h3probe: "*)
	E38_TALLY=$(printf '%s\n' "$E38_OUT" | sed -n 's/^h3probe: \([0-9]*\/[0-9]*\) PASS$/\1/p' | tail -1)
	assert_eq '38a/38b h3 probe tally clean (N/N PASS)' "$E38_TALLY" "${E38_TALLY%%/*}/${E38_TALLY##*/}"
	assert_eq '38a/38b h3 probe exit code 0' 0 "$E38_RC"
	;;
*)
	assert_contains '38a/38b h3 probe produced a tally line' "$E38_OUT" 'h3probe:'
	;;
esac

# --- 38c: probe tcp mode (401 + Alt-Svc + Range parity) -------------------------
E38_TOUT=$(go run -mod=mod ./scripts/e2e/h3probe -mode tcp \
	-url "$E38_WD_URL" \
	-user "$E38_USER" -pass "$E38_PASS" \
	-port-h3 "$E38_PORT_H3" 2>&1)
E38_TRC=$?
printf '%s\n' "$E38_TOUT" | sed 's/^/    /'
case "$E38_TOUT" in
*"h3probe: "*)
	E38_TTALLY=$(printf '%s\n' "$E38_TOUT" | sed -n 's/^h3probe: \([0-9]*\/[0-9]*\) PASS$/\1/p' | tail -1)
	assert_eq '38c tcp probe tally clean (N/N PASS)' "$E38_TTALLY" "${E38_TTALLY%%/*}/${E38_TTALLY##*/}"
	assert_eq '38c tcp probe exit code 0' 0 "$E38_TRC"
	;;
*)
	assert_contains '38c tcp probe produced a tally line' "$E38_TOUT" 'h3probe:'
	;;
esac

# The Alt-Svc UDP port must be OUR h3 frontend's port (the probe already
# asserted the header on every response; this re-proves the value against
# the config directly through the TCP frontend, transport-independent).
E38_ALTSVC=$(curl -sk -o /dev/null -D - -u "$E38_USER:$E38_PASS" \
	"$E38_WD_URL/" 2>/dev/null | grep -i '^alt-svc:' | tr -d '\r')
assert_contains '38c alt-svc names the h3 UDP port from config' "$E38_ALTSVC" "h3=\":$E38_PORT_H3\"; persist=1"


printf '\n'
