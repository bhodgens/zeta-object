# 13-interop-mc.sh — leaf 5.2: the shared interop scenario table through the
# the mc client. Same frozen expectations as case 12. Scenarios mc cannot
# express (raw Range headers, manual 3-phase multipart, object-GET NoSuchKey
# surfaced as its own outcome) assert the CLOSEST observable, reason commented
# inline — per the leaf's tolerance clause. Client-side parse failures are
# FINDINGS (recorded by scenario_pass, triaged at the bottom).
set -u
E2E_ROOT="${BASH_SOURCE[0]%/*}/.."
E2E_ROOT=$(cd "$E2E_ROOT" && pwd)
REPO_ROOT=$(cd "$E2E_ROOT/../.." && pwd)
# shellcheck source=../interop-lib.sh
source "$E2E_ROOT/interop-lib.sh"

# --- mc binary bootstrap -------------------------------------------------------
# vendor/mc (gitignored): official release binary for the host OS/arch,
# downloaded at case runtime if absent — no system install. dl.min.io now
# 410s the community downloads (project archived 2026-07); the official
# GitHub release assets remain authoritative:
# https://github.com/minio/mc/releases/download/<TAG>/mc.<GOOS>-<GOARCH>.<TAG>
MC_RELEASE_TAG='RELEASE.2025-08-13T08-35-41Z'
MC_BIN="$REPO_ROOT/vendor/mc"
if [ ! -x "$MC_BIN" ]; then
	case "$(uname -s)/$(uname -m)" in
	Darwin/arm64)                 MC_ASSET="mc.darwin-arm64.$MC_RELEASE_TAG" ;;
	Darwin/x86_64)                MC_ASSET="mc.darwin-amd64.$MC_RELEASE_TAG" ;;
	Linux/arm64 | Linux/aarch64)  MC_ASSET="mc.linux-arm64.$MC_RELEASE_TAG" ;;
	*)                            MC_ASSET="mc.linux-amd64.$MC_RELEASE_TAG" ;;
	esac
	MC_URL="https://github.com/minio/mc/releases/download/$MC_RELEASE_TAG/$MC_ASSET"
	echo "  (downloading mc: $MC_URL)"
	TMP_MC=$(mktemp /tmp/e2e13-mc.XXXXXX)
	if curl -fsSL -o "$TMP_MC" "$MC_URL" && chmod +x "$TMP_MC"; then
		mkdir -p "$REPO_ROOT/vendor"
		mv "$TMP_MC" "$MC_BIN"
	else
		rm -f "$TMP_MC"
		echo '  FATAL: mc download failed'
		assert_eq 'mc binary downloadable' yes no
		exit 0
	fi
fi

BKT='e2e-13-interop'
ALIAS=zetaobject
export INTEROP_ENDPOINT="$ENDPOINT"
export INTEROP_MC_BIN="$MC_BIN"
INTEROP_SHIM_ERR=$(mktemp /tmp/e2e13-shimerr.XXXXXX)
INTEROP_MC_SHIM="$E2E_ROOT/mc-runner.sh"
INTEROP_EXPECT_DATA=''

TMPD=$(mktemp -d)
MC_DEBUG=1 "$MC_BIN" alias set "$ALIAS" "$ENDPOINT" zetaadmin zetaadmin --insecure \
	>"$TMPD/mcout.txt" 2>"$TMPD/mcerr.txt"
ALIAS_RC=$?
if [ "$ALIAS_RC" -ne 0 ]; then
	echo "  mc alias set stderr:"; head -5 "$TMPD/mcerr.txt"
	echo "  mc alias set stdout:"; head -5 "$TMPD/mcout.txt"
fi
assert_eq 'mc alias set' 0 "$ALIAS_RC"


# mc_put <bucket/key> <payload> — mc cannot read /dev/stdin as a cp source;
# stage the payload in a temp file (mc accepts regular files only).
mc_put() {
	local dst=$1 payload=$2 pf
	pf=$(mktemp /tmp/e2e13-put.XXXXXX)
	printf %s "$payload" >"$pf"
	"$MC_BIN" --insecure cp "$pf" "$ALIAS/$dst" >/dev/null 2>&1
	local rc=$?
	rm -f "$pf"
	return $rc
}

PAYLOAD='interop-payload-13'
PAYLOAD_HASH=$(printf %s "$PAYLOAD" | shasum -a 256 | cut -c1-16)

# --- bucket lifecycle (parity: case 01) --------------------------------------
scenario_pass mc bucket_create "$BKT"
scenario_pass mc bucket_head "$BKT"
scenario_pass mc bucket_head_missing 'e2e-13-no-such-bucket-xyz'
scenario_pass mc bucket_list "$BKT"
scenario_pass mc bucket_create_dup "$BKT"

