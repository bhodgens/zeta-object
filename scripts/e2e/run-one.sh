#!/usr/bin/env bash
# run-one.sh — run a SINGLE e2e case against a fresh scratch server.
#
# Why this exists: the full suite (make e2e) starts one long-lived server and
# runs 40 cases against it in lexical order. When one case fails you usually
# want ONE case, ONE server, ONE temp dataDir, and the server log - which is
# exactly what this gives, with nothing left behind.
#
# Usage:
#   scripts/e2e/run-one.sh 39-bucket-name-gate     # one case, fresh server
#   scripts/e2e/run-one.sh 18-auth-identities      # any case under cases/
#
# Notes:
#   - The CREDENTIAL must match the server's built-in default (config.go's
#     defaultAccessKey, "zetaadmin"). It was renamed from minioadmin in
#     a7f5efb; a stale value here makes every SigV4 call 403 while the
#     helper itself looks fine, which reads as "the server is broken".
#     run-e2e.sh exports the same pair for the same reason.
#   - The generated config sets no "auth" key, so the server runs in
#     REQUIRED-auth mode (not the auth:"none" dev wildcard). A case that
#     passes here really did authenticate.
#   - Credentials come from the environment when set, so an operator can run
#     against a server with a custom pair.
#   - The tally is read back from a file, the same trick run-e2e.sh uses: a
#     case that exits early (or crashes) must not be mistaken for a pass, and
#     sourcing a case in this shell would leave its variables behind for the
#     next invocation of the helper.
set -u
E2E_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT=$(cd "$E2E_ROOT/../.." && pwd)
cd "$REPO_ROOT" || exit 1

CASE=${1:-}
if [ -z "$CASE" ]; then
	echo "usage: $(basename "$0") <case-name-without-.sh>" >&2
	echo "available:" >&2
	ls -1 "$E2E_ROOT/cases" | sed 's/\.sh$//' | sed 's/^/  /' >&2
	exit 2
fi
CASE_FILE="$E2E_ROOT/cases/$CASE.sh"
if [ ! -f "$CASE_FILE" ]; then
	echo "no such case: $CASE_FILE" >&2
	exit 2
fi

# The server binary is a build product; build it if it is missing so a fresh
# checkout can run a case without a separate `make build`.
if [ ! -x ./zeta-object-server ]; then
	echo "building ./zeta-object-server ..."
	go build -o zeta-object-server . || exit 1
fi

WORK=$(mktemp -d /tmp/zeta-one.XXXXXX)
mkdir -p "$WORK/data"
openssl req -x509 -newkey rsa:2048 -keyout "$WORK/key.pem" -out "$WORK/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1

# 127.0.0.1, not localhost: mc resolves localhost to ::1 first and never falls
# back to IPv4, while the server binds v4 (run-e2e.sh's launcher note).
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
  "dataDir": "$WORK/data",
  "listenAddr": "127.0.0.1:$FREE_PORT",
  "certFile": "$WORK/cert.pem",
  "keyFile": "$WORK/key.pem"
}
EOF
ZETAOBJECT_CONFIG="$WORK/config.json" ./zeta-object-server >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
ENDPOINT="https://127.0.0.1:$FREE_PORT"
BASE_URL="$ENDPOINT"

cleanup() {
	kill -9 "$SERVER_PID" 2>/dev/null
	# Keep the temp dir when the run failed or ZETAONE_KEEP=1: the dataDir and
	# the server log are the evidence a failing case produced.
	if [ "${ZETAONE_KEEP:-0}" = "1" ] || [ "${CASE_RC:-0}" != "0" ]; then
		echo "artifacts kept in $WORK"
		echo "  server log: $WORK/server.log"
		echo "  data dir:   $WORK/data"
		return
	fi
	rm -rf "$WORK"
}
trap cleanup EXIT

source "$E2E_ROOT/lib.sh"
wait_for_port 127.0.0.1 "$FREE_PORT" 15 || {
	echo 'server did not start; log follows:'
	cat "$WORK/server.log"
	exit 1
}

# The credential: the server's built-in default unless the operator overrides
# it. zetaadmin is config.go's defaultAccessKey (a7f5efb renamed it from
# minioadmin; a stale value here 403s every signed call).
E2E_AK=${ZETAOBJECT_ACCESS_KEY:-zetaadmin}
E2E_SK=${ZETAOBJECT_SECRET_KEY:-zetaadmin}

rm -f "$WORK/.case-tally"
CASE_RC=0
bash -c "
	set -u
	unset AWS_PROFILE AWS_DEFAULT_PROFILE AWS_REGION
	export AWS_ACCESS_KEY_ID='$E2E_AK' AWS_SECRET_ACCESS_KEY='$E2E_SK'
	export AWS_DEFAULT_REGION=us-east-1 AWS_EC2_METADATA_DISABLED=true
	export E2E_DATA_DIR='$WORK/data' E2E_SENTINEL_DIR='$WORK/sentinels' \
	       E2E_SERVER_LOG='$WORK/server.log' E2E_SERVER_PID='$SERVER_PID' \
	       E2E_ENDPOINT='$ENDPOINT' \
	       E2E_ADMIN_CONSOLE_AVAILABLE='${E2E_ADMIN_CONSOLE_AVAILABLE:-}' \
	       E2E_ADMIN_CONSOLE_BIN='${E2E_ADMIN_CONSOLE_BIN:-$REPO_ROOT/zeta-object-admin}' \
	       E2E_SERVER_CERT='$WORK/cert.pem'
	source '$E2E_ROOT/lib.sh'
	ENDPOINT='$ENDPOINT'
	BASE_URL='$BASE_URL'
	source '$CASE_FILE'
	echo \"\$E2E_PASS \$E2E_FAIL\" > '$WORK/.case-tally'
" || CASE_RC=$?

# Fold the tally back. A missing tally file means the case exited before its
# first assert (a server-did-not-start bail, say): that is a FAILURE, never a
# pass - the same rule run-e2e.sh applies per case.
if [ -f "$WORK/.case-tally" ]; then
	read -r cp cf < "$WORK/.case-tally"
else
	cp=0
	cf=1
	echo '(case bailed before any assert — counted as failure)'
fi
if [ "$CASE_RC" -ne 0 ]; then
	echo "(case script exited $CASE_RC — counted as failure)"
	cf=$((cf + 1))
fi

echo
printf 'case %s: %d passed, %d failed\n' "$CASE" "$cp" "$cf"
if [ "$cf" -ne 0 ]; then
	echo "--- server log (tail 30) ---"
	tail -30 "$WORK/server.log" 2>/dev/null
	exit 1
fi
echo 'PASS'
exit 0
