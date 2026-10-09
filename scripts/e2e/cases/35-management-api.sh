#!/usr/bin/env bash
# 35-management-api.sh — wire-level e2e for the management API: a dedicated
# mTLS JSON surface on its own loopback listener (management-api-2026-10
# leaf 06; master Contract 5). It drives the SHARED suite server, whose
# harness (run-e2e.sh) generates the client-certificate fixtures and adds the
# admin frontends entry; the client certificate — not the server's self-signed
# one — is the thing under test, so every request uses -k.
#
#   35a. Auth: no client certificate -> handshake failure or 401 (no cert
#        detail leaked); a certificate from the WRONG CA -> rejected (handshake
#        failure or 401, no detail leaked); the VALID certificate -> 200 on
#        /status.
#   35b. Status + config read: /status reports the frontends list including
#        admin; /config never contains the literal secretKey from the config
#        (secrets are masked).
#   35c. Config write: PUT /config of a hot key (region) lists it under
#        applied; PUT /config of a restart-required key (dataDir) lists it
#        under restartRequired and NOT applied; an invalid key -> 400 with the
#        validator message and no change.
#   35d. Buckets: POST /buckets creates a plain bucket (directory exists, it
#        appears in GET /buckets), DELETE removes it (directory gone).
#   35e. Dataset refusal (requires ZFS; skipped gracefully otherwise): DELETE a
#        dataset-backed bucket -> 409 with DatasetBucketNotDeletable and the
#        dataset still exists.
#   35f. Audit: the audit file carries a record for a management request with
#        the eight contract keys, op=admin, principal=the client cert CN. POST
#        /purge answers the honest JSON error envelope (no zmetad here; never a
#        destructive purge).
#
# BKT= convention, create/cleanup pairing, an EXIT trap, and the lib.sh assert
# helpers; graceful SKIP lines for the openssl-missing and ZFS-only cases.
set -u
BKT='e2e35-mgmt'
E35_DS_BKT='e2e35-ds'
E35_BKT_DIR=''
E35_DS=''

# --- whole-case guard: the harness builds the client-cert fixtures with
# openssl (run-e2e.sh). When it could not, print the standard SKIP line and
# leave cleanly (0 pass / 0 fail).
if [ "${E2E_ADMIN_AVAILABLE:-0}" != 1 ] || [ -z "${E2E_ADMIN_URL:-}" ]; then
	e2e_skip 'management API: client-certificate fixtures unavailable (openssl missing); unit + wiring coverage in internal/frontend/admin carries the contract'
	e2e_finish
	return 0 2>/dev/null || exit 0
fi

# e35_cleanup — best-effort removal of anything this case created: the plain
# bucket (via the API), its directory, and any dataset it may have provisioned
# on a ZFS host. It NEVER touches the shared server process.
e35_cleanup() {
	if [ "${E2E_ADMIN_AVAILABLE:-0}" = 1 ] && [ -n "${E2E_ADMIN_URL:-}" ] \
		&& [ -n "${E2E_ADMIN_CLIENT_CERT:-}" ] && [ -n "${E2E_ADMIN_CLIENT_KEY:-}" ]; then
		for b in "$BKT" "$E35_DS_BKT"; do
			curl -sk --cert "$E2E_ADMIN_CLIENT_CERT" --key "$E2E_ADMIN_CLIENT_KEY" \
				-X DELETE "$E2E_ADMIN_URL/buckets/$b" >/dev/null 2>&1 || true
		done
	fi
	[ -n "${E35_BKT_DIR:-}" ] && rm -rf "$E35_BKT_DIR"
	if [ -n "${E35_DS:-}" ] && command -v zfs >/dev/null 2>&1; then
		zfs destroy "$E35_DS" >/dev/null 2>&1 || true
	fi
}
trap e35_cleanup EXIT

# --- management-request helpers -----------------------------------------------
# mreq <cert> <key> <method> <path> [curl args...] — sets M_STATUS, M_BODY,
# M_RC. -k skips SERVER-certificate verification (not the subject); the CLIENT
# certificate is what the admin listener verifies.
mreq() {
	local cert=$1 key=$2 method=$3 path=$4
	shift 4
	local bf
	bf=$(mktemp /tmp/e2e35-mreq.XXXXXX)
	M_STATUS=$(curl -sk --cert "$cert" --key "$key" -X "$method" \
		-o "$bf" -w '%{http_code}' "$E2E_ADMIN_URL$path" "$@" 2>/dev/null)
	M_RC=$?
	M_BODY=$(cat "$bf")
	rm -f "$bf"
}