# --- object round trip + unicode (parity: case 02) ---------------------------
scenario_pass mc object_put "$BKT" plain.txt "$PAYLOAD"
INTEROP_EXPECT_DATA="$PAYLOAD_HASH"
scenario_pass mc object_get_roundtrip "$BKT" plain.txt
INTEROP_EXPECT_DATA=''
# unicode_key asserts its own byte-exact round-trip (data=unicode-ok)
scenario_pass mc unicode_key "$BKT" 'uni-✓.txt'

# --- error codes (parity: cases 01–03) ---------------------------------------
scenario_pass mc error_nosuchbucket
scenario_pass mc error_nosuchkey "$BKT"
# NOTE (tolerance): mc stat/cat of an existing object cannot itself trigger
# the server's AccessDenied branch (single credential, always valid SigV4).
# Closest observable AccessDenied both clients share: an EXPIRED presigned
# URL (case 07 parity) — the shim fetches the share URL and reads the 403
# wire XML <Code>AccessDenied</Code> directly.
scenario_pass mc error_accessdenied "$BKT"

# --- listing with prefix (parity: case 04) -----------------------------------
mc_put "$BKT/pre/one.txt" "$PAYLOAD"
mc_put "$BKT/pre/two.txt" "$PAYLOAD"
mc_put "$BKT/other.txt" "$PAYLOAD"
INTEROP_EXPECT_DATA=2
scenario_pass mc list_prefix "$BKT"
INTEROP_EXPECT_DATA=''

# --- range GET (parity: case 06) ------------------------------------------------
# mc cannot send Range headers (no CLI flag for conditional/range gets).
# Closest observable: the object still GETs whole via mc cat (shim hashes it).
RANGE_BIN=$(mktemp /tmp/e2e13-range.XXXXXX)
python3 -c "print(''.join(chr(97 + i % 26) for i in range(119)), end='')" \
	>"$RANGE_BIN"
"$MC_BIN" cp --insecure "$RANGE_BIN" "$ALIAS/$BKT/r.bin" >/dev/null 2>&1
INTEROP_EXPECT_DATA=$(shasum -a 256 "$RANGE_BIN" | cut -c1-16)
scenario_pass mc range_get "$BKT" r.bin
# mc cannot send a Range header at all, so it can never elicit the server's
# 416 InvalidRange — the failure branch is UNEXPRESSIBLE in mc (tolerance
# clause). Closest observable: the object remains readable (success-shaped).
INTEROP_EXPECT_CODE=''
scenario_pass mc range_get_invalid "$BKT" r.bin
unset INTEROP_EXPECT_CODE
rm -f "$RANGE_BIN"
INTEROP_EXPECT_DATA=''

# --- multipart (parity: case 05) ------------------------------------------------
# mc's automatic multipart threshold is 64 MiB and it does not expose the
# three-phase MPU API; a manual 3-part upload is impossible in mc. Closest
# observable: byte-exact streaming round-trip of a >5 MiB object (the actual
# multipart wire protocol is covered by boto3 case 12).
BIG_BIN=$(mktemp /tmp/e2e13-big.XXXXXX)
python3 -c 'import sys; sys.stdout.write("m" * (5*1024*1024))' >"$BIG_BIN"
INTEROP_EXPECT_DATA='mpu-ok'
scenario_pass mc multipart_roundtrip "$BKT" <"$BIG_BIN"
INTEROP_EXPECT_DATA=''
rm -f "$BIG_BIN"

# --- presigned GET (parity: case 07: mc share download) -----------------------
INTEROP_EXPECT_DATA="$PAYLOAD_HASH"
scenario_pass mc presigned_get "$BKT" plain.txt
INTEROP_EXPECT_DATA=''

# --- copy (parity: case 08) ------------------------------------------------------
mc_put "$BKT/copy-src.txt" "$PAYLOAD"
INTEROP_EXPECT_DATA="$PAYLOAD_HASH"
scenario_pass mc copy_object "$BKT" copy-dst.txt "$BKT" copy-src.txt
INTEROP_EXPECT_DATA=''

# --- batch delete (parity: case 08: 2 exist + 1 missing) ------------------------
# mc rm surfaces the missing key as a client-side error and never shows the
# server's Deleted array; the observable is the count mc actually removed.
mc_put "$BKT/d1.txt" "$PAYLOAD"
mc_put "$BKT/d2.txt" "$PAYLOAD"
INTEROP_EXPECT_DATA=2
scenario_pass mc batch_delete "$BKT" d1.txt d2.txt ghost.txt
INTEROP_EXPECT_DATA=''

# --- cleanup ---------------------------------------------------------------------
"$MC_BIN" rb --force --insecure "$ALIAS/$BKT" >/dev/null 2>&1
rm -f "$INTEROP_SHIM_ERR"

# --- findings triage (parity rule: parse failures are findings) ----------------
if [ -n "$INTEROP_FINDINGS" ]; then
	printf '  FINDINGS (client-side parse failures — triage [a] fix wire format / [b] divergence / [c] client bug+skip):\n'
	printf '%s\n' "$INTEROP_FINDINGS" | tr ' ' '\n' | sed '/^$/d; s/^/    /'
fi
