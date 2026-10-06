#!/usr/bin/env bash
# run-e2e.sh — zeta-object end-to-end suite entry point (leaf 3.6).
#
# - builds the server binary
# - generates temp certs + temp dataDir (never touches repo ./data or ./certs)
# - launches the server on a FREE port (detected, never a fixed 8443)
# - sources cases/*.sh in lexical order; each case creates + cleans its buckets
# - prints a per-case PASS/FAIL table; exits non-zero on any failure
set -u

E2E_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$E2E_ROOT/../.." && pwd)"
cd "$REPO_ROOT"

# Optional whole-suite timeout (10 min) when GNU timeout/gtimeout exists.
if command -v timeout >/dev/null 2>&1; then
	TIMEOUT_BIN=timeout
elif command -v gtimeout >/dev/null 2>&1; then
	TIMEOUT_BIN=gtimeout
else
	TIMEOUT_BIN=''
fi

# --- build -------------------------------------------------------------------
echo '== building zeta-object-server =='
if ! go build -o zeta-object-server . ; then
	echo 'FATAL: go build failed'
	exit 1
fi

# The web console is a second binary (cmd/admin-server). Build it too, but a
# failure here must NOT fail the whole suite — case 36 skips gracefully when the
# console binary is unavailable.
E2E_ADMIN_CONSOLE_AVAILABLE=0
echo '== building zeta-object-admin (console) =='
if go build -o zeta-object-admin ./cmd/admin-server && [ -x zeta-object-admin ]; then
	E2E_ADMIN_CONSOLE_AVAILABLE=1
else
	echo 'WARN: console build failed — case 36 will skip'
fi

# --- workdir, certs, config ---------------------------------------------------
WORK=$(mktemp -d /tmp/zetaobject-e2e.XXXXXX)
E2E_DATA_DIR="$WORK/data"
E2E_SENTINEL_DIR="$WORK/sentinels"
mkdir -p "$E2E_DATA_DIR" "$E2E_SENTINEL_DIR"
openssl req -x509 -newkey rsa:2048 -keyout "$WORK/key.pem" -out "$WORK/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
if [ ! -s "$WORK/cert.pem" ]; then
	echo 'FATAL: temp cert generation failed'
	exit 1
fi

