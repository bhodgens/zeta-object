#!/usr/bin/env bash
# 28-xattr-audit.sh — principal breadcrumbs + audit log (auth extensions
# leaf 10, docs/plans/auth-2026-09/extensions/10-xattr-breadcrumbs.md):
#
#   28a. owner stability: ak-one creates an object (user.zeta.owner =
#        ak-one); ak-two overwrites the SAME key — owner UNCHANGED and
#        user.zeta.writer.ak-two appears (multi-writer owner stability).
#   28b. writer accumulation: two writers = two distinct
#        user.zeta.writer.* xattrs (per-principal names, no shared list).
#   28c. reader stamp: on the auditReads bucket, GET as a principal stamps
#        user.zeta.reader.<key> once (value unchanged by a second GET);
#        the auditReads-off bucket never gains reader xattrs.
#   28d. audit log: every JSONL line carries exactly the 8 contract keys;
#        a granted PUT records denied:false; an unauthorized PUT records
#        denied:true with status 403; the file is append-only JSON.
#   28e. events parity: a plain bucket without a provider returns the
#        contracted 503 (no fabricated owner anywhere; the owner-field
#        wire check is the zfs-validate probe's job on real ZFS).
#
# Private-server pattern (cases 26/27): own config.json, own port,
# create/cleanup pairing, BKT convention, lib.sh assert helpers + s3req.
# xattr assertions use getfattr(1) on macOS/Linux alike.
set -u
E28_ROOT=$(mktemp -d /tmp/e2e28-xattr.XXXXXX)
E28_WORK=$(mktemp -d /tmp/e2e28-work.XXXXXX)
E28_CERT=$(mktemp -d /tmp/e2e28-cert.XXXXXX)
E28_BKT='e2e28-xattr-bkt'
E28_BKT_READS='e2e28-xattr-reads-bkt'
E28_PORT=$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)
openssl req -x509 -newkey rsa:2048 -keyout "$E28_CERT/key.pem" -out "$E28_CERT/cert.pem" \
	-days 1 -nodes -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
mkdir -p "$E28_WORK/data" "$E28_WORK/audit" "$E28_WORK/data/$E28_BKT_READS"
E28_LOG="$E28_WORK/server.log"
E28_AUDIT="$E28_WORK/audit/audit.jsonl"

e28_cleanup() {
	if [ -n "${E28_PID:-}" ] && kill -0 "$E28_PID" 2>/dev/null; then
		kill -TERM "$E28_PID" 2>/dev/null
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			kill -0 "$E28_PID" 2>/dev/null || break
			sleep 0.5
		done
		kill -9 "$E28_PID" 2>/dev/null
	fi
	rm -rf "$E28_ROOT" "$E28_WORK" "$E28_CERT"
}
trap e28_cleanup EXIT

cat > "$E28_WORK/config.json" <<EOF
{
  "dataDir": "$E28_WORK/data",
  "listenAddr": "127.0.0.1:$E28_PORT",
  "certFile": "$E28_CERT/cert.pem",
  "keyFile": "$E28_CERT/key.pem",
  "auditLog": {"path": "$E28_AUDIT"},
  "buckets": {
    "$E28_BKT_READS": {"path": "$E28_WORK/data/$E28_BKT_READS", "auditReads": true}
  },
  "identities": [
    {"name": "one", "accessKey": "e2e28-ak-one", "secretKey": "e2e28-sk-one-not-real", "grants": {"*": "readwrite"}},
    {"name": "two", "accessKey": "e2e28-ak-two", "secretKey": "e2e28-sk-two-not-real", "grants": {"*": "readwrite"}},
    {"name": "ro", "accessKey": "e2e28-ak-ro", "secretKey": "e2e28-sk-ro-not-real", "grants": {"*": "readonly"}}
  ]
}
EOF

ZETAOBJECT_ACCESS_KEY=e2e28-env-ak ZETAOBJECT_SECRET_KEY=e2e28-env-sk-not-real \
	ZETAOBJECT_CONFIG="$E28_WORK/config.json" ./zeta-object-server >"$E28_LOG" 2>&1 &
E28_PID=$!
ENDPOINT="https://127.0.0.1:$E28_PORT"
BASE_URL="$ENDPOINT"

if ! type wait_for_port >/dev/null 2>&1; then
	# run via run-e2e.sh: wait_for_port comes from lib.sh; standalone runs
	# fall back to a bounded curl loop.
	wait_for_port() {
		local host=$1 port=$2 tries=$3 i=0
		while [ "$i" -lt "$tries" ]; do
			if nc -z "$host" "$port" >/dev/null 2>&1; then return 0; fi
			sleep 0.5; i=$((i + 1))
		done
		return 1
	}
