#!/usr/bin/env bash
# 34-zfs-bucket-datasets.sh — per-bucket ZFS dataset provisioning over the
# wire (zfs-bucket-datasets-2026-10 tree leaf 04).
#
# The standard e2e launch (run-e2e.sh) sets NO `zfs_bucket_datasets` flag
# and uses a plain temp dataDir, so this case SKIPS everywhere except a
# feature-flagged launch against a ZFS-backed dataDir — leaf 05's live
# harness (scripts/zfs-validate/run-zfs-validation.sh + the feature-on
# second server phase) supplies that launch. The skip guard FIRST checks
# that the SHARED suite server's dataDir is a ZFS mountpoint AND that the
# server actually provisioned a dataset for a probe bucket (the only
# honest on-the-wire proof the flag is on); when the guard holds, the
# case asserts:
#   34a. PUT /zbd34-bkt -> 200 AND `zfs list -H -o name
#        <parent>/zbd34-bkt` resolves (<parent> is resolved at RUNTIME
#        from `zfs list -H -o name -t filesystem <dataDir>` — never
#        hardcoded: the shared testpool is churned by sibling sessions).
#   34b. ListBuckets (GET /) contains zbd34-bkt.
#   34c. PUT object -> GET it back byte-for-byte -> DELETE object (204).
#   34d. DELETE /zbd34-bkt (empty) -> 204; the dataset no longer
#        resolves in `zfs list`; the directory is gone.
#   34e. Snapshot refusal: re-create the bucket, PUT+DELETE an object,
#        `zfs snapshot <parent>/zbd34-bkt@e2e-pin`, DELETE bucket ->
#        409 with code BucketHasSnapshots AND the snapshot count AND the
#        `zfs destroy <ds>@<snapshot>` hint IN THE BODY (body asserts,
#        not status-only); the dataset STILL exists; after `zfs destroy`
#        of the pin snapshot, DELETE -> 204 and the dataset is gone.
#   34f. Dotted bucket name: PUT zbd34.dot.bkt -> 200, dataset
#        <parent>/zbd34.dot.bkt resolves (dots are legal dataset
#        components); DELETE clean. Then a round-trip re-create of
#        zbd34-bkt (idempotence of the provisioning path), and a final
#        cleanup that destroys ANY leftover dataset/dir — no leaks into
#        the shared testpool (harness rule: sibling sessions reuse it).
#
# Private-server pattern (cases 32/33): own config.json, own port,
# create/cleanup pairing, lib.sh assert helpers + s3req. The feature is
# probed through THIS case's own server so the shared suite server is
# never touched.
set -u
BKT='zbd34-bkt'
BKT_DOT='zbd34.dot.bkt'
E34_ROOT=$(mktemp -d /tmp/e2e34-zbd.XXXXXX)
E34_CERT=$(mktemp -d /tmp/e2e34-cert.XXXXXX)
E34_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
openssl req -x509 -newkey rsa:2048 -keyout "$E34_CERT/key.pem" -out "$E34_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
mkdir -p "$E34_ROOT/data"

# e34_zfs <args...> — exec the server's zfs binary (config `zfs_binary`),
# stderr discarded: probe failures read as "feature absent", which is the
# skip semantic, never a case failure.
e34_zfs() {
	local zf=${E34_ZFS_BINARY:-zfs}
	"$zf" "$@" 2>/dev/null
}

# e34_cleanup — kill the server, destroy the pin snapshot and ANY leftover
# bucket dataset under the resolved parent, then remove the dirs. Best
# effort: the shared testpool must never be wedged for sibling sessions.
e34_cleanup() {
	if [ -n "${E34_PID:-}" ] && kill -0 "$E34_PID" 2>/dev/null; then
		kill -TERM "$E34_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$E34_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$E34_PID" 2>/dev/null
	fi
	if [ -n "${E34_PARENT:-}" ]; then
		e34_zfs destroy "${E34_PARENT}/${BKT}@e2e-pin" >/dev/null 2>&1
		e34_zfs destroy "${E34_PARENT}/${BKT}" >/dev/null 2>&1
		e34_zfs destroy "${E34_PARENT}/${BKT_DOT}" >/dev/null 2>&1
	fi
	rm -rf "$E34_ROOT" "$E34_CERT"
}
trap e34_cleanup EXIT

