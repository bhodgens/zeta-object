# 11-lifecycle.sh — ZETAOBJECT_LISTEN_ADDR honored (the whole suite runs on a
# non-default detected port — that IS the proof, asserted against the log),
# graceful SIGTERM shutdown (pid exits, log contains shutdown line),
# concurrent PUT+GET loop (20 iters background) never serves torn content.
# NOTE: this case drives the EXISTING suite server (E2E_SERVER_PID /
# E2E_SERVER_LOG); it must run LAST — lexical case order guarantees that.
set -u
BKT='e2e-11-lifecycle'
aws s3api create-bucket --bucket "$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

# --- listen addr proof: server log shows the detected (non-8443) port --------
PORT=$(printf '%s' "$ENDPOINT" | sed 's|.*:||')
sleep 0.5
if grep -q "127.0.0.1:$PORT" "$E2E_SERVER_LOG" || grep -q ":$PORT" "$E2E_SERVER_LOG"; then
	assert_eq "server listening on detected port $PORT (not 8443)" 0 0
else
	assert_eq "server listening on detected port $PORT (not 8443)" 0 1
fi
if grep -q '8443' "$E2E_SERVER_LOG"; then
	assert_eq 'default port 8443 NOT in server log' 0 1
else
	assert_eq 'default port 8443 NOT in server log' 0 0
fi

# --- concurrent PUT+GET loop: 20 iters, md5 never torn ------------------------
TMPD=$(mktemp -d /tmp/e2e11.XXXXXX)
python3 - "$TMPD/payload" <<'PY'
import sys, random
rng = random.Random(11)
with open(sys.argv[1], 'wb') as f:
	f.write(bytes(rng.getrandbits(8) for _ in range(256 * 1024)))
PY
GOOD_MD5=$(md5 -q "$TMPD/payload")
TORN=0
(
	for i in $(seq 1 20); do
		aws s3 cp "$TMPD/payload" "s3://$BKT/c$i.bin" \
			--endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
		aws s3 cp "s3://$BKT/c$i.bin" "$TMPD/got-$i" \
			--endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1
	done
) &
LOOP_PID=$!
wait "$LOOP_PID"
for i in $(seq 1 20); do
	M=$(md5 -q "$TMPD/got-$i" 2>/dev/null)
	[ "$M" = "$GOOD_MD5" ] || TORN=$((TORN + 1))
done
assert_eq '20 concurrent PUT+GET iters, zero torn content' 0 "$TORN"

# --- graceful SIGTERM shutdown -----------------------------------------------
kill -TERM "$E2E_SERVER_PID"
SHUTDOWN_OK=0
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
	if ! kill -0 "$E2E_SERVER_PID" 2>/dev/null; then
		SHUTDOWN_OK=1
		break
	fi
	sleep 0.5
done
assert_eq 'server pid exits within 10s of SIGTERM' 1 "$SHUTDOWN_OK"
if grep -qi 'shutdown' "$E2E_SERVER_LOG"; then
	assert_eq 'log contains shutdown line' 0 0
else
	assert_eq 'log contains shutdown line' 0 1
fi

# NOTE: after this case the suite server is DOWN by design (it is the last
# case). run-e2e's cleanup trap tolerates an already-dead pid.
aws s3 rb "s3://$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl --force >/dev/null 2>&1 || true
rm -rf "$TMPD"