# mreq_nocert <method> <path> [curl args...] — no client certificate at all.
mreq_nocert() {
	local method=$1 path=$2
	shift 2
	local bf
	bf=$(mktemp /tmp/e2e35-mreq.XXXXXX)
	M_STATUS=$(curl -sk -X "$method" -o "$bf" -w '%{http_code}' \
		"$E2E_ADMIN_URL$path" "$@" 2>/dev/null)
	M_RC=$?
	M_BODY=$(cat "$bf")
	rm -f "$bf"
}

# e35_no_cert_detail <label> — assert the rejection body leaks no identity or
# reason (no CN, issuer, expiry, or the CA path). A handshake failure has an
# empty body, which trivially passes.
e35_no_cert_detail() {
	local label=$1 bad=0
	case "$M_BODY" in
		*"$E2E_ADMIN_CN"*) bad=1 ;;
	esac
	for tok in issuer expired e2e-admin-ca e2e-other-ca; do
		case "$M_BODY" in
			*"$tok"*) bad=1 ;;
		esac
	done
	assert_eq "$label" 0 "$bad"
}

# --- 35a: authentication ------------------------------------------------------
mreq_nocert GET /status
case "$M_STATUS" in
	000) assert_eq '35a no client certificate: TLS handshake rejected' 0 0 ;;
	401) assert_eq '35a no client certificate: 401' 0 0 ;;
	*) assert_eq '35a no client certificate rejected (handshake failure or 401)' 0 1 ;;
esac
e35_no_cert_detail '35a no-certificate rejection body leaks no cert detail'

mreq "$E2E_ADMIN_WRONG_CERT" "$E2E_ADMIN_WRONG_KEY" GET /status
case "$M_STATUS" in
	000) assert_eq '35a wrong-CA certificate: TLS handshake rejected' 0 0 ;;
	401) assert_eq '35a wrong-CA certificate: 401' 0 0 ;;
	*) assert_eq '35a wrong-CA certificate rejected (handshake failure or 401)' 0 1 ;;
esac
e35_no_cert_detail '35a wrong-CA rejection body leaks no cert detail'

mreq "$E2E_ADMIN_CLIENT_CERT" "$E2E_ADMIN_CLIENT_KEY" GET /status
assert_eq '35a valid client certificate -> 200 /status' 200 "$M_STATUS"
assert_contains '35a /status body is the JSON status report' "$M_BODY" '"version"'
E35_STATUS_JSON="$M_BODY"