# --- management-API client-certificate fixtures (case 35) ---------------------
# The admin frontend authenticates every request by TLS CLIENT certificate and
# verifies it against a configured client CA file. Generate: a trusted CA; a
# client certificate signed by it with a pinned Common Name (e2e-admin); and a
# SECOND CA with its own client certificate (same CN) for the rejection test.
# When openssl is unavailable the whole management case is skipped gracefully
# (E2E_ADMIN_AVAILABLE=0 and no admin frontend in the config).
E2E_ADMIN_AVAILABLE=0
E2E_ADMIN_CN='e2e-admin'
E2E_ADMIN_CONFIG_SECRET='e2e35-config-secret-DO-NOT-LEAK'
E2E_ADMIN_CA_DIR=''
if command -v openssl >/dev/null 2>&1; then
	E2E_ADMIN_CA_DIR="$WORK/admin-ca"
	mkdir -p "$E2E_ADMIN_CA_DIR"
	printf 'keyUsage=critical,digitalSignature\nextendedKeyUsage=clientAuth\n' > "$E2E_ADMIN_CA_DIR/client.ext"
	# trusted CA + admin client certificate.
	openssl req -x509 -newkey rsa:2048 -keyout "$E2E_ADMIN_CA_DIR/ca1-key.pem" \
		-out "$E2E_ADMIN_CA_DIR/ca1.pem" -days 1 -nodes -subj '/CN=e2e-admin-ca' >/dev/null 2>&1
	openssl req -newkey rsa:2048 -keyout "$E2E_ADMIN_CA_DIR/client-key.pem" \
		-out "$E2E_ADMIN_CA_DIR/client.csr" -nodes -subj "/CN=$E2E_ADMIN_CN" >/dev/null 2>&1
	openssl x509 -req -in "$E2E_ADMIN_CA_DIR/client.csr" -CA "$E2E_ADMIN_CA_DIR/ca1.pem" \
		-CAkey "$E2E_ADMIN_CA_DIR/ca1-key.pem" -CAcreateserial \
		-out "$E2E_ADMIN_CA_DIR/client.pem" -days 1 -extfile "$E2E_ADMIN_CA_DIR/client.ext" >/dev/null 2>&1
	# untrusted CA + its own client certificate (wrong issuer -> handshake reject).
	openssl req -x509 -newkey rsa:2048 -keyout "$E2E_ADMIN_CA_DIR/ca2-key.pem" \
		-out "$E2E_ADMIN_CA_DIR/ca2.pem" -days 1 -nodes -subj '/CN=e2e-other-ca' >/dev/null 2>&1
	openssl req -newkey rsa:2048 -keyout "$E2E_ADMIN_CA_DIR/wrong-key.pem" \
		-out "$E2E_ADMIN_CA_DIR/wrong.csr" -nodes -subj "/CN=$E2E_ADMIN_CN" >/dev/null 2>&1
	openssl x509 -req -in "$E2E_ADMIN_CA_DIR/wrong.csr" -CA "$E2E_ADMIN_CA_DIR/ca2.pem" \
		-CAkey "$E2E_ADMIN_CA_DIR/ca2-key.pem" -CAcreateserial \
		-out "$E2E_ADMIN_CA_DIR/wrong.pem" -days 1 -extfile "$E2E_ADMIN_CA_DIR/client.ext" >/dev/null 2>&1
	if [ -s "$E2E_ADMIN_CA_DIR/client.pem" ] && [ -s "$E2E_ADMIN_CA_DIR/wrong.pem" ] && [ -s "$E2E_ADMIN_CA_DIR/ca1.pem" ]; then
		E2E_ADMIN_AVAILABLE=1
	fi
fi

# --- free port (never fixed) ---------------------------------------------------
FREE_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
if [ -z "$FREE_PORT" ]; then
	echo 'FATAL: could not detect a free port'
	exit 1
fi

# Dedicated free port for the admin (management API) listener.
E2E_ADMIN_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
if [ -z "$E2E_ADMIN_PORT" ]; then
	echo 'FATAL: could not detect a free port for the admin listener'
	exit 1
fi

# Optional config fragments: the admin frontends entry, the audit log, and one
# config identity whose literal secretKey the case proves is masked by GET
# /config. Empty when the client-cert fixtures could not be built.
ADMIN_CFG_FRAGMENTS=''
AUDIT_LOG_PATH="$WORK/audit.jsonl"
if [ "$E2E_ADMIN_AVAILABLE" = 1 ]; then
	ADMIN_CFG_FRAGMENTS=$(cat <<FRAG
,
  "auditLog": {"path": "$AUDIT_LOG_PATH"},
  "identities": [
    {"name": "e2e35-config-id", "accessKey": "e2e35-config-ak", "secretKey": "$E2E_ADMIN_CONFIG_SECRET", "grants": {"*": "readwrite"}}
  ],
  "frontends": [
    {"type": "s3"},
    {"type": "admin", "listenAddr": "127.0.0.1:$E2E_ADMIN_PORT", "options": {"clientCAFile": "$E2E_ADMIN_CA_DIR/ca1.pem"}}
  ]
FRAG
)
fi

cat > "$WORK/config.json" <<EOF
{
  "dataDir": "$E2E_DATA_DIR",
  "listenAddr": ":$FREE_PORT",
  "certFile": "$WORK/cert.pem",
  "keyFile": "$WORK/key.pem"$ADMIN_CFG_FRAGMENTS
}
EOF

