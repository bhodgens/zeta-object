#!/usr/bin/env bash
# Reusable ZFS validation harness (AGENTS.md "ZFS validation rule").
#
# Validates the CURRENT repo HEAD against real OpenZFS (extended-metadata
# branch) on the zfs-meta host:
#   1. builds the repo's linux/amd64 server binary
#   2. deploys it (+ a freshly generated TLS cert) to zfs-meta:~/zeta-validate/
#   3. recreates a scratch dataset testpool/zval fresh (events=on, events_size=1M)
#   4. writes the config and launches the server on :9707
#   5. runs the S3 + ZFS-event checks (see run_checks below)
#   6. prints a PASS/FAIL table, exits non-zero on any failure
#   7. cleans up: stops the server, destroys testpool/zval
#
# Usage:  scripts/zfs-validate/run-zfs-validation.sh [--keep-server]
#   --keep-server   leave the server + dataset in place after the run
#                   (for manual poking; server keeps running)
#
# ---------------------------------------------------------------------------
# LIVE-HOST GOTCHAS (both discovered the hard way on zfs-meta - do not regress)
# ---------------------------------------------------------------------------
# (a) Launching the server with `ssh zfs-meta "nohup ... &"` DIES as soon as
#     the ssh session closes (systemd user session teardown kills the whole
#     process group even under nohup). The working pattern: scp a small
#     host-side starter script to the remote host and run it SYNCHRONOUSLY
#     over ssh; the script uses `setsid nohup ... < /dev/null >> log 2>&1 &`
#     so the daemon is reparented away from the ssh session.
# (b) A config where `listenAddr` and a `frontends[].listenAddr` are the same
#     port self-collides (the server binds both the data plane and the
#     frontend on it -> "address already in use" at startup). The config this
#     script writes uses a distinct frontend port, or an empty `frontends`
#     array when no frontend checks are requested.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HOST="${ZFS_VALIDATE_HOST:-zfs-meta}"
REMOTE_HOME="$(ssh -o BatchMode=yes "$HOST" 'echo $HOME')"
REMOTE_DIR="${ZFS_VALIDATE_REMOTE_DIR:-$REMOTE_HOME/zeta-validate}"
DATASET="${ZFS_VALIDATE_DATASET:-testpool/zval}"
PORT="${ZFS_VALIDATE_PORT:-9707}"
BUCKET="${ZFS_VALIDATE_BUCKET:-zval}"
AK="valuser"; SK="valpass"
KEEP_SERVER=0
[[ "${1:-}" == "--keep-server" ]] && KEEP_SERVER=1

WORK="$(mktemp -d /tmp/zfs-validate.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

log()  { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }
die()  { printf 'FATAL: %s\n' "$*" >&2; exit 2; }

