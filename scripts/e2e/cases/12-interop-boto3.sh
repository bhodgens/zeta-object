# 12-interop-boto3.sh — leaf 5.2: the shared interop scenario table through
# python boto3. Scenario table + parity rule live in interop-lib.sh; the
# frozen expectations are hardcoded from the aws-cli e2e cases (the suite
# IS the expected-behavior spec). Client-side parse failures are FINDINGS
# (recorded by scenario_pass, triaged at the bottom of this file).
set -u
E2E_ROOT="${BASH_SOURCE[0]%/*}/.."
E2E_ROOT=$(cd "$E2E_ROOT" && pwd)
REPO_ROOT=$(cd "$E2E_ROOT/../.." && pwd)
# shellcheck source=../interop-lib.sh
source "$E2E_ROOT/interop-lib.sh"

# --- locate / create the boto3 venv ------------------------------------------
# Leaf 5.1's ceph-suite venv provides boto3 too — reuse when present; else
# this leaf's own vendor/boto3-venv (gitignored, boto3 only).
BOTO3_PY=''
for cand in \
	"$REPO_ROOT/vendor/s3-tests/.venv/bin/python" \
	"$REPO_ROOT/vendor/boto3-venv/bin/python"; do
	if [ -x "$cand" ] && "$cand" -c 'import boto3' >/dev/null 2>&1; then
		BOTO3_PY=$cand
		break
	fi
done
if [ -z "$BOTO3_PY" ]; then
	echo '  (boto3 venv missing — creating vendor/boto3-venv with boto3)'
	if python3 -m venv "$REPO_ROOT/vendor/boto3-venv" \
		&& "$REPO_ROOT/vendor/boto3-venv/bin/pip" -q install boto3; then
		BOTO3_PY="$REPO_ROOT/vendor/boto3-venv/bin/python"
	else
		echo '  FATAL: could not provision a boto3 venv'
		assert_eq 'boto3 venv provisionable' yes no
		exit 0
	fi
fi

BKT='e2e-12-interop'
export INTEROP_ENDPOINT="$ENDPOINT"
INTEROP_SHIM_ERR=$(mktemp /tmp/e2e12-shimerr.XXXXXX)
INTEROP_BOTO3_SHIM="$BOTO3_PY $E2E_ROOT/boto3-runner.py"
INTEROP_EXPECT_DATA=''

# Canonical payload: every client hashes this same content.
PAYLOAD='interop-payload-12'
PAYLOAD_HASH=$(printf %s "$PAYLOAD" | shasum -a 256 | cut -c1-16)

# --- bucket lifecycle (parity: case 01) --------------------------------------
scenario_pass boto3 bucket_create "$BKT"
scenario_pass boto3 bucket_head "$BKT"
scenario_pass boto3 bucket_head_missing 'e2e-12-no-such-bucket-xyz'
scenario_pass boto3 bucket_list "$BKT"
scenario_pass boto3 bucket_create_dup "$BKT"

# --- object round trip + unicode (parity: case 02) ---------------------------
scenario_pass boto3 object_put "$BKT" "$PAYLOAD" plain.txt
INTEROP_EXPECT_DATA="$PAYLOAD_HASH"
scenario_pass boto3 object_get_roundtrip "$BKT" plain.txt
INTEROP_EXPECT_DATA=''
# unicode_key asserts its own byte-exact round-trip (data=unicode-ok)
scenario_pass boto3 unicode_key "$BKT"

# --- error codes (parity: cases 01–03) ---------------------------------------
scenario_pass boto3 error_nosuchbucket
scenario_pass boto3 error_nosuchkey "$BKT"
scenario_pass boto3 error_accessdenied "$BKT" plain.txt

# --- listing with prefix (parity: case 04) -----------------------------------
scenario_pass boto3 object_put "$BKT" "$PAYLOAD" pre/one.txt
scenario_pass boto3 object_put "$BKT" "$PAYLOAD" pre/two.txt
scenario_pass boto3 object_put "$BKT" "$PAYLOAD" other.txt
INTEROP_EXPECT_DATA=2
scenario_pass boto3 list_prefix "$BKT"
INTEROP_EXPECT_DATA=''

# --- range GET (parity: case 06) ---------------------------------------------
RANGE_BIN=$(mktemp /tmp/e2e12-range.XXXXXX)
python3 -c "print(''.join(chr(97 + i % 26) for i in range(119)), end='')" \
	>"$RANGE_BIN"
"$BOTO3_PY" "$E2E_ROOT/boto3-runner.py" object_put_file "$BKT" r.bin \
	"$RANGE_BIN" >/dev/null 2>&1
scenario_pass boto3 range_get "$BKT" r.bin
scenario_pass boto3 range_get_invalid "$BKT" r.bin
rm -f "$RANGE_BIN"

# --- multipart round trip (parity: case 05) ----------------------------------
MPU_HASH=$(python3 -c '
import hashlib
P1 = b"p1-" + b"a" * (5*1024*1024 - 3)
P2 = b"p2-" + b"b" * (5*1024*1024 - 3)
P3 = b"final-part-tail"
print(hashlib.sha256(P1+P2+P3).hexdigest()[:16])')
INTEROP_EXPECT_DATA="$MPU_HASH"
scenario_pass boto3 multipart_roundtrip "$BKT"
INTEROP_EXPECT_DATA=''

# --- presigned GET (parity: case 07) -----------------------------------------
INTEROP_EXPECT_DATA="$PAYLOAD_HASH"
scenario_pass boto3 presigned_get "$BKT" plain.txt
INTEROP_EXPECT_DATA=''

# --- copy (parity: case 08) ---------------------------------------------------
INTEROP_EXPECT_DATA="$PAYLOAD_HASH"
scenario_pass boto3 copy_object "$BKT" dst.txt "$BKT" plain.txt
INTEROP_EXPECT_DATA=''

# --- batch delete (parity: case 08: 2 exist + 1 missing → 3 deleted) ---------
INTEROP_EXPECT_DATA=3
scenario_pass boto3 batch_delete "$BKT" plain.txt dst.txt ghost.txt
INTEROP_EXPECT_DATA=''

# --- cleanup -------------------------------------------------------------------
"$BOTO3_PY" "$E2E_ROOT/boto3-runner.py" bucket_delete "$BKT" >/dev/null 2>&1
rm -f "$INTEROP_SHIM_ERR"

# --- findings triage (parity rule: parse failures are findings) ----------------
if [ -n "$INTEROP_FINDINGS" ]; then
	printf '  FINDINGS (client-side parse failures — triage [a] fix wire format / [b] divergence / [c] client bug+skip):\n'
	printf '%s\n' "$INTEROP_FINDINGS" | tr ' ' '\n' | sed '/^$/d; s/^/    /'
fi