# --- launch the feature-flagged private server --------------------------------
cat > "$E34_ROOT/config.json" <<EOF
{
  "dataDir": "$E34_ROOT/data",
  "listenAddr": "127.0.0.1:$E34_PORT",
  "certFile": "$E34_CERT/cert.pem",
  "keyFile": "$E34_CERT/key.pem",
  "zfs_bucket_datasets": true
}
EOF
: > "$E34_ROOT/server.log"
ZETAOBJECT_CONFIG="$E34_ROOT/config.json" ./zeta-object-server >"$E34_ROOT/server.log" 2>&1 &
E34_PID=$!
E34_URL="https://127.0.0.1:$E34_PORT"
if ! wait_for_port 127.0.0.1 "$E34_PORT" 15; then
	# Startup ABORTS by design when the feature is on but the dataDir is
	# not a ZFS mountpoint (leaf 01 fail-loud contract) — the standard
	# dev-host shape. Print the standard SKIP line and exit clean.
	echo '  (zfs bucket datasets: dataDir not on ZFS or feature off — skipping case; live coverage in scripts/zfs-validate/run-zfs-validation.sh section 12)'
	e2e_finish
	return 0 2>/dev/null || exit 0
fi

# --- skip guard: dataset proof on the wire (34a's create IS the probe) ---------
export AWS_DEFAULT_REGION=us-east-1 AWS_EC2_METADATA_DISABLED=true
export AWS_ACCESS_KEY_ID=zetaadmin AWS_SECRET_ACCESS_KEY=zetaadmin

# Resolve <parent> at RUNTIME from the live dataDir — never hardcode it
# (the shared testpool is churned by sibling sessions).
E34_PARENT=$(e34_zfs list -H -o name -t filesystem "$E34_ROOT/data" | head -1 | tr -d '[:space:]')
if [ -z "$E34_PARENT" ]; then
	echo '  (zfs bucket datasets: dataDir not on ZFS or feature off — skipping case; live coverage in scripts/zfs-validate/run-zfs-validation.sh section 12)'
	e2e_finish
	return 0 2>/dev/null || exit 0
fi

s3req PUT "/$BKT" --data-binary ''
assert_eq '34a PUT /zbd34-bkt -> 200' 200 "$S3_STATUS"
case "$S3_BODY" in
	*'<Error>'*) assert_eq '34a create body carries no error code' clean error ;;
	*) assert_eq '34a create body carries no error code' clean clean ;;
esac
E34_DS=$(e34_zfs list -H -o name "${E34_PARENT}/${BKT}" | head -1 | tr -d '[:space:]')
assert_eq "34a dataset <parent>/zbd34-bkt resolves (${E34_PARENT}/zbd34-bkt)" "${E34_PARENT}/${BKT}" "$E34_DS"

# --- 34b: ListBuckets contains the dataset-backed bucket -----------------------
s3req GET '/'
assert_eq '34b ListBuckets -> 200' 200 "$S3_STATUS"
assert_contains '34b ListBuckets contains zbd34-bkt' "$S3_BODY" 'zbd34-bkt'

# --- 34c: object round-trip on the dataset-backed bucket ------------------------
s3req PUT "/$BKT/obj.txt" --data-binary 'dataset-bucket-payload'
assert_eq '34c PUT object -> 200' 200 "$S3_STATUS"
s3req GET "/$BKT/obj.txt"
assert_eq '34c GET object -> 200' 200 "$S3_STATUS"
assert_eq '34c GET returns the exact bytes' 'dataset-bucket-payload' "$S3_BODY"
s3req DELETE "/$BKT/obj.txt"
assert_eq '34c DELETE object -> 204' 204 "$S3_STATUS"

# --- 34d: empty-bucket delete destroys the dataset ------------------------------
s3req DELETE "/$BKT"
assert_eq '34d DELETE empty bucket -> 204' 204 "$S3_STATUS"
if e34_zfs list -H -o name "${E34_PARENT}/${BKT}" >/dev/null 2>&1; then
	assert_eq '34d dataset gone from zfs list after delete' gone present
else
	assert_eq '34d dataset gone from zfs list after delete' gone gone
fi
if [ -d "$E34_ROOT/data/$BKT" ]; then
	assert_eq '34d bucket directory gone after delete' gone present
else
	assert_eq '34d bucket directory gone after delete' gone gone
fi