# --- 35b: status + config read ------------------------------------------------
E35_FE=$(printf '%s' "$E35_STATUS_JSON" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print("parse-error"); sys.exit(0)
print("yes" if "admin" in (d.get("frontends") or []) else "no")
')
assert_eq '35b /status frontends list includes admin' yes "$E35_FE"

mreq "$E2E_ADMIN_CLIENT_CERT" "$E2E_ADMIN_CLIENT_KEY" GET /config
assert_eq '35b GET /config -> 200' 200 "$M_STATUS"
assert_contains '35b /config body is the JSON config document' "$M_BODY" '"dataDir"'
if printf '%s' "$M_BODY" | grep -qF "$E2E_ADMIN_CONFIG_SECRET"; then
	assert_eq '35b /config never contains the literal secretKey' absent LEAKED
else
	assert_eq '35b /config never contains the literal secretKey' absent absent
fi
assert_contains '35b /config masks the configured secret' "$M_BODY" '"secretKey":"********"'

# --- 35c: config write --------------------------------------------------------
# Hot key: region -> applied.
mreq "$E2E_ADMIN_CLIENT_CERT" "$E2E_ADMIN_CLIENT_KEY" PUT /config \
	-H 'Content-Type: application/json' --data '{"region":"e2e35-region"}'
assert_eq '35c PUT /config region -> 200' 200 "$M_STATUS"
assert_contains '35c hot key region listed under applied' "$M_BODY" '"applied":["region"]'

# Restart-required key: dataDir -> restartRequired, NOT applied. The value is
# unchanged (the live config's own dataDir), so the running server is untouched.
mreq "$E2E_ADMIN_CLIENT_CERT" "$E2E_ADMIN_CLIENT_KEY" PUT /config \
	-H 'Content-Type: application/json' --data "{\"dataDir\":\"$E2E_DATA_DIR\"}"
assert_eq '35c PUT /config dataDir -> 200' 200 "$M_STATUS"
E35_RESTART_HAS=$(printf '%s' "$M_BODY" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print("yes" if "dataDir" in (d.get("restartRequired") or []) else "no")
')
assert_eq '35c restart-required key dataDir listed under restartRequired' yes "$E35_RESTART_HAS"
E35_APPLIED_HAS=$(printf '%s' "$M_BODY" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print("yes" if "dataDir" in (d.get("applied") or []) else "no")
')
assert_eq '35c restart-required key dataDir NOT under applied' no "$E35_APPLIED_HAS"

# Invalid key -> 400 with the validator message, nothing changes.
mreq "$E2E_ADMIN_CLIENT_CERT" "$E2E_ADMIN_CLIENT_KEY" PUT /config \
	-H 'Content-Type: application/json' --data '{"notAKey":true}'
assert_eq '35c PUT /config invalid key -> 400' 400 "$M_STATUS"
assert_contains '35c validator message says unknown field' "$M_BODY" 'unknown field'
assert_contains '35c validator message names the offending key' "$M_BODY" 'notAKey'
mreq "$E2E_ADMIN_CLIENT_CERT" "$E2E_ADMIN_CLIENT_KEY" GET /config
assert_contains '35c invalid patch changed nothing (region still set)' "$M_BODY" '"region":"e2e35-region"'

# --- 35d: bucket operations ---------------------------------------------------
E35_BKT_DIR="$E2E_DATA_DIR/$BKT"
mreq "$E2E_ADMIN_CLIENT_CERT" "$E2E_ADMIN_CLIENT_KEY" POST /buckets \
	-H 'Content-Type: application/json' --data "{\"name\":\"$BKT\"}"
assert_eq '35d POST /buckets -> 200' 200 "$M_STATUS"
assert_contains '35d create body confirms the bucket' "$M_BODY" '"created":true'
if [ -d "$E35_BKT_DIR" ]; then
	assert_eq '35d created bucket directory exists on disk' exists exists
else
	assert_eq '35d created bucket directory exists on disk' exists missing
fi
mreq "$E2E_ADMIN_CLIENT_CERT" "$E2E_ADMIN_CLIENT_KEY" GET /buckets
assert_eq '35d GET /buckets -> 200' 200 "$M_STATUS"
assert_contains '35d new bucket appears in GET /buckets' "$M_BODY" "$BKT"
mreq "$E2E_ADMIN_CLIENT_CERT" "$E2E_ADMIN_CLIENT_KEY" DELETE "/buckets/$BKT"
assert_eq '35d DELETE /buckets/{name} -> 200' 200 "$M_STATUS"
assert_contains '35d delete body confirms the deletion' "$M_BODY" '"deleted":true'
if [ -d "$E35_BKT_DIR" ]; then
	assert_eq '35d bucket directory gone after delete' gone present
else
	assert_eq '35d bucket directory gone after delete' gone gone
fi

# --- 35e: dataset-delete refusal (ZFS only) -----------------------------------
if ! command -v zfs >/dev/null 2>&1; then
	echo '  (management API dataset-delete refusal: requires ZFS; no zfs binary on this host — skipping group; live coverage in scripts/zfs-validate/run-zfs-validation.sh)'
elif ! zfs list -H -o name "$E2E_DATA_DIR" >/dev/null 2>&1; then
	echo '  (management API dataset-delete refusal: dataDir is not a ZFS mountpoint — skipping group; live coverage in scripts/zfs-validate/run-zfs-validation.sh)'
else
	E35_PARENT=$(zfs list -H -o name -t filesystem "$E2E_DATA_DIR" 2>/dev/null | head -1 | tr -d '[:space:]')
	mreq "$E2E_ADMIN_CLIENT_CERT" "$E2E_ADMIN_CLIENT_KEY" POST /buckets \
		-H 'Content-Type: application/json' --data "{\"name\":\"$E35_DS_BKT\"}"
	if [ -n "$E35_PARENT" ]; then
		E35_DS=$(zfs list -H -o name "$E35_PARENT/$E35_DS_BKT" 2>/dev/null | head -1 | tr -d '[:space:]')
	fi
	if [ -z "$E35_DS" ]; then
		echo '  (management API dataset-delete refusal: the shared server did not provision a dataset (zfs_bucket_datasets off) — skipping group; live coverage in scripts/zfs-validate/run-zfs-validation.sh)'
	else
		mreq "$E2E_ADMIN_CLIENT_CERT" "$E2E_ADMIN_CLIENT_KEY" DELETE "/buckets/$E35_DS_BKT"
		assert_eq '35e DELETE dataset-backed bucket -> 409' 409 "$M_STATUS"
		assert_contains '35e body carries DatasetBucketNotDeletable' "$M_BODY" 'DatasetBucketNotDeletable'
		if zfs list -H -o name "$E35_DS" >/dev/null 2>&1; then
			assert_eq '35e dataset still exists after the refusal' present present
		else
			assert_eq '35e dataset still exists after the refusal' present gone
		fi
		# Cleanup (the case owns this dataset; the API never destroys it).
		zfs destroy "$E35_DS" >/dev/null 2>&1 || true
		E35_DS=''
		E35_BKT_DIR="$E2E_DATA_DIR/$E35_DS_BKT"
		mreq "$E2E_ADMIN_CLIENT_CERT" "$E2E_ADMIN_CLIENT_KEY" DELETE "/buckets/$E35_DS_BKT" >/dev/null 2>&1
		rm -rf "$E35_BKT_DIR"
	fi
fi

# --- 35f: audit record + honest purge refusal ---------------------------------
E35_AUDIT="${E2E_AUDIT_LOG:-}"
if [ -n "$E35_AUDIT" ] && [ -f "$E35_AUDIT" ]; then
	E35_AUDIT_CHECK=$(python3 - "$E35_AUDIT" "$E2E_ADMIN_CN" <<'PY'
import json, sys
path, cn = sys.argv[1], sys.argv[2]
want = {"ts", "principal", "method", "bucket", "key", "op", "status", "denied"}
result = "missing"
with open(path) as fh:
    for line in fh:
        line = line.strip()
        if not line:
            continue
        try:
            rec = json.loads(line)
        except Exception:
            continue
        if rec.get("op") == "admin" and rec.get("principal") == cn:
            if set(rec.keys()) != want:
                result = "keyset:" + ",".join(sorted(rec.keys()))
                break
            result = "ok"
            break
print(result)
PY
)
	assert_eq '35f audit record: op=admin, principal=CN, exactly the 8 contract keys' ok "$E35_AUDIT_CHECK"
else
	echo '  (management API audit: no audit log configured in this environment — skipping audit-group assert)'
fi

# POST /purge must answer honestly (the e2e host has no zmetad): a JSON error
# envelope, never an invented success. Never a destructive purge here.
mreq "$E2E_ADMIN_CLIENT_CERT" "$E2E_ADMIN_CLIENT_KEY" POST /purge \
	-H 'Content-Type: application/json' --data '{"dataset":"e2e35-no-such-dataset"}'
case "$M_STATUS" in
	500|503) assert_eq '35f POST /purge unavailable -> 5xx (honest)' 5xx 5xx ;;
	*) assert_eq '35f POST /purge unavailable -> 5xx (honest)' 5xx "$M_STATUS" ;;
esac
E35_PURGE_SHAPE=$(printf '%s' "$M_BODY" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print("bad"); sys.exit(0)
e = d.get("error")
print("ok" if isinstance(e, dict) and e.get("code") and e.get("message") else "bad")
')
assert_eq '35f /purge answers the honest JSON error envelope' ok "$E35_PURGE_SHAPE"
if printf '%s' "$M_BODY" | grep -q '"purged":true'; then
	assert_eq '35f no destructive purge occurred' none purged
else
	assert_eq '35f no destructive purge occurred' none none
fi

# Restore the region 35c hot-applied: this case runs on the SHARED suite
# server, and every later SigV4 case signs us-east-1 (lib.sh's s3req scope).
# A leaked region turned every later shared-server request into
# SignatureDoesNotMatch (surfaced when case 39 landed).
mreq "$E2E_ADMIN_CLIENT_CERT" "$E2E_ADMIN_CLIENT_KEY" PUT /config \
	-H 'Content-Type: application/json' --data '{"region":"us-east-1"}'
assert_eq '35z restore the shared server region -> 200' 200 "$M_STATUS"

printf '\n'