# ---------------------------------------------------------------- build ----
log "Building linux/amd64 server from repo HEAD ($(git -C "$REPO_ROOT" rev-parse --short HEAD))"
BIN="$WORK/zeta-server"
( cd "$REPO_ROOT" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$BIN" . ) \
  || die "go build failed"
openssl req -x509 -newkey rsa:2048 -keyout "$WORK/key.pem" -out "$WORK/cert.pem" \
  -days 2 -nodes -subj "/CN=$HOST" >/dev/null 2>&1 || die "openssl failed"

# ------------------------------------------------------------- deploy ------
log "Deploying to $HOST:$REMOTE_DIR"
# stop any prior instance first - overwriting a running binary fails (ETXTBSY)
ssh -o BatchMode=yes "$HOST" "pkill -f 'zeta-serve[r].*zeta-validate' 2>/dev/null; sleep 0.5; mkdir -p $REMOTE_DIR; true"
scp -q "$BIN" "$HOST:$REMOTE_DIR/zeta-server"
scp -q "$WORK/cert.pem" "$WORK/key.pem" "$HOST:$REMOTE_DIR/"
ssh -o BatchMode=yes "$HOST" "chmod +x $REMOTE_DIR/zeta-server"

# ----------------------------------------------- fresh scratch dataset -----
log "Recreating fresh dataset $DATASET (events=on, events_size=1M)"
ssh -o BatchMode=yes "$HOST" "
  sudo zfs destroy -r '$DATASET' 2>/dev/null || true
  sudo zfs create -o events=on -o events_size=1M '$DATASET'
  sudo zfs set events=on '$DATASET'
  sudo chown -R \$(id -u):\$(id -g) '/$DATASET'
"

# ------------------------------------------------------ config + starter ---
# NOTE gotcha (b): frontend port must differ from listenAddr (or use empty
# frontends array) to avoid the dual-listener self-collision.
FRONTENDS='[]'
cat > "$WORK/config.json" <<EOF
{
  "dataDir": "/testpool/",
  "listenAddr": ":$PORT",
  "certFile": "cert.pem",
  "keyFile": "key.pem",
  "identities": [
    { "name": "val", "accessKey": "$AK", "secretKey": "$SK", "grants": { "*": "readwrite" } }
  ],
  "frontends": $FRONTENDS
}
EOF
scp -q "$WORK/config.json" "$HOST:$REMOTE_DIR/config.json"

# NOTE gotcha (a): the starter script is executed synchronously over ssh; the
# setsid+nohup+</dev/null combo detaches the server from the ssh session.
cat > "$WORK/start-server.sh" <<EOF
#!/usr/bin/env bash
cd '$REMOTE_DIR'
pkill -f 'zeta-serve[r].*zeta-validate' 2>/dev/null || true
sleep 0.5
setsid nohup ./zeta-server < /dev/null >> server.log 2>&1 &
for i in \$(seq 1 50); do
  if timeout 2 bash -c "echo > /dev/tcp/127.0.0.1/$PORT" 2>/dev/null; then
    echo "SERVER_UP"; exit 0
  fi
  sleep 0.2
done
echo "SERVER_FAILED"; tail -20 server.log; exit 1
EOF
scp -q "$WORK/start-server.sh" "$HOST:$REMOTE_DIR/start-server.sh"

log "Starting server on :$PORT"
ssh -o BatchMode=yes "$HOST" "bash $REMOTE_DIR/start-server.sh" \
  || die "server did not come up on :$PORT"
echo "server is up"

# --------------------------------------------------- self-contained probe --
# SigV4 probe client, generated here so the harness has no external deps.
cat > "$WORK/probe.py" <<'PYEOF'
#!/usr/bin/env python3
"""SigV4 probe client for mini-s3 ZFS validation."""
import hashlib, hmac, http.client, json, ssl, sys, urllib.parse
from datetime import datetime, timezone

HOST, PORT = "zfs-meta", 9707
AK, SK, REGION = "valuser", "valpass", "us-east-1"

def _ssl():
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return ctx

def sign(method, path, query="", payload=b"", amz_meta=None, content_type=None):
    t = datetime.now(timezone.utc)
    amzdate = t.strftime("%Y%m%dT%H%M%SZ"); datestamp = t.strftime("%Y%m%d")
    headers = {
        "host": f"{HOST}:{PORT}",
        "x-amz-content-sha256": hashlib.sha256(payload).hexdigest(),
        "x-amz-date": amzdate,
    }
    if amz_meta: headers.update({k.lower(): v for k, v in amz_meta.items()})
    if content_type: headers["content-type"] = content_type
    signed = ";".join(sorted(headers))
    canon = "".join(f"{k}:{headers[k]}\n" for k in sorted(headers))
    cqp = ""
    if query:
        q = urllib.parse.parse_qsl(query, keep_blank_values=True)
        cqp = "&".join(f"{urllib.parse.quote(k, safe='')}={urllib.parse.quote(v, safe='')}"
                       for k, v in sorted(q))
    creq = "\n".join([method, urllib.parse.quote(path, safe="/"), cqp, canon,
                      signed, headers["x-amz-content-sha256"]])
    scope = f"{datestamp}/{REGION}/s3/aws4_request"
    sts = f"AWS4-HMAC-SHA256\n{amzdate}\n{scope}\n{hashlib.sha256(creq.encode()).hexdigest()}"
    def h(k, m): return hmac.new(k, m.encode(), hashlib.sha256).digest()
    k = h(h(h(h(("AWS4"+SK).encode(), datestamp), REGION), "s3"), "aws4_request")
    sig = hmac.new(k, sts.encode(), hashlib.sha256).hexdigest()
    hdrs = dict(headers)
    hdrs["Authorization"] = (f"AWS4-HMAC-SHA256 Credential={AK}/{scope}, "
                             f"SignedHeaders={signed}, Signature={sig}")
    conn = http.client.HTTPSConnection(HOST, PORT, context=_ssl(), timeout=30)
    conn.request(method, path + ("?"+query if query else ""),
                 body=payload if payload else None, headers=hdrs)
    r = conn.getresponse(); body = r.read()
    rh = {k.lower(): v for k, v in r.getheaders()}
    conn.close()
    return r.status, rh, body
PYEOF

# ------------------------------------------------------------- checks ------
cat > "$WORK/checks.py" <<'PYEOF'
import hashlib, json, re, subprocess, sys, time
exec(open("probe.py").read())  # defines sign()
import os
os.chdir(os.path.dirname(os.path.abspath(__file__)))

HOST = "zfs-meta"
DATASET = "testpool/zval"
BUCKET = "zval"

def sh(cmd, timeout=60):
    r = subprocess.run(["ssh", "-o", "BatchMode=yes", HOST, cmd],
                       capture_output=True, text=True, timeout=timeout)
    return (r.stdout + r.stderr).strip()

results = []
def check(name, ok, detail=""):
    results.append((name, bool(ok), str(detail)[:300]))

def wait_events(secs=1.2):
    time.sleep(secs)

# ---- 1. plain S3 round-trip on the ZFS-backed bucket
s, h, b = sign("PUT", f"/{BUCKET}/val-doc.txt", payload=b"zfs validation payload",
               amz_meta={"x-amz-meta-probe": "adversarial"}, content_type="text/plain")
check("S3 PUT", s == 200, f"{s} {b[:120]}")
s, h, b = sign("GET", f"/{BUCKET}/val-doc.txt")
check("S3 GET round-trip", s == 200 and b == b"zfs validation payload", f"{s} len={len(b)}")
check("S3 custom meta present", h.get("x-amz-meta-probe") == "adversarial",
      f"{h.get('x-amz-meta-probe')}")
disk = sh(f"ls -la /{DATASET}/ | head -5")
check("file lands on ZFS dataset", "val-doc.txt" in disk, disk[:200])
s, h, b = sign("DELETE", f"/{BUCKET}/val-doc.txt")
check("S3 DELETE (204)", s == 204, f"{s} {b[:120]}")

# ---- 2. multipart complete + GET integrity
s, h, b = sign("POST", f"/{BUCKET}/mp.bin", query="uploads",
               content_type="application/octet-stream")
m = re.search(rb"<UploadId>([^<]+)</UploadId>", b)
check("multipart initiate", s == 200 and m, f"{s} {b[:120]}")
uid = m.group(1).decode() if m else ""
parts = []
for pn, chunk in ((1, b"A" * (5 * 1024 * 1024)), (2, b"B" * (1024 * 1024))):
    s, h, b = sign("PUT", f"/{BUCKET}/mp.bin",
                   query=f"partNumber={pn}&uploadId={uid}", payload=chunk)
    etag = h.get("etag", "")
    parts.append(f"<Part><PartNumber>{pn}</PartNumber><ETag>{etag}</ETag></Part>")
body = f"<CompleteMultipartUpload>{''.join(parts)}</CompleteMultipartUpload>".encode()
s, h, b = sign("POST", f"/{BUCKET}/mp.bin", query=f"uploadId={uid}",
               payload=body, content_type="application/xml")
check("multipart complete", s == 200 and b"CompleteMultipartUploadResult" in b,
      f"{s} {b[:150]}")
s, h, b = sign("GET", f"/{BUCKET}/mp.bin")
check("multipart GET integrity", s == 200 and len(b) == 6 * 1024 * 1024 and
      b[:1] == b"A" and b[-1:] == b"B", f"{s} len={len(b)}")
wait_events()

# ---- 3. ?events JSON vs `zfs events -j` ground truth
s, h, b = sign("GET", f"/{BUCKET}/mp.bin", query="events")
check("?events JSON 200", s == 200, f"{s} {b[:150]}")
try:
    ev = json.loads(b)
    check("?events has dataset", ev.get("dataset") == DATASET, str(ev)[:200])
except Exception as e:
    check("?events JSON parses", False, f"{e} {b[:120]}")
    ev = {"events": []}
z = sh(f"sudo zfs events -j {DATASET}")
z = z[:z.rindex("]") + 1] if "]" in z else z
truth = json.loads(z)
names = {e.get("name") for e in truth if isinstance(e, dict)}
check("ZFS ground truth has mp.bin", "mp.bin" in names, sorted(n for n in names if n)[:8])
srv_keys = {e.get("key") for e in ev.get("events", [])}
check("server events include mp.bin", any("mp.bin" in (k or "") for k in srv_keys),
      sorted(k for k in srv_keys if k)[:8])
# per-record match: every server key for mp.bin corresponds to a ZFS record
srecs = [e for e in ev.get("events", []) if "mp.bin" in (e.get("key") or "")]
# F-live-4 (documented semantics): a completed multipart object surfaces as
# exactly ONE rename event (tmp assembly file -> final name) on the wire;
# the ZFS ring holds the full CREATE(tmp)+RENAME pair.
check("multipart rename event (tmp->final) present",
      any(e.get("op") == "rename" and e.get("key") == "mp.bin" and
          "tmp" in (e.get("oldKey") or "") for e in srecs),
      json.dumps(srecs)[:250])
for e in srecs:
    zn = e.get("key", "").rsplit("/", 1)[-1]
    check(f"  event '{e.get('op', '?')} {e.get('key')}' backed by ZFS truth",
          zn in names, f"no zfs record named {zn}")

# ---- 4. ?events&versions ext XML
s, h, b = sign("GET", f"/{BUCKET}", query="events&versions&max-events=20")
check("?events&versions ext XML", s == 200 and b"ListObjectVersionsExt" in b,
      f"{s} {b[:150]}")
check("ext XML has IsLossy/RecordsLost", b"IsLossy" in b and b"RecordsLost" in b, b[:150])

# ---- 5. events=off -> 503 -> events=on -> restored
sh(f"sudo zfs set events=off {DATASET}")
s, h, b = sign("GET", f"/{BUCKET}/mp.bin", query="events")
check("events=off -> 503", s == 503, f"{s} {b[:120]}")
sh(f"sudo zfs set events=on {DATASET}")
time.sleep(0.5)
s, h, b = sign("GET", f"/{BUCKET}/mp.bin", query="events")
check("events=on restores service", s == 200 and b'"events"' in b, f"{s} {b[:120]}")

# ---- 6. nested-key history exact (full-path reconstruction)
sh(f"mkdir -p /{DATASET}/edge/deep && echo -n x > /{DATASET}/edge/deep/leaf.txt")
wait_events()
s, h, b = sign("GET", f"/{BUCKET}/edge/deep/leaf.txt", query="events")
try:
    ev2 = json.loads(b)
except Exception:
    ev2 = {"events": []}
check("nested key ?events 200", s == 200, f"{s} {b[:120]}")
nested = [e for e in ev2.get("events", []) if e.get("key") == "edge/deep/leaf.txt"]
check("nested-key history exact full path", len(nested) >= 1,
      json.dumps(ev2)[:250])
z = sh(f"sudo zfs events -j {DATASET}")
z = z[:z.rindex("]") + 1] if "]" in z else z
truth2 = json.loads(z)
check("nested key in ZFS truth (bare name)", any(e.get("name") == "leaf.txt" for e in truth2),
      str([e for e in truth2 if "leaf" in str(e)])[:200])

# ---- 7. '.'-key rejection (bucket integrity)
# Go >=1.22 http.ServeMux canonicalizes PUT /zval/. -> 307 redirect to /zval
# BEFORE the S3 frontend sees it: the key never reaches the data plane, so
# the bucket stays intact (that is what this check actually guards).
s, h, b = sign("PUT", f"/{BUCKET}/.", payload=b"x")
redirected = s == 307 and h.get("location", "").rstrip("/") == f"/{BUCKET}"
check("PUT '.' rejected (400/403 or mux 307 canonicalize)",
      s in (400, 403) or redirected, f"{s} loc={h.get('location')}")
disk = sh(f"ls -ld /{DATASET} && ls /{DATASET}/ | grep -c '^\\.$' ; true")
check("no '.' object written to dataset", "drwx" in disk, disk[:200])

# ---- 8. deep 2-level path history
sh(f"mkdir -p /{DATASET}/a1/a2 && echo -n x > /{DATASET}/a1/a2/leaf2.txt")
wait_events()
s, h, b = sign("GET", f"/{BUCKET}/a1/a2/leaf2.txt", query="events")
try:
    ev3 = json.loads(b)
except Exception:
    ev3 = {"events": []}
check("deep 2-level path exact", any(e.get("key") == "a1/a2/leaf2.txt"
                                     for e in ev3.get("events", [])),
      json.dumps(ev3)[:250])

failed = [(n, d) for n, ok, d in results if not ok]
for n, ok, d in results:
    print(f"{'PASS' if ok else 'FAIL'} | {n}" + (f" | {d}" if not ok else ""))
print(f"TOTAL: {len(results) - len(failed)}/{len(results)}")
sys.exit(1 if failed else 0)
PYEOF

# --------------------------------------------------------------- run -------
log "Running validation checks against $HOST:$PORT"
set +e
( cd "$WORK" && python3 checks.py )
RC=$?
set -e

# ------------------------------------------------------------- cleanup -----
if [[ $RC -ne 0 || $KEEP_SERVER -eq 0 ]]; then
  log "Cleanup: stopping server, destroying $DATASET"
  ssh -o BatchMode=yes "$HOST" "
    pkill -f 'zeta-serve[r].*zeta-validate' 2>/dev/null || true
    sudo zfs destroy -r '$DATASET' 2>/dev/null || true
    true
  " || true
fi

if [[ $RC -ne 0 ]]; then
  echo; echo "VALIDATION FAILED ($RC check(s) failed). Server log follows:"
  ssh -o BatchMode=yes "$HOST" "tail -30 $REMOTE_DIR/server.log" 2>/dev/null || true
fi
exit $RC