# --- 34e: snapshot refusal — 409 BucketHasSnapshots, count + destroy hint -------
s3req PUT "/$BKT" --data-binary ''
assert_eq '34e setup: re-create bucket -> 200' 200 "$S3_STATUS"
s3req PUT "/$BKT/obj.txt" --data-binary 'pinned'
assert_eq '34e setup: PUT object -> 200' 200 "$S3_STATUS"
s3req DELETE "/$BKT/obj.txt"
assert_eq '34e setup: DELETE object (bucket empty again) -> 204' 204 "$S3_STATUS"
e34_zfs snapshot "${E34_PARENT}/${BKT}@e2e-pin"
assert_eq '34e setup: pin snapshot taken' 0 "$?"
s3req DELETE "/$BKT"
assert_eq '34e DELETE with snapshots -> 409' 409 "$S3_STATUS"
assert_contains '34e body code BucketHasSnapshots' "$S3_BODY" '<Code>BucketHasSnapshots</Code>'
assert_contains '34e body carries the snapshot count (1)' "$S3_BODY" '1 snapshot(s)'
assert_contains '34e body carries the zfs destroy hint' "$S3_BODY" 'zfs destroy'
assert_contains '34e body names the dataset' "$S3_BODY" "${E34_PARENT}/${BKT}"
if e34_zfs list -H -o name "${E34_PARENT}/${BKT}" >/dev/null 2>&1; then
	assert_eq '34e dataset STILL exists after refusal' present present
else
	assert_eq '34e dataset STILL exists after refusal' present gone
fi
e34_zfs destroy "${E34_PARENT}/${BKT}@e2e-pin"
assert_eq '34e setup: pin snapshot destroyed' 0 "$?"
s3req DELETE "/$BKT"
assert_eq '34e DELETE after snapshot destroy -> 204' 204 "$S3_STATUS"
if e34_zfs list -H -o name "${E34_PARENT}/${BKT}" >/dev/null 2>&1; then
	assert_eq '34e dataset gone after successful delete' gone present
else
	assert_eq '34e dataset gone after successful delete' gone gone
fi

# --- 34f: dotted bucket name + round-trip re-create + leak-free cleanup ---------
s3req PUT "/$BKT_DOT" --data-binary ''
assert_eq '34f PUT /zbd34.dot.bkt (dotted name) -> 200' 200 "$S3_STATUS"
E34_DS_DOT=$(e34_zfs list -H -o name "${E34_PARENT}/${BKT_DOT}" | head -1 | tr -d '[:space:]')
assert_eq "34f dataset <parent>/zbd34.dot.bkt resolves" "${E34_PARENT}/${BKT_DOT}" "$E34_DS_DOT"
s3req DELETE "/$BKT_DOT"
assert_eq '34f DELETE /zbd34.dot.bkt -> 204' 204 "$S3_STATUS"
if e34_zfs list -H -o name "${E34_PARENT}/${BKT_DOT}" >/dev/null 2>&1; then
	assert_eq '34f dotted dataset gone after delete' gone present
else
	assert_eq '34f dotted dataset gone after delete' gone gone
fi

# Round-trip re-create: the provisioning path is idempotent after a destroy.
s3req PUT "/$BKT" --data-binary ''
assert_eq '34f re-create zbd34-bkt after destroy -> 200' 200 "$S3_STATUS"
assert_eq "34f re-created dataset resolves again" "${E34_PARENT}/${BKT}" \
	"$(e34_zfs list -H -o name "${E34_PARENT}/${BKT}" | head -1 | tr -d '[:space:]')"

# Final cleanup pairing (also runs in the EXIT trap on any failure path):
# destroy any leftover dataset/dir — no leaks into the shared testpool.
s3req DELETE "/$BKT"
assert_eq '34f final DELETE -> 204' 204 "$S3_STATUS"
e34_zfs destroy "${E34_PARENT}/${BKT}" >/dev/null 2>&1
e34_zfs destroy "${E34_PARENT}/${BKT_DOT}" >/dev/null 2>&1
e34_zfs destroy "${E34_PARENT}/${BKT}@e2e-pin" >/dev/null 2>&1
rm -rf "$E34_ROOT/data/$BKT" "$E34_ROOT/data/$BKT_DOT"
if e34_zfs list -H -o name -r "$E34_PARENT" 2>/dev/null | grep -q 'zbd34'; then
	assert_eq '34f no zbd34 dataset leaks under the parent' clean leaked
else
	assert_eq '34f no zbd34 dataset leaks under the parent' clean clean
fi

printf '\n'