# --- launch server -------------------------------------------------------------
echo "== launching server on 127.0.0.1:$FREE_PORT =="
ZETAOBJECT_CONFIG="$WORK/config.json" ./zeta-object-server >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
cleanup() {
	if kill -0 "$SERVER_PID" 2>/dev/null; then
		kill -TERM "$SERVER_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$SERVER_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$SERVER_PID" 2>/dev/null
	fi
	rm -rf "$WORK"
}
trap cleanup EXIT

# shellcheck source=lib.sh
source "$E2E_ROOT/lib.sh"

# 127.0.0.1, not localhost: mc resolves localhost to ::1 first and never
# falls back to IPv4, while the server may bind v4-only (leaf 5.2 finding).
ENDPOINT="https://127.0.0.1:$FREE_PORT"
BASE_URL="$ENDPOINT"
# The host shell may carry AWS_PROFILE / AWS_REGION (e.g. a production
# profile); those override the exported credentials in the aws CLI
# (profile credentials win over AWS_ACCESS_KEY_ID). The suite must talk
# ONLY to the local server with the zetaadmin pair.
unset AWS_PROFILE AWS_DEFAULT_PROFILE AWS_REGION
export AWS_ACCESS_KEY_ID=zetaadmin AWS_SECRET_ACCESS_KEY=zetaadmin
export AWS_DEFAULT_REGION=us-east-1 AWS_EC2_METADATA_DISABLED=true

if ! wait_for_port 127.0.0.1 "$FREE_PORT" 15; then
	echo 'FATAL: server did not start listening'
	cat "$WORK/server.log"
	exit 1
fi

# Shared env for cases (leaf 10 needs the sentinel dir; leaf 11 needs the log path).
export E2E_DATA_DIR E2E_SENTINEL_DIR E2E_SERVER_LOG="$WORK/server.log" E2E_SERVER_PID="$SERVER_PID" E2E_ENDPOINT="$ENDPOINT"
# Case 35 (management API): the client-certificate fixtures, the admin listener
# URL, the audit log path, and the masked-secret literal it asserts on.
export E2E_ADMIN_AVAILABLE E2E_ADMIN_CN E2E_ADMIN_CONFIG_SECRET E2E_ADMIN_PORT
export E2E_ADMIN_URL="https://127.0.0.1:$E2E_ADMIN_PORT" E2E_AUDIT_LOG="$AUDIT_LOG_PATH"
export E2E_ADMIN_CLIENT_CERT="$E2E_ADMIN_CA_DIR/client.pem" E2E_ADMIN_CLIENT_KEY="$E2E_ADMIN_CA_DIR/client-key.pem"
export E2E_ADMIN_WRONG_CERT="$E2E_ADMIN_CA_DIR/wrong.pem" E2E_ADMIN_WRONG_KEY="$E2E_ADMIN_CA_DIR/wrong-key.pem"
# Case 36 (web console): the built console binary, the gateway listener's own
# certificate (the console trusts it as the root for the admin listener), and
# the console-build availability flag.
export E2E_ADMIN_CONSOLE_AVAILABLE E2E_ADMIN_CONSOLE_BIN="$REPO_ROOT/zeta-object-admin" E2E_SERVER_CERT="$WORK/cert.pem"