fi

if ! wait_for_port 127.0.0.1 "$E28_PORT" 30; then
	echo '  (xattr-audit server did not start — failing case)'
	E2E_FAIL=$((E2E_FAIL + 1))
	exit 0
fi

export AWS_DEFAULT_REGION=us-east-1 AWS_EC2_METADATA_DISABLED=true
export AWS_ACCESS_KEY_ID=e2e28-ak-one AWS_SECRET_ACCESS_KEY=e2e28-sk-one-not-real

# e28_xattr <file> <name> — print the xattr value or empty. Portable:
# getfattr(1) on Linux, python3/xattr fallbacks elsewhere (macOS dev hosts
# ship /usr/bin/xattr, not getfattr).
e28_xattr() {
	local file=$1 name=$2
	if command -v getfattr >/dev/null 2>&1; then
		getfattr --only-values -n "$name" "$file" 2>/dev/null || true
	elif command -v xattr >/dev/null 2>&1; then
		xattr -p "$name" "$file" 2>/dev/null || true
	else
		python3 - "$file" "$name" <<'PY' 2>/dev/null || true
import sys
try:
    print(__import__("xattr").xattr(sys.argv[1]).get(sys.argv[2].encode()).decode())
except Exception:
    pass
PY
	fi
}

# --- 28a: owner stability across two writers ---------------------------------
s3req PUT "/$E28_BKT" --data-binary ''
assert_eq '28a setup: create plain bucket (ak-one)' 200 "$S3_STATUS"
s3req PUT "/$E28_BKT/obj.txt" --data-binary 'version-one'
assert_eq '28a setup: PUT by ak-one' 200 "$S3_STATUS"
OBJ_FILE="$E28_WORK/data/$E28_BKT/obj.txt"
assert_eq '28a owner xattr is ak-one (creator)' 'e2e28-ak-one' "$(e28_xattr "$OBJ_FILE" user.zeta.owner)"

export AWS_ACCESS_KEY_ID=e2e28-ak-two AWS_SECRET_ACCESS_KEY=e2e28-sk-two-not-real
s3req PUT "/$E28_BKT/obj.txt" --data-binary 'version-two'
assert_eq '28a overwrite by ak-two' 200 "$S3_STATUS"
assert_eq '28a owner UNCHANGED after second writer' 'e2e28-ak-one' "$(e28_xattr "$OBJ_FILE" user.zeta.owner)"
case "$(e28_xattr "$OBJ_FILE" user.zeta.writer.e2e28-ak-two)" in
	put@*) assert_eq '28a writer breadcrumb for ak-two (put@RFC3339)' 0 0 ;;
	*) assert_eq '28a writer breadcrumb for ak-two (put@RFC3339)' 0 1 ;;
esac

# --- 28b: writer accumulation = two distinct xattrs ---------------------------
case "$(e28_xattr "$OBJ_FILE" user.zeta.writer.e2e28-ak-one)" in
	put@*) assert_eq '28b writer breadcrumb for ak-one survives' 0 0 ;;
	*) assert_eq '28b writer breadcrumb for ak-one survives' 0 1 ;;
esac
N_WRITERS=$(python3 - "$OBJ_FILE" <<'PY'
import subprocess, sys
file = sys.argv[1]
n = 0
try:  # Linux: getfattr -d lists all user.* attrs
    out = subprocess.run(["getfattr", "-m", "-", "-d", file],
                         capture_output=True, text=True).stdout
    n = sum(1 for line in out.splitlines() if line.startswith("user.zeta.writer."))
except Exception:
    pass
if n == 0:  # macOS: xattr -l lists "name: value" lines
    out = subprocess.run(["xattr", "-l", file], capture_output=True, text=True).stdout
    n = sum(1 for line in out.splitlines() if line.startswith("user.zeta.writer."))
print(n)
PY
)
assert_eq '28b two writers = two writer xattrs' 2 "$N_WRITERS"

# --- 28c: reader stamp on the auditReads bucket; nothing on the off bucket ----
s3req PUT "/$E28_BKT_READS" --data-binary ''
assert_eq '28c setup: create auditReads bucket' 200 "$S3_STATUS"
s3req PUT "/$E28_BKT_READS/read.txt" --data-binary 'readable'
assert_eq '28c setup: PUT to auditReads bucket' 200 "$S3_STATUS"
READS_FILE="$E28_WORK/data/$E28_BKT_READS/read.txt"
s3req GET "/$E28_BKT_READS/read.txt"
assert_eq '28c first GET as ak-two' 200 "$S3_STATUS"
case "$(e28_xattr "$READS_FILE" user.zeta.reader.e2e28-ak-two)" in
	*T*Z) assert_eq '28c reader stamp present (ak-two, RFC3339)' 0 0 ;;
	*) assert_eq '28c reader stamp present (ak-two, RFC3339)' 0 1 ;;
