#!/usr/bin/env bash
# run-conformance.sh — ceph/s3-tests conformance baseline runner (leaf 5.1).
#
# Mirrors scripts/e2e/run-e2e.sh lifecycle:
#   - builds the server binary
#   - generates temp certs + temp dataDir (never touches repo ./data or ./certs)
#   - launches the server on a FREE port (default target: 18499)
#   - runs the vendored ceph/s3-tests (vendor/s3-tests, gitignored) pytest
#     subset selected by the frozen in-scope marker/-k expressions below
#   - writes the raw pytest -v log to /tmp and prints a per-group summary
#   - compares per-group pass/fail counts against
#     scripts/conformance/baseline.txt (the committed ratchet):
#     exit non-zero ONLY on regressions (a test passing in the baseline
#     that now fails, or an error where there was none).
#
# Environment:
#   ZETAOBJECT_CONFORMANCE_PORT  preferred port (default 18499; a free port is
#                            substituted if busy)
#   ZETAOBJECT_CONFORMANCE_HTTP=1  fallback: not needed — the suite accepts the
#                            self-signed cert via ssl_verify=false. Kept as a
#                            documented no-op hook; see docs/conformance/.
set -u

CONF_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$CONF_ROOT/../.." && pwd)"
cd "$REPO_ROOT"

S3TESTS_DIR="vendor/s3-tests"
VENV_DIR="vendor/s3-tests-venv"
PY="$VENV_DIR/bin/python"
BASELINE="$CONF_ROOT/baseline.txt"

# --- vendored suite -----------------------------------------------------------
if [ ! -d "$S3TESTS_DIR" ]; then
	echo "== shallow-cloning ceph/s3-tests into $S3TESTS_DIR =="
	git clone --depth 1 https://github.com/ceph/s3-tests.git "$S3TESTS_DIR" || {
		echo 'FATAL: clone failed'; exit 1; }
fi

# --- venv (created once, reused) ----------------------------------------------
if [ ! -x "$PY" ]; then
	echo "== creating venv + installing requirements =="
	python3 -m venv "$VENV_DIR" || { echo 'FATAL: venv creation failed'; exit 1; }
	"$PY" -m pip install -q -r "$S3TESTS_DIR/requirements.txt" || {
		echo 'FATAL: pip install failed'; exit 1; }
fi

# --- build ---------------------------------------------------------------------
echo '== building zeta-object-server =='
go build -o zeta-object-server . || { echo 'FATAL: go build failed'; exit 1; }

# --- workdir, certs, config ------------------------------------------------------
WORK=$(mktemp -d /tmp/zetaobject-conf.XXXXXX)
mkdir -p "$WORK/data"
openssl req -x509 -newkey rsa:2048 -keyout "$WORK/key.pem" -out "$WORK/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
if [ ! -s "$WORK/cert.pem" ]; then
	echo 'FATAL: temp cert generation failed'
	exit 1
fi

# --- port: prefer the configured one, fall back to a free port -------------------
PREFERRED_PORT="${ZETAOBJECT_CONFORMANCE_PORT:-18499}"
port_in_use() { python3 -c "
import socket,sys
s=socket.socket()
try:
    s.connect(('127.0.0.1', $1)); sys.exit(0)
except OSError:
    sys.exit(1)
" 2>/dev/null; }
if port_in_use "$PREFERRED_PORT"; then
	FREE_PORT=$(python3 -c '
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
	echo "== port $PREFERRED_PORT busy, using $FREE_PORT =="
else
	FREE_PORT="$PREFERRED_PORT"
fi

cat > "$WORK/config.json" <<EOF
{
  "dataDir": "$WORK/data",
  "listenAddr": ":$FREE_PORT",
  "certFile": "$WORK/cert.pem",
  "keyFile": "$WORK/key.pem"
}
EOF

# s3tests conf: the committed template with the actual port substituted.
sed "s/^port = .*/port = $FREE_PORT/" "$CONF_ROOT/s3tests.conf" > "$WORK/s3tests.conf"

# --- launch server ---------------------------------------------------------------
echo "== launching zeta-object on 127.0.0.1:$FREE_PORT (HTTPS, self-signed) =="
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

wait_for_port() {
	for _ in $(seq 1 30); do
		if port_in_use "$2"; then return 0; fi
		sleep 0.5
	done
	return 1
}
if ! wait_for_port x "$FREE_PORT"; then
	echo 'FATAL: server did not start listening'
	cat "$WORK/server.log"
	exit 1
fi

# --- test selection (frozen; see docs/plans/conformance-2026-09/5.1-conformance.md)
# Out-of-scope FEATURES deselected by suite-defined pytest markers:
MARKER_DESELECT="not (auth_aws2 or bucket_encryption or bucket_logging or bucket_logging_cleanup or bucket_policy or checksum or cloud_restore or cloud_transition or conditional_write or delete_marker or encryption or lifecycle or lifecycle_expiration or lifecycle_transition or object_ownership or sse_s3 or tagging or target_by_bucket or fails_without_logging_rollover)"
# Remaining out-of-scope families (versioning, object lock, CORS, website,
# s3select, storage class, ACL, policy/POST, SSE-C, encryption, lifecycle,
# logging, checksum, conditional write, tenants, BPA/ownership) are not all
# marker-tagged, so also deselect by name (-k):
K_DESELECT="not (version or lock or cors or acl or tag or policy or sse or encrypt or lifecycle or logging or checksum or conditional or post_object or tenant or ownership)"
S3TEST_FILES=(
	"$S3TESTS_DIR/s3tests/functional/test_s3.py"
	"$S3TESTS_DIR/s3tests/functional/test_headers.py"
)

# --- run ----------------------------------------------------------------------------
echo '== running pytest subset =='
S3TEST_CONF="$WORK/s3tests.conf" AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin \
	"$VENV_DIR/bin/pytest" "${S3TEST_FILES[@]}" \
	-m "$MARKER_DESELECT" \
	-k "$K_DESELECT" \
	-v --tb=no -rf \
	> "$WORK/pytest.log" 2>&1
PYTEST_RC=$?

RAW_LOG="/tmp/zetaobject-conformance-pytest.log"
cp "$WORK/pytest.log" "$RAW_LOG" 2>/dev/null || RAW_LOG="$PWD/pytest.log"
echo "raw pytest log: $RAW_LOG (pytest rc=$PYTEST_RC)"
tail -3 "$WORK/pytest.log"

# --- summarize + ratchet ------------------------------------------------------------
if ! S3TEST_CONF="$WORK/s3tests.conf" "$PY" "$CONF_ROOT/summarize.py" "$WORK/pytest.log" "$BASELINE"; then
	echo 'FAIL: conformance ratchet regressed (see group table above)'
	exit 1
fi
echo 'conformance ratchet OK (no regressions vs baseline)'
exit 0