# --- run cases -----------------------------------------------------------------
CASE_RESULTS=()   # "name:PASS:FAIL"
CASE_NAMES=()
# Run each case in a nested bash so variable leakage cannot cross cases,
# then fold its counters back via the tally file it writes.
# launch_server — (re)start the suite server on a fresh free port; updates
# ENDPOINT/BASE_URL/config in place. Needed because case 11's graceful-
# shutdown proof SIGTERMs the server BY DESIGN ("must run LAST") — with the
# leaf-5.2 interop cases 12/13 sorted after it, the harness must be able to
# bring a server back for the remaining cases.
launch_server() {
	FREE_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
	cat > "$WORK/config.json" <<EOF
{
  "dataDir": "$E2E_DATA_DIR",
  "listenAddr": "127.0.0.1:$FREE_PORT",
  "certFile": "$WORK/cert.pem",
  "keyFile": "$WORK/key.pem"$ADMIN_CFG_FRAGMENTS
}
EOF
	ZETAOBJECT_CONFIG="$WORK/config.json" ./zeta-object-server >>"$WORK/server.log" 2>&1 &
	SERVER_PID=$!
	E2E_SERVER_PID="$SERVER_PID"
	# 127.0.0.1, not localhost: mc resolves localhost to ::1 first and never
	# falls back to IPv4, while the server binds v4 (leaf 5.2 finding).
	ENDPOINT="https://127.0.0.1:$FREE_PORT"
	BASE_URL="$ENDPOINT"
	export E2E_SERVER_PID E2E_ENDPOINT="$ENDPOINT"
	wait_for_port 127.0.0.1 "$FREE_PORT" 15 || {
		echo 'FATAL: relaunched server did not start listening'
		exit 1
	}
}

for case_file in "$E2E_ROOT"/cases/*.sh; do
	[ -e "$case_file" ] || { echo 'FATAL: no cases found'; exit 1; }
	name=$(basename "$case_file" .sh)
	# A previous case may have shut the server down by design (case 11).
	if ! kill -0 "$SERVER_PID" 2>/dev/null; then
		echo '  (server down — relaunching for remaining cases)'
		launch_server
	fi
	echo
	echo "== case $name =="
# Tally file is per-case (named), not shared: a case that exits EARLY
# (server-did-not-start bail) must not leave a stale tally for the NEXT
# case to read — that masked a real case failure as PASS (case 21, once).
	rm -f "$WORK/.case-tally"
	if bash -c "
		set -u
		unset AWS_PROFILE AWS_DEFAULT_PROFILE AWS_REGION
		export AWS_ACCESS_KEY_ID=zetaadmin AWS_SECRET_ACCESS_KEY=zetaadmin
		export AWS_DEFAULT_REGION=us-east-1 AWS_EC2_METADATA_DISABLED=true
		source '$E2E_ROOT/lib.sh'
		ENDPOINT='$ENDPOINT'
		BASE_URL='$BASE_URL'
		source '$case_file'
		echo \"\$E2E_PASS \$E2E_FAIL\" > '$WORK/.case-tally'
	"; then
		if [ ! -f "$WORK/.case-tally" ]; then
			# The case sourced cleanly but never asserted (e.g. it bailed
			# on server-did-not-start BEFORE any assert): that is a case
			# failure, not a pass.
			cp_=0
			cf_=1
			echo '  (case bailed before any assert — counted as failure)'
		else
			read -r cp_ cf_ < "$WORK/.case-tally"
		fi
		E2E_PASS=$((E2E_PASS + cp_))
		E2E_FAIL=$((E2E_FAIL + cf_))
	else
		# Case script itself crashed (syntax error, unbound var). Count as 1 fail.
		E2E_FAIL=$((E2E_FAIL + 1))
		cf_=1
		cp_=0
		echo '  (case script crashed — counted as failure)'
	fi
	if [ "$cf_" -eq 0 ]; then
		printf '  -> PASS (%d asserts)\n' "$cp_"
	else
		printf '  -> FAIL (%d failed asserts)\n' "$cf_"
	fi
	CASE_NAMES+=("$name")
	CASE_RESULTS+=("$name:$cp_:$cf_")
done

# --- summary table ---------------------------------------------------------------
printf '\n=== per-case results ===\n'
printf '%-28s %6s %6s %s\n' CASE PASS FAIL VERDICT
for r in "${CASE_RESULTS[@]}"; do
	IFS=: read -r n p f <<< "$r"
	v=PASS
	[ "$f" -gt 0 ] && v=FAIL
	printf '%-28s %6s %6s %s\n' "$n" "$p" "$f" "$v"
done

e2e_finish
rc=$?
echo "suite exit code: $rc"
exit "$rc"