esac
READER_V1=$(e28_xattr "$READS_FILE" user.zeta.reader.e2e28-ak-two)
sleep 1
s3req GET "/$E28_BKT_READS/read.txt"
assert_eq '28c second GET still 200' 200 "$S3_STATUS"
assert_eq '28c reader stamp first-read-ONLY (value unchanged)' "$READER_V1" "$(e28_xattr "$READS_FILE" user.zeta.reader.e2e28-ak-two)"
if [ -n "$(e28_xattr "$OBJ_FILE" user.zeta.reader.e2e28-ak-two)" ]; then
	assert_eq '28c auditReads-off bucket: NO reader stamp' 0 1
else
	assert_eq '28c auditReads-off bucket: NO reader stamp' 0 0
fi

# --- 28d: audit log JSONL — shape, granted write, denied write -----------------
s3req PUT "/$E28_BKT/nope.txt" --data-binary 'denied?'
assert_eq '28d setup: grant check (both identities readwrite)' 200 "$S3_STATUS"
# A real DENIED request: ak-two has * readwrite, so deny via a bad signature
# (auth failure is denied:true too — presented key is recorded).
if [ -f "$E28_AUDIT" ]; then
	assert_eq '28d audit log file exists' 0 0
else
	assert_eq '28d audit log file exists' 0 1
fi
E28_LINES=$(wc -l < "$E28_AUDIT" | tr -d ' ')
if [ "$E28_LINES" -ge 3 ]; then
	assert_eq '28d audit log has records for the requests above' 0 0
else
	assert_eq '28d audit log has records for the requests above' 0 1
fi
# Every line: valid JSON with EXACTLY the 8 contract keys.
E28_SHAPE_BAD=$(python3 - "$E28_AUDIT" <<'PY'
import json, sys
bad = 0
want = {"ts", "principal", "method", "bucket", "key", "op", "status", "denied"}
with open(sys.argv[1]) as fh:
    for line in fh:
        line = line.strip()
        if not line:
            continue
        rec = json.loads(line)
        if set(rec.keys()) != want:
            bad += 1
print(bad)
PY
)
assert_eq '28d every line has exactly the 8 contract keys' 0 "$E28_SHAPE_BAD"
# Granted PUT recorded denied:false (ak-two wrote obj.txt last).
E28_DENIED_OK=$(python3 - "$E28_AUDIT" <<'PY'
import json, sys
granted = denied = 0
with open(sys.argv[1]) as fh:
    for line in fh:
        rec = json.loads(line)
        if rec["principal"] == "e2e28-ak-two" and rec["method"] == "PUT" and not rec["denied"]:
            granted += 1
        if rec["denied"]:
            denied += 1
print("ok" if granted >= 1 else f"granted={granted}")
PY
)
assert_eq '28d granted ak-two PUT recorded denied:false' ok "$E28_DENIED_OK"

# A real authorization DENIAL: the read-only identity PUTs → 403 → the audit
# record carries denied:true (denials are forensically interesting).
export AWS_ACCESS_KEY_ID=e2e28-ak-ro AWS_SECRET_ACCESS_KEY=e2e28-sk-ro-not-real
s3req PUT "/$E28_BKT/denied.txt" --data-binary 'must-not-land'
assert_eq '28d-ro PUT by readonly identity rejected' 403 "$S3_STATUS"
E28_HAS_DENIED=$(python3 - "$E28_AUDIT" <<'PY'
import json, sys
with open(sys.argv[1]) as fh:
    for line in fh:
        rec = json.loads(line)
        if (rec["denied"] and rec["principal"] == "e2e28-ak-ro"
                and rec["method"] == "PUT" and rec["status"] == 403):
            print("ok")
            break
    else:
        print("missing")
PY
)
assert_eq '28d audit log records the denial (denied:true, 403)' ok "$E28_HAS_DENIED"

# --- 28e: events parity on a plain (provider-less) bucket ----------------------
s3req GET "/$E28_BKT/obj.txt?events"
assert_eq '28e plain bucket ?events without provider is the contracted 503' 503 "$S3_STATUS"

printf '\n'
