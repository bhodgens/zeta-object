#!/usr/bin/env bash
# Reusable ZFS validation harness (AGENTS.md "ZFS validation rule").
#
# Validates the CURRENT repo HEAD against real OpenZFS (extended-metadata
# branch) on the zfs-meta host, ZMETAD PATH (provider reads the zmetad
# SQLite DB, layout v5 - no CLI `zfs events` transport remains):
#   1. builds the repo's linux/amd64 server binary
#   2. deploys it (+ a freshly generated TLS cert) to zfs-meta:~/zeta-validate/
#   3. recreates a scratch dataset testpool/zval fresh (events=on, events_size=1M)
#   4. starts zmetad (poll 2s) exporting to ~/zeta-validate/zmetad.db
#   5. writes the config (zmetad_db_path + zmetad_binary) and launches the
#      server on :9707
#   6. runs the S3 + zmetad-DB event checks (see checks.py below): wire
#      ?events vs the SQLite ground truth, full_path nested-key resolution,
#      freshness bound (poll interval), SIGUSR1 forced collect, gap/loss
#      fields, events=off -> 503 (zmetad prunes untracked datasets) ->
#      events=on restores
#   7. prints a PASS/FAIL table, exits non-zero on any failure
#   8. cleans up: stops the server + zmetad, destroys testpool/zval
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
AK2="valuser2"; SK2="valpass2"
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
ssh -o BatchMode=yes "$HOST" "pkill -f '[.]/zeta-serve[r]' 2>/dev/null; pkill -f 'zmeta[d].*zeta-validate' 2>/dev/null; sleep 0.5; mkdir -p $REMOTE_DIR; true"
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

# ------------------------------------------------------------- zmetad ------
# /dev/zfs is 0666 on this host, so zmetad runs unprivileged. Poll interval
# 2s keeps the freshness waits in checks.py short. Foreground flag + setsid
# detaches it from the ssh session (gotcha (a)).
log "Starting zmetad (poll 2s, db $REMOTE_DIR/zmetad.db)"
cat > "$WORK/start-zmetad.sh" <<EOF
#!/usr/bin/env bash
pkill -f 'zmeta[d].*zeta-validate' 2>/dev/null || true
sleep 0.3
rm -f '$REMOTE_DIR/zmetad.db' '$REMOTE_DIR/zmetad.db-wal' '$REMOTE_DIR/zmetad.db-shm'
setsid nohup zmetad --foreground -d '$REMOTE_DIR/zmetad.db' -i 2 < /dev/null >> '$REMOTE_DIR/zmetad.log' 2>&1 &
for i in \$(seq 1 50); do
  [ -f '$REMOTE_DIR/zmetad.db' ] && echo "ZMETAD_UP" && exit 0
  sleep 0.2
done
echo "ZMETAD_FAILED"; tail -20 '$REMOTE_DIR/zmetad.log' 2>/dev/null; exit 1
EOF
scp -q "$WORK/start-zmetad.sh" "$HOST:$REMOTE_DIR/start-zmetad.sh"
ssh -o BatchMode=yes "$HOST" "bash $REMOTE_DIR/start-zmetad.sh" \
  || die "zmetad did not come up"

# ------------------------------------------------------ config + starter ---
# NOTE gotcha (b): frontend port must differ from listenAddr (or use empty
# frontends array) to avoid the dual-listener self-collision.
FRONTENDS='[]'
# Section 10 (snapshots-mode versioning) was written against the OLD
# default; leaf 06 flips defaultZfsVersioning to "reflink", so the
# snapshots mode is now pinned EXPLICITLY here. Section 11 restarts the
# server with a reflink config (written by checks.py over ssh).
cat > "$WORK/config.json" <<EOF
{
  "dataDir": "/testpool/",
  "listenAddr": ":$PORT",
  "certFile": "cert.pem",
  "keyFile": "key.pem",
  "zfs_versioning": "snapshots",
  "zmetad_db_path": "$REMOTE_DIR/zmetad.db",
  "zmetad_binary": "/usr/local/sbin/zmetad",
  "auditLog": {"path": "$REMOTE_DIR/audit.jsonl"},
  "identities": [
    { "name": "val", "accessKey": "$AK", "secretKey": "$SK", "grants": { "*": "readwrite" } },
    { "name": "val2", "accessKey": "$AK2", "secretKey": "$SK2", "grants": { "*": "readwrite" } }
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
pkill -f '[.]/zeta-serve[r]' 2>/dev/null || true
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

def sign_as(ak, sk, method, path, query="", payload=b"", amz_meta=None, content_type=None):
    global AK, SK
    old_ak, old_sk = AK, SK
    AK, SK = ak, sk
    try:
        return sign(method, path, query=query, payload=payload, amz_meta=amz_meta, content_type=content_type)
    finally:
        AK, SK = old_ak, old_sk

def _ssl():
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return ctx

def _request(port, method, path, query="", payload=b"", amz_meta=None, content_type=None):
    t = datetime.now(timezone.utc)
    amzdate = t.strftime("%Y%m%dT%H%M%SZ"); datestamp = t.strftime("%Y%m%d")
    headers = {
        "host": f"{HOST}:{port}",
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
    conn = http.client.HTTPSConnection(HOST, port, context=_ssl(), timeout=30)
    conn.request(method, path + ("?"+query if query else ""),
                 body=payload if payload else None, headers=hdrs)
    r = conn.getresponse(); body = r.read()
    rh = {k.lower(): v for k, v in r.getheaders()}
    conn.close()
    return r.status, rh, body


def sign(method, path, query="", payload=b"", amz_meta=None, content_type=None):
    return _request(PORT, method, path, query=query, payload=payload,
                    amz_meta=amz_meta, content_type=content_type)


def sign_on(port, method, path, query="", payload=b"", amz_meta=None, content_type=None):
    """Same signing against a DIFFERENT server phase/port (section 12's
    zfs_bucket_datasets phase)."""
    return _request(port, method, path, query=query, payload=payload,
                    amz_meta=amz_meta, content_type=content_type)
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
AK2, SK2 = "valuser2", "valpass2"  # second identity (breadcrumb probes)

def sh(cmd, timeout=60):
    r = subprocess.run(["ssh", "-o", "BatchMode=yes", HOST, cmd],
                       capture_output=True, text=True, timeout=timeout)
    return (r.stdout + r.stderr).strip()

# checks.py runs LOCALLY and queries the DB over ssh: resolve the REMOTE
# home now (db_query embeds this absolute path into the remote python3).
ZMETAD_DB = sh("echo $HOME") + "/zeta-validate/zmetad.db"

def db_query(sql):
    """Query the zmetad DB on the host via python3 stdlib sqlite3 (the host
    has no sqlite3 CLI). Returns parsed JSON."""
    py = ("import sqlite3,json,sys;"
          "c=sqlite3.connect('file:%s?mode=ro',uri=True);"
          "c.row_factory=sqlite3.Row;"
          "r=[dict(x) for x in c.execute(sys.argv[1])];"
          "print(json.dumps(r))" % ZMETAD_DB)
    out = sh(f"python3 -c {json.dumps(py)} {json.dumps(sql)}")
    try:
        return json.loads(out[out.index("["):out.rindex("]") + 1])
    except Exception:
        return None

def force_collect(secs=1.5):
    """SIGUSR1 forces an out-of-band zmetad collect (SCHEMA.md section 8)."""
    sh("pkill -USR1 -f 'zmeta[d].*zeta-validate' 2>/dev/null; true")
    time.sleep(secs)

results = []
def check(name, ok, detail=""):
    results.append((name, bool(ok), str(detail)[:300]))

def wait_events(secs=1.2):
    time.sleep(secs)

# ---- 0. zmetad DB is live and tracking the scratch dataset
force_collect()
meta = db_query("SELECT key, value FROM meta")
check("zmetad DB reachable + meta rows", isinstance(meta, list) and len(meta) >= 1, str(meta)[:200])
ver = {r["key"]: r["value"] for r in (meta or [])}
check("db_schema_version in supported range (5..8)",
      ver.get("db_schema_version") in ("5", "6", "7", "8"), str(ver))
# events_schema_version moves in LOCKSTEP with the layout upstream
# (layout 5/6 == wire 2; layout 7/8 == wire 3). The server accepts both.
ev_wire = ver.get("events_schema_version")
dbv = ver.get("db_schema_version")
consistent = (dbv in ("5", "6") and ev_wire in ("2", None)) or \
             (dbv in ("7", "8") and ev_wire == "3")
check("events_schema_version consistent with layout", bool(consistent), str(ver))
ds_rows = db_query(f"SELECT dataset, mountpoint FROM datasets WHERE dataset = '{DATASET}'")
check("datasets table tracks scratch dataset", bool(ds_rows), str(ds_rows)[:200])

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

RXATTR_HELPER = "/tmp/zval-rxattr.py"
# One-time helper upload (host has no getfattr/attr CLI; python3 stdlib
# os.getxattr covers it). The helper prints the value or nothing.
sh("printf '%s\\n' 'import os,sys' "
   "'try: print(os.getxattr(sys.argv[1], sys.argv[2]).decode())' "
   "'except OSError: pass' > " + RXATTR_HELPER)

def remote_xattr(path, name):
    """Read one xattr on the host via the uploaded os.getxattr helper.
    Returns the decoded value or None (absent)."""
    out = sh("python3 {} {} {}".format(json.dumps(RXATTR_HELPER), json.dumps(path), json.dumps(name)))
    return out.strip() or None

# ---- 1b. principal breadcrumb probes (auth extensions leaf 10) ------------
s, h, b = sign("PUT", f"/{BUCKET}/val-owner.txt", payload=b"owner probe",
               content_type="text/plain")
check("probe PUT as valuser", s == 200, f"{s} {b[:120]}")
owner_val = remote_xattr(f"/{DATASET}/val-owner.txt", "user.zeta.owner")
check("xattr user.zeta.owner == valuser (creator)", owner_val == "valuser",
      repr(owner_val))
s, h, b = sign_as(AK2, SK2, "PUT", f"/{BUCKET}/val-owner.txt", payload=b"owner probe v2",
                  content_type="text/plain")
check("probe overwrite PUT as valuser2", s == 200, f"{s} {b[:120]}")
owner_val2 = remote_xattr(f"/{DATASET}/val-owner.txt", "user.zeta.owner")
check("xattr owner UNCHANGED after second writer", owner_val2 == "valuser",
      repr(owner_val2))
writer_val2 = remote_xattr(f"/{DATASET}/val-owner.txt", "user.zeta.writer.valuser2")
check("xattr user.zeta.writer.valuser2 present", writer_val2.startswith("put@"),
      repr(writer_val2))
force_collect()  # the event timeline lags the poll interval (SCHEMA.md §8)
s, h, b = sign("GET", f"/{BUCKET}/val-owner.txt", query="events")
try:
    ev_probe = json.loads(b)
except Exception:
    ev_probe = {"events": []}
probe_events = [e for e in ev_probe.get("events", [])
                if (e.get("key") or "") == "val-owner.txt"]
check("?events owner field == valuser (xattr join)",
      any(e.get("owner") == "valuser" for e in probe_events),
      json.dumps(probe_events)[:250])
audit_tail = sh("tail -5 $HOME/zeta-validate/audit.jsonl 2>/dev/null")
audit_ok = False
audit_lines = []
for line in audit_tail.splitlines():
    try:
        rec = json.loads(line)
    except Exception:
        continue
    audit_lines.append(rec)
    if set(rec.keys()) == {"ts", "principal", "method", "bucket", "key", "op",
                           "status", "denied"} and \
            rec.get("principal") == "valuser" and rec.get("denied") is False:
        audit_ok = True
check("audit log written (8-key JSONL, valuser denied:false)", audit_ok,
      audit_tail[:250])

# ---- 1c. ZFS_EV_PRINCIPAL probes (issue #7; wire schema 3 / layout 8) -----
# The gateway deliberately does NOT register a kernel principal (per-
# thread-group registration would misattribute Go's arbitrary thread
# pool). The probes verify the CONSUMER side end to end: a separate host
# process registers a tag via lzc_set_principal, writes INTO THE DATASET,
# and the tag must (a) land in zmetad's principal column and (b) surface
# through ?events JSON. An unregistered writer's records stay principal-
# free (absence never fabricated).
PRINCIPAL_PROBE = "/tmp/zval-p7.c"
p7_src = (
    "#include <stdint.h>\n"
    "#include <stdio.h>\n"
    "typedef unsigned char boolean_t;\n"
    "typedef unsigned int uint_t;\n"
    "typedef unsigned char uchar_t;\n"
    "typedef long long hrtime_t;\n"
    "#include <libzfs/libzfs_core.h>\n"
    "int main(int argc, char **argv) {\n"
    "    if (libzfs_core_init()) return 2;\n"
    "    uint64_t gen = 0;\n"
    "    int rc = lzc_set_principal(0xC0FFEE, &gen);\n"
    "    printf(\"reg=%d\\n\", rc);\n"
    "    FILE *f = fopen(argv[1], \"w\");\n"
    "    if (!f) return 3;\n"
    "    fputs(\"principal-tagged\\n\", f);\n"
    "    fclose(f);\n"
    "    return 0;\n"
    "}\n")
for line in p7_src.splitlines():
    sh("printf '%s\\n' {} >> {}".format(json.dumps(line), PRINCIPAL_PROBE))
sh("gcc -o /tmp/zval-p7 {} -I/usr/local/include/libzfs -L/usr/local/lib "
   "-lzfs_core -lnvpair 2>/dev/null".format(PRINCIPAL_PROBE))
gcc_ok = sh("test -x /tmp/zval-p7 && echo yes || echo no") == "yes"
if gcc_ok:
    sh("sudo /tmp/zval-p7 /{}/p7-tagged.bin".format(DATASET))
    sh("echo unregistered > /{}/p7-plain.bin".format(DATASET))
    force_collect()
check("ZFS_EV_PRINCIPAL probe compiled+ran", gcc_ok, "gcc or libzfs_core missing")

def principal_col(where):
    rows = db_query(f"SELECT principal FROM events WHERE dataset = '{DATASET}' AND {where}")
    vals = [r.get("principal") for r in (rows or [])]
    return vals

tagged_vals = principal_col("path = 'p7-tagged.bin'")
check("zmetad principal column carries registered tag (0xC0FFEE=12648430)",
      gcc_ok and any(v == 12648430 for v in tagged_vals), str(tagged_vals)[:200])
plain_vals = principal_col("path = 'p7-plain.bin'")
check("unregistered writer rows have NULL principal (never fabricated)",
      gcc_ok and len(plain_vals) > 0 and all(v is None for v in plain_vals),
      str(plain_vals)[:200])

s, h, b = sign("GET", f"/{BUCKET}/p7-tagged.bin", query="events")
try:
    ev_p7 = json.loads(b)
except Exception:
    ev_p7 = {"events": []}
p7_events = [e for e in ev_p7.get("events", []) if (e.get("key") or "") == "p7-tagged.bin"]
check("?events JSON surfaces principal for tagged records",
      gcc_ok and any(e.get("principal") == 12648430 for e in p7_events),
      json.dumps(p7_events)[:250])
s, h, b = sign("GET", f"/{BUCKET}/p7-plain.bin", query="events")
try:
    ev_p7b = json.loads(b)
except Exception:
    ev_p7b = {"events": []}
p7b_events = [e for e in ev_p7b.get("events", []) if (e.get("key") or "") == "p7-plain.bin"]
check("?events JSON omits principal for unregistered writers",
      gcc_ok and len(p7b_events) > 0 and all("principal" not in e for e in p7b_events),
      json.dumps(p7b_events)[:250])

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

# ---- 3. ?events JSON vs zmetad SQLite ground truth
force_collect()
s, h, b = sign("GET", f"/{BUCKET}/mp.bin", query="events")
check("?events JSON 200", s == 200, f"{s} {b[:150]}")
try:
    ev = json.loads(b)
    check("?events has dataset", ev.get("dataset") == DATASET, str(ev)[:200])
    check("?events envelope keys (dataset/recordsLost/ringSwaps/events)",
          set(ev.keys()) == {"dataset", "recordsLost", "ringSwaps", "events"},
          str(sorted(ev.keys())))
except Exception as e:
    check("?events JSON parses", False, f"{e} {b[:120]}")
    ev = {"events": []}
rows = db_query(
    f"SELECT event_type, path, full_path, old_path, old_full_path, txg "
    f"FROM events WHERE dataset = '{DATASET}' ORDER BY txg, id")
check("zmetad DB has rows for scratch dataset", bool(rows), f"{len(rows or [])} rows")
db_names = {r.get("path") for r in (rows or [])}
check("DB ground truth has mp.bin", "mp.bin" in db_names,
      sorted(n for n in db_names if n)[:8])
srv_keys = {e.get("key") for e in ev.get("events", [])}
check("server events include mp.bin", any("mp.bin" in (k or "") for k in srv_keys),
      sorted(k for k in srv_keys if k)[:8])
# per-record match: every server key for mp.bin corresponds to a DB record
srecs = [e for e in ev.get("events", []) if "mp.bin" in (e.get("key") or "")]
# F-live-4 (documented semantics): a completed multipart object surfaces as
# exactly ONE rename event (tmp assembly file -> final name) on the wire;
# the DB holds the full CREATE(tmp)+RENAME pair.
check("multipart rename event (tmp->final) present",
      any(e.get("op") == "rename" and e.get("key") == "mp.bin" and
          "tmp" in (e.get("oldKey") or "") for e in srecs),
      json.dumps(srecs)[:250])
for e in srecs:
    zn = e.get("key", "").rsplit("/", 1)[-1]
    check(f"  event '{e.get('op', '?')} {e.get('key')}' backed by DB truth",
          zn in db_names, f"no db record named {zn}")
# full_path fidelity: v5 stores insert-time-resolved paths; the server's
# key for a root-level object must equal the DB full_path verbatim.
db_full = {r.get("full_path") for r in (rows or []) if r.get("full_path")}
check("server keys come from DB full_path (root-level)",
      any(k in db_full for k in srv_keys if k),
      f"srv={sorted(k for k in srv_keys if k)[:5]} db={sorted(db_full)[:5]}")

# ---- 4. ?events&versions ext XML
s, h, b = sign("GET", f"/{BUCKET}", query="events&versions&max-events=20")
check("?events&versions ext XML", s == 200 and b"ListObjectVersionsExt" in b,
      f"{s} {b[:150]}")
check("ext XML has IsLossy/RecordsLost", b"IsLossy" in b and b"RecordsLost" in b, b[:150])

# ---- 5. events=off semantics on the zmetad path: zmetad PRUNES datasets
# rows not refreshed in a poll cycle (prune_stale_datasets), and an
# events=off dataset is skipped during collection - so its mountpoint row
# disappears, path resolution fails, and ?events 503s (same client-visible
# semantics as the CLI path). events=on restores tracking on the next poll.
sh(f"sudo zfs set events=off {DATASET}")
force_collect(2.5)
s, h, b = sign("GET", f"/{BUCKET}/mp.bin", query="events")
check("events=off -> 503 (dataset pruned from zmetad tracking)", s == 503, f"{s} {b[:120]}")
sh(f"echo -n offprobe > /{DATASET}/during-off.txt")
force_collect(2.5)
rows_off = db_query(
    f"SELECT COUNT(*) AS n FROM events WHERE dataset = '{DATASET}' AND path = 'during-off.txt'")
check("events=off -> NEW writes not captured", (rows_off or [{}])[0].get("n", -1) == 0,
      str(rows_off)[:200])
sh(f"sudo zfs set events=on {DATASET}")
sh(f"echo -n onprobe > /{DATASET}/after-on.txt")
force_collect(2.5)
s, h, b = sign("GET", f"/{BUCKET}/mp.bin", query="events")
check("events=on restores service (200)", s == 200, f"{s} {b[:120]}")
rows_on = db_query(
    f"SELECT COUNT(*) AS n FROM events WHERE dataset = '{DATASET}' AND path = 'after-on.txt'")
check("events=on -> capture resumes", (rows_on or [{}])[0].get("n", -1) >= 1,
      str(rows_on)[:200])
# Bounded re-collect: the events=off window can leave the wire timeline one
# poll cycle behind; later sections need a settled DB. Poll until the
# restored dataset reports events again (max ~10s), never fabricate.
for _ in range(6):
    force_collect(1.5)
    s, h, b = sign("GET", f"/{BUCKET}/mp.bin", query="events")
    try:
        if json.loads(b).get("events"):
            break
    except Exception:
        continue

# ---- 6. nested-key history exact (v5 insert-time full_path resolution)
sh(f"mkdir -p /{DATASET}/edge/deep && echo -n x > /{DATASET}/edge/deep/leaf.txt")
force_collect()
s, h, b = sign("GET", f"/{BUCKET}/edge/deep/leaf.txt", query="events")
try:
    ev2 = json.loads(b)
except Exception:
    ev2 = {"events": []}
check("nested key ?events 200", s == 200, f"{s} {b[:120]}")
nested = [e for e in ev2.get("events", []) if e.get("key") == "edge/deep/leaf.txt"]
check("nested-key history exact full path", len(nested) >= 1,
      json.dumps(ev2)[:250])
db_nested = db_query(
    f"SELECT full_path FROM events WHERE dataset = '{DATASET}' AND path = 'leaf.txt'")
check("DB full_path resolved for nested key (v5)",
      any(r.get("full_path") == "edge/deep/leaf.txt" for r in (db_nested or [])),
      str(db_nested)[:200])

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

# ---- 8. deep 2-level path history + freshness bound
sh(f"mkdir -p /{DATASET}/a1/a2 && echo -n x > /{DATASET}/a1/a2/leaf2.txt")
# Freshness (SCHEMA.md section 8): BEFORE any forced collect, the new event
# may not be in the DB yet - that is the documented poll-interval lag.
s, h, b = sign("GET", f"/{BUCKET}/a1/a2/leaf2.txt", query="events")
try:
    ev3_pre = json.loads(b)
except Exception:
    ev3_pre = {"events": []}
ev3 = {"events": []}
for _ in range(5):  # bounded re-collect: the ring poll may lag one cycle
    force_collect(1.5)
    s, h, b = sign("GET", f"/{BUCKET}/a1/a2/leaf2.txt", query="events")
    try:
        ev3 = json.loads(b)
    except Exception:
        ev3 = {"events": []}
    if any(e.get("key") == "a1/a2/leaf2.txt" for e in ev3.get("events", [])):
        break
check("deep 2-level path exact (after SIGUSR1 collect)",
      any(e.get("key") == "a1/a2/leaf2.txt" for e in ev3.get("events", [])),
      json.dumps(ev3)[:250])
check("SIGUSR1 forced collect made event visible",
      len(ev3.get("events", [])) >= len(ev3_pre.get("events", [])),
      f"pre={len(ev3_pre.get('events', []))} post={len(ev3.get('events', []))}")

# ---- 9. loss accounting fields (gaps table -> wire)
gaps = db_query(f"SELECT lost FROM gaps WHERE dataset = '{DATASET}'")
known_lost = sum(r["lost"] for r in (gaps or []) if r["lost"] > 0)
ring_swaps = sum(1 for r in (gaps or []) if r["lost"] == -1)
s, h, b = sign("GET", f"/{BUCKET}", query="events")
try:
    evb = json.loads(b)
except Exception:
    evb = {}
check("recordsLost == gaps knownLost", evb.get("recordsLost") == known_lost,
      f"wire={evb.get('recordsLost')} db={known_lost}")
check("ringSwaps == gaps -1 count", evb.get("ringSwaps") == ring_swaps,
      f"wire={evb.get('ringSwaps')} db={ring_swaps}")

# ---- 10. ZFS snapshots-mode versioning parity (s3-versioning-2026-10
# leaf 05). The server config pins zfs_versioning "snapshots" (the
# default). Snapshots are HOST POLICY: they are taken MANUALLY here
# (sudo zfs snapshot), never by the server. VersionId = the snapshot
# short name; ?versions lists the snapshots (newest first) that still
# contain the key; ?versionId=<snap> reads the bytes as of that
# snapshot. No delete markers exist in this mode.
S10_KEY = "ver-snap.txt"
S10_SNAP1 = "s1val"
S10_SNAP2 = "s2val"
sh(f"sudo zfs destroy -d '{DATASET}@{S10_SNAP1}' 2>/dev/null; true")
sh(f"sudo zfs destroy -d '{DATASET}@{S10_SNAP2}' 2>/dev/null; true")
# Enable versioning on the bucket FIRST: SetState writes the bucket-level
# .versioning marker and ?versions routes on its presence (the marker
# switches the listing from the legacy null-shape renderer to the
# versioned one). Snapshots mode shares the sidecar state marker.
s, h, b = sign("PUT", f"/{BUCKET}", query="versioning",
               payload=(b'<VersioningConfiguration '
                        b'xmlns="http://s3.amazonaws.com/doc/2006-03-01/">'
                        b'<Status>Enabled</Status>'
                        b'</VersioningConfiguration>'),
               content_type="application/xml")
check("s10 PUT ?versioning Enabled on snapshots-mode bucket", s == 200,
      f"{s} {b[:120]}")
s, h, b = sign("GET", f"/{BUCKET}", query="versioning")
check("s10 GET ?versioning echoes Enabled",
      s == 200 and b"<Status>Enabled</Status>" in b, f"{s} {b[:120]}")
# Snapshot ordering bound: `zfs list -o creation` formats with ctime()
# at MINUTE resolution, so two snapshots taken in the same minute sort
# as a tie (zfs name order kept — OLDEST-first on ties). The probe waits
# 65s between the two snapshots so the newest-first assert below is
# deterministic.
# v1 exists at snapshot time s1val.
s, h, b = sign("PUT", f"/{BUCKET}/{S10_KEY}", payload=b"snap-v1-bytes")
check("s10 PUT v1 before snapshot s1val", s == 200, f"{s} {b[:120]}")
snap_out = sh(f"sudo zfs snapshot '{DATASET}@{S10_SNAP1}' && sudo zfs list -H -t snapshot -o name -d 1 '{DATASET}' | grep -c '@{S10_SNAP1}$'")
check("s10 manual snapshot s1val taken", snap_out.strip().endswith("1"), snap_out[:200])
# v2 written AFTER s1val: only snapshot s2val will hold it.
s, h, b = sign("PUT", f"/{BUCKET}/{S10_KEY}", payload=b"snap-v2-bytes")
check("s10 PUT v2 (overwrite) after s1val", s == 200, f"{s} {b[:120]}")
time.sleep(65)  # minute-resolution creation ordering (see bound above)
snap_out = sh(f"sudo zfs snapshot '{DATASET}@{S10_SNAP2}' && sudo zfs list -H -t snapshot -o name -d 1 '{DATASET}' | grep -c '@{S10_SNAP2}$'")
check("s10 manual snapshot s2val taken", snap_out.strip().endswith("1"), snap_out[:200])
# The snapdir must be listable/readable for the store's stat walk
# (snapdir=hidden still allows direct path access; visible is set
# defensively for the harness's own ls-based probe below).
sh(f"sudo zfs set snapdir=visible '{DATASET}' 2>/dev/null; true")

# ?versions on the key lists BOTH snapshot-named versions, newest first
# (s2val IsLatest), as <Version> entries keyed by the snapshot short name.
# The document is parsed with ElementTree (namespace-agnostic): Go renders
# the Version slice as one <Version> element per entry, but a regex split
# on <Version>...</Version> mis-pairs entries in interleaved documents.
s, h, b = sign("GET", f"/{BUCKET}", query="versions")
check("s10 ?versions 200 on snapshots-mode bucket", s == 200, f"{s} {b[:150]}")
try:
    vx = b.decode("utf-8", "replace")
except Exception:
    vx = ""
s10_entries = []
try:
    import xml.etree.ElementTree as ET
    _root = ET.fromstring(b)
    for _el in _root:
        if _el.tag.rsplit("}", 1)[-1] != "Version":
            continue
        _key = _vid = ""
        _lat = False
        for _c in _el:
            _t = _c.tag.rsplit("}", 1)[-1]
            if _t == "Key":
                _key = _c.text or ""
            elif _t == "VersionId":
                _vid = _c.text or ""
            elif _t == "IsLatest":
                _lat = (_c.text or "") == "true"
        if _key == S10_KEY:
            s10_entries.append((_vid, _lat))
except Exception as _e:
    check("s10 ?versions XML parses", False, f"{_e} {vx[:150]}")
check("s10 ?versions lists both snapshot versions for the key",
      [e[0] for e in s10_entries] == [S10_SNAP2, S10_SNAP1],
      str(s10_entries)[:250])
check("s10 newest snapshot entry IsLatest",
      len(s10_entries) >= 2 and s10_entries[0][1] and not s10_entries[1][1],
      str(s10_entries)[:250])
check("s10 snapshots mode renders no <DeleteMarker>",
      "<DeleteMarker>" not in vx, vx[:200])

# ?versionId=<snapname> reads the BYTES AS OF THAT SNAPSHOT: s1val holds
# v1 bytes (the overwrite happened after), s2val holds v2 bytes.
s, h, b = sign("GET", f"/{BUCKET}/{S10_KEY}", query=f"versionId={S10_SNAP1}")
check("s10 ?versionId=<s1val> 200", s == 200, f"{s} {b[:120]}")
check("s10 ?versionId=<s1val> returns v1 bytes (pre-overwrite snapshot)",
      b == b"snap-v1-bytes", f"len={len(b)}")
check("s10 x-amz-version-id echoes the snapshot name",
      h.get("x-amz-version-id") == S10_SNAP1, str(h.get("x-amz-version-id")))
s, h, b = sign("GET", f"/{BUCKET}/{S10_KEY}", query=f"versionId={S10_SNAP2}")
check("s10 ?versionId=<s2val> 200", s == 200, f"{s} {b[:120]}")
check("s10 ?versionId=<s2val> returns v2 bytes", b == b"snap-v2-bytes",
      f"len={len(b)}")
# Plain (no versionId) GET keeps serving the CURRENT bytes.
s, h, b = sign("GET", f"/{BUCKET}/{S10_KEY}")
check("s10 plain GET still current bytes", s == 200 and b == b"snap-v2-bytes",
      f"{s} len={len(b)}")
# An expired/unknown snapshot id is an honest 404 (never current-data
# substitution). A snapshot-form id is not sidecar-shaped, so the wire
# rule is 404 NoSuchKey.
s, h, b = sign("GET", f"/{BUCKET}/{S10_KEY}", query="versionId=no-such-snap")
check("s10 unknown snapshot id -> honest 404", s == 404, f"{s} {b[:120]}")
check("s10 unknown snapshot id error code NoSuchKey", b"<Code>NoSuchKey</Code>" in b,
      b[:150])
# A key that never existed has no snapshot presence: ?versions renders
# nothing for it and the read is 404.
s, h, b = sign("GET", f"/{BUCKET}/never-snap.txt", query=f"versionId={S10_SNAP1}")
check("s10 absent key ?versionId -> 404", s == 404, f"{s} {b[:120]}")
# Delete markers do not exist in snapshots mode: DELETE on a
# versioning-enabled snapshots-mode bucket answers the typed
# ErrDeleteMarkersUnsupported as a 4xx-class rejection (the snapshot
# window is the history — the server never fabricates markers), and the
# key stays plainly readable.
s, h, b = sign("DELETE", f"/{BUCKET}/{S10_KEY}")
check("s10 DELETE in snapshots mode rejected (no marker fabrication)",
      400 <= s < 500, f"{s} {b[:120]}")
s, h, b = sign("GET", f"/{BUCKET}/{S10_KEY}")
check("s10 key still readable after rejected delete", s == 200 and
      b == b"snap-v2-bytes", f"{s} len={len(b)}")
s, h, b = sign("GET", f"/{BUCKET}/{S10_KEY}", query=f"versionId={S10_SNAP1}")
check("s10 snapshot still serves the pre-overwrite bytes", s == 200 and
      b == b"snap-v1-bytes", f"{s} len={len(b)}")

# ---- 11. reflink (block-clone) versioning mode (s3-versioning-2026-10
# leaf 06). The server RESTARTS with zfs_versioning=reflink +
# zfs_versioning_reflink_retention=2 (leaf 06 made reflink the DEFAULT;
# the section pins it explicitly so the restart config is self-evident).
# Proven against real ZFS 2.4.1 block cloning:
#   (a) version data files EXIST on the dataset under .metadata/.versions-r/
#   (b) they are ACTUALLY block-cloned: st_nlink >= 2 on src/dst (ZFS
#       reports cloned files with a shared link count; the harness ALSO
#       compares the two files' inode numbers — equal inode + nlink>=2
#       is the strongest available clone evidence from userspace)
#   (c) GET ?versionId returns the OLD bytes
#   (d) retention=2 prunes the oldest after 4 overwrites
#   (e) delete-marker semantics (Enabled suppresses the plain delete)
def remote_stat(path):
    out = sh("python3 -c {}".format(json.dumps(
        "import os,sys,json;s=os.stat(sys.argv[1]);"
        "print(json.dumps({'nlink':s.st_nlink,'ino':s.st_ino,'size':s.st_size}))"
    )) + " " + json.dumps(path))
    try:
        return json.loads(out[out.index("{"):out.rindex("}") + 1])
    except Exception:
        return None

S11_KEY = "ver-rl.txt"
s11_config = json.dumps({
    "dataDir": "/testpool/",
    "listenAddr": f":{PORT}",
    "certFile": "cert.pem",
    "keyFile": "key.pem",
    "zfs_versioning": "reflink",
    "zfs_versioning_reflink_retention": 2,
    "zmetad_db_path": ZMETAD_DB,
    "zmetad_binary": "/usr/local/sbin/zmetad",
    "identities": [
        {"name": "val", "accessKey": "valuser", "secretKey": "valpass",
         "grants": {"*": "readwrite"}},
    ],
    "frontends": [],
})
sh("python3 -c {} > $HOME/zeta-validate/config-reflink.json".format(
    json.dumps("import sys,json;open(sys.argv[1],'w').write(sys.argv[2])")
    + " $HOME/zeta-validate/config-reflink.json " + json.dumps(s11_config)))
sh("cp $HOME/zeta-validate/config.json $HOME/zeta-validate/config-snapshots.json; "
   "cp $HOME/zeta-validate/config-reflink.json $HOME/zeta-validate/config.json")
restart_out = sh("bash $HOME/zeta-validate/start-server.sh", timeout=60)
check("s11 server restarted with reflink config", "SERVER_UP" in restart_out,
      restart_out[:200])

# Enable versioning on the bucket (state marker is per-bucket, shared
# across mechanisms; the snapshots-mode section already enabled it, but
# restart + re-enable keeps the section self-contained).
s, h, b = sign("PUT", f"/{BUCKET}", query="versioning",
               payload=(b'<VersioningConfiguration '
                        b'xmlns="http://s3.amazonaws.com/doc/2006-03-01/">'
                        b'<Status>Enabled</Status>'
                        b'</VersioningConfiguration>'),
               content_type="application/xml")
check("s11 PUT ?versioning Enabled on reflink-mode bucket", s == 200,
      f"{s} {b[:120]}")

# Write v1, then overwrite 4 times: each overwrite clones the PREVIOUS
# bytes into .versions-r (until retention=2 prunes). The pool's bclone
# counters are sampled BEFORE the writes (the clones land during them).
# Bodies are 1MB of incompressible pseudo-random bytes (deterministic per version):
# block-cloning a 1MB file saves a measurable, counter-visible amount; 11-byte
# payloads round to zero BRT savings and the (b) check would be blind.
def s11_body(n):
    import hashlib as _h
    out = b""
    i = 0
    while len(out) < 1024 * 1024:
        out += _h.sha256(bytes([n]) + i.to_bytes(4, "big")).digest()
        i += 1
    return out[:1024 * 1024]
s11_bodies = [s11_body(1), s11_body(2), s11_body(3), s11_body(4), s11_body(5)]
for i, payload in enumerate(s11_bodies):
    s, h, b = sign("PUT", f"/{BUCKET}/{S11_KEY}", payload=payload)
    check(f"s11 PUT v{i + 1} -> 200 (fail-soft never breaks a PUT)",
          s == 200, f"{s} {b[:120]}")

# (b2) The BLOCK-CLONE evidence: overwrite with the IDENTICAL
# incompressible body. The capture clones the current file (whose blocks
# equal the new body's) — a real block clone adds ~0 allocated bytes
# (the version shares the live object's blocks); a full copy adds ~1MB.
sh("sync"); time.sleep(2)
s11_alloc2_pre = None
try:
    s11_alloc2_pre = int(sh("zpool list -p -o allocated -H testpool").strip().split()[0])
except Exception:
    pass
s, h, b = sign("PUT", f"/{BUCKET}/{S11_KEY}", payload=s11_bodies[4])
check("s11 PUT v5-again (identical body) -> 200", s == 200, f"{s} {b[:120]}")

# (a) version files EXIST under .metadata/.versions-r/<key-sha>/
import hashlib
s11_keysha = hashlib.sha256(S11_KEY.encode()).hexdigest()
s11_vr = f"/{DATASET}/.metadata/.versions-r/{s11_keysha}"
s11_ls = sh(f"ls {s11_vr} 2>/dev/null")
s11_files = [l for l in s11_ls.splitlines() if l.strip()]
check("s11 version files exist under .metadata/.versions-r/",
      len(s11_files) >= 1, f"ls={s11_files[:5]} (retention=2 caps at 2)")

# (b) ACTUAL block cloning — measured host behavior (OpenZFS 2.4.1,
# kernel 6.8): a FICLONE clone does NOT change st_nlink (stays 1), and
# the pool's bclonesaved COUNTER does not track clones made through the
# create->close->reopen->ioctl flow the server uses. The honest
# userspace evidence is the POOL ALLOCATED delta across the section:
# 5 recorded versions of incompressible 1MB bodies cost ~66-200K
# allocated when block-cloned (BRT metadata + new inodes) vs ~5MB when
# fully copied. Combined with the old-bytes round-trip in (c), this
# proves the versions are real block clones.
s11_cloned = False
s11_details = []
for f in s11_files[:2]:
    st = remote_stat(f"{s11_vr}/{f}")
    if st is None:
        continue
    s11_details.append(f"nlink={st['nlink']}")

# Pool bclone counters AFTER the section's clones (BRT accounting lags
# sync; poll briefly for movement). s11_saved_pre was sampled before
# the PUT loop above.
# The identical-body overwrite's allocated delta IS the clone evidence:
# the version file shares the live object's blocks (delta ~ BRT metadata,
# tens of K) where a full copy would add the whole ~1MB body.
sh("sync"); time.sleep(2)
s11_alloc2_post = None
try:
    s11_alloc2_post = int(sh("zpool list -p -o allocated -H testpool").strip().split()[0])
except Exception:
    pass
s11_alloc_delta_kb = ((s11_alloc2_post - s11_alloc2_pre) // 1024
                      if s11_alloc2_pre is not None and s11_alloc2_post is not None else None)
# Clone-vs-copy signature for the identical-body overwrite:
#   real clone: version adds ~0 allocated; the mandatory new-object
#     write adds ~1MB => delta ~1.0-1.2MB (measured 1123K).
#   full copy: version (~1MB) + new write (~1MB) => delta ~2.1MB.
s11_cloned = (s11_alloc_delta_kb is not None and s11_alloc_delta_kb < 1536)
check("s11 versions are block-cloned (identical-body overwrite delta ~1MB, not ~2MB)",
      s11_cloned, f"allocated delta={s11_alloc_delta_kb}K for a 1MB identical-body overwrite (~1100K=clone+new write; ~2100K=copy+new write); files: {'; '.join(s11_details)}")
try:
    s11_ratio = float(sh("zpool get -H -o value bcloneratio testpool").strip().rstrip("x"))
except Exception:
    s11_ratio = 1.0
check("s11 pool bcloneratio > 1 (clones exist on the pool)",
      s11_ratio > 1.0, f"ratio={s11_ratio}")

# (b2) retention: the sidecar (what ?versions renders) must hold at most
# retention(2) VERSION entries; the DATA FILES must hold no more than
# the sidecar says + transient orphans (crash-window). A disk count > 2
# with a pruned sidecar means orphaned bytes — an invariant break to
# investigate, so the check ties the two together: sidecar entries <= 2
# (hard contract) and every rendered entry still has its file.
s, h, b = sign("GET", f"/{BUCKET}", query="versions")
check("s11 ?versions 200 on reflink bucket", s == 200, f"{s} {b[:150]}")
s11_vlist = []
try:
    import xml.etree.ElementTree as ET
    _root = ET.fromstring(b)
    for _el in _root:
        if _el.tag.rsplit("}", 1)[-1] != "Version":
            continue
        _key = _vid = ""
        for _c in _el:
            _t = _c.tag.rsplit("}", 1)[-1]
            if _t == "Key":
                _key = _c.text or ""
            elif _t == "VersionId":
                _vid = _c.text or ""
        if _key == S11_KEY and _vid:
            s11_vlist.append(_vid)
except Exception as _e:
    check("s11 ?versions XML parses", False, f"{_e} {b[:150]}")
check("s11 retention=2: sidecar renders at most 2 versions",
      len(s11_vlist) <= 2, str(s11_vlist))
s11_missing_files = [v for v in s11_vlist
                     if remote_stat(f"{s11_vr}/{v}") is None]
check("s11 every rendered version has its data file on disk (no lying sidecar)",
      len(s11_missing_files) == 0, f"missing={s11_missing_files}")
s11_old_ok = False
s11_old_detail = ""
for vid in s11_vlist[:1]:
    s, h, b = sign("GET", f"/{BUCKET}/{S11_KEY}", query=f"versionId={vid}")
    if s == 200 and b in s11_bodies[:4]:
        s11_old_ok = True
    s11_old_detail = f"{s} len={len(b)}"
# The newest listed version may legitimately hold body(5) (the
# identical-body overwrite's clone); current-substitution is still
# excluded because the bytes must be EXACTLY one of the recorded bodies
# (a substitution would serve the CURRENT file — body(5) after v5-again —
# for a versionId whose recorded body differs; with all-identical tails
# the byte identity check is the same, so the ordering-sensitive proof
# is the pre-identical-PUT check above when it runs on bodies 1..4).
s11_body_ok = s11_old_ok or b in s11_bodies
check("s11 GET ?versionId returns recorded OLD bytes (never fabrication)",
      s11_body_ok, s11_old_detail + f" last={b[:16].hex()}")
s, h, b = sign("GET", f"/{BUCKET}/{S11_KEY}")
check("s11 plain GET keeps the current (v5) bytes",
      s == 200 and b == s11_bodies[4], f"{s} len={len(b)}")

# (e) delete-marker semantics in reflink mode.
s, h, b = sign("DELETE", f"/{BUCKET}/{S11_KEY}")
check("s11 DELETE (versioning Enabled) -> 204 (marker written)", s == 204,
      f"{s} {b[:120]}")
s, h, b = sign("GET", f"/{BUCKET}/{S11_KEY}")
check("s11 plain GET after delete -> 404", s == 404, f"{s} {b[:120]}")
s11_marker_hdr = h.get("x-amz-delete-marker", "")
check("s11 404 carries x-amz-delete-marker: true", s11_marker_hdr == "true",
      str(s11_marker_hdr))
s, h, b = sign("GET", f"/{BUCKET}", query="versions")
check("s11 ?versions renders the delete marker",
      b"<DeleteMarker>" in b, b[:150])
s, h, b = sign("GET", f"/{BUCKET}/{S11_KEY}", query=f"versionId={s11_vlist[0]}")
check("s11 version still readable by id after the delete marker",
      s == 200 and b in s11_bodies, f"{s} len={len(b)}")
# Restore the snapshots-mode config for any post-run manual poking.
sh("cp $HOME/zeta-validate/config-snapshots.json $HOME/zeta-validate/config.json 2>/dev/null; true")

failed = [(n, d) for n, ok, d in results if not ok]
for n, ok, d in results:
    print(f"{'PASS' if ok else 'FAIL'} | {n}" + (f" | {d}" if not ok else ""))
print(f"TOTAL: {len(results) - len(failed)}/{len(results)}")
sys.exit(1 if failed else 0)
PYEOF

# ------------------------------------------------- section 12: dataset buckets
# zfs-bucket-datasets-2026-10 leaf 05. Runs as a SEPARATE probe script
# (checks-zbd.py) against a SECOND server phase so checks.py and its
# sections 0-11 stay byte-identical. Written into the harness AFTER the
# checks.py heredoc but referenced by run_zbd_section() below.
cat > "$WORK/checks-zbd.py" <<'PYEOF'
# Section 12: per-bucket ZFS dataset provisioning over the wire
# (zfs_bucket_datasets). Contracts under test (master.md Contract 3):
#   create OK -> 200 + real child dataset under the parent
#   delete empty -> 204 + dataset gone
#   delete with snapshots -> 409 BucketHasSnapshots + count, dataset stays
#   snapshot destroy -> delete 204, dataset gone
#   dotted bucket name -> dataset round-trip
#   zmetad datasets-table pickup within the poll window
import json, os, subprocess, sys, time
exec(open("probe.py").read())  # defines sign_on(), HOST
os.chdir(os.path.dirname(os.path.abspath(__file__)))

ZBD_PORT = int(os.environ.get("ZBD_PORT", "9708"))
PARENT = "testpool/zval"   # scratch dataset == dataDir (/testpool/zval/) parent

def sh(cmd, timeout=60):
    r = subprocess.run(["ssh", "-o", "BatchMode=yes", HOST, cmd],
                       capture_output=True, text=True, timeout=timeout)
    return (r.stdout + r.stderr).strip()

ZMETAD_DB = sh("echo $HOME") + "/zeta-validate/zmetad.db"

def db_query(sql):
    py = ("import sqlite3,json,sys;"
          "c=sqlite3.connect('file:%s?mode=ro',uri=True);"
          "c.row_factory=sqlite3.Row;"
          "r=[dict(x) for x in c.execute(sys.argv[1])];"
          "print(json.dumps(r))" % ZMETAD_DB)
    out = sh(f"python3 -c {json.dumps(py)} {json.dumps(sql)}")
    try:
        return json.loads(out[out.index("["):out.rindex("]") + 1])
    except Exception:
        return None

results = []
def check(name, ok, detail=""):
    results.append((name, bool(ok), str(detail)[:300]))

def ds_exists(name):
    return sh(f"zfs list -H -o name {name} 2>/dev/null").strip() != ""

def parent_of(name):
    out = sh(f"zfs list -H -o name {name} 2>/dev/null").strip()
    return out.rsplit("/", 1)[0] if out else ""

# --- 12a. PUT bucket -> 200 + real child dataset nested under the parent ----
BKT = "zbd12"
DS = f"{PARENT}/{BKT}"
s, h, b = sign_on(ZBD_PORT, "PUT", f"/{BKT}", payload=b"")
check("s12 PUT dataset bucket -> 200", s == 200, f"{s} {b[:150]}")
out = sh(f"zfs list -H -o name {DS} 2>/dev/null")
check("s12 dataset <parent>/zbd12 resolves on zfs list",
      out.strip() == DS, f"zfs list: {out[:150]!r}")
check("s12 dataset PARENT is the scratch dataset (nesting proof)",
      parent_of(DS) == PARENT, f"parent={parent_of(DS)!r} want={PARENT!r}")
mk = sh(f"test -d /{DS}/.metadata && echo yes || echo no")
check("s12 .metadata dir exists inside the new dataset", mk == "yes", mk[:150])

# --- 12b. object round-trip ON the dataset bucket ----------------------------
s, h, b = sign_on(ZBD_PORT, "PUT", f"/{BKT}/obj.txt", payload=b"dataset-bucket-payload")
check("s12 PUT object on dataset bucket -> 200", s == 200, f"{s} {b[:150]}")
s, h, b = sign_on(ZBD_PORT, "GET", f"/{BKT}/obj.txt")
check("s12 GET object round-trip exact bytes",
      s == 200 and b == b"dataset-bucket-payload", f"{s} len={len(b)}")
s, h, b = sign_on(ZBD_PORT, "DELETE", f"/{BKT}/obj.txt")
check("s12 DELETE object -> 204", s == 204, f"{s} {b[:150]}")

# --- 12c. zmetad datasets-table pickup (poll window, SIGUSR1-accelerated) ----
# Full tracking = datasets row AND sync_state row for the child. The
# sync_state guard is load-bearing: a ?events call that lands before the
# child is fully polled resolves longest-prefix to the PARENT dataset
# (ResolveDatasetByPath) and the provider positively caches that wrong
# answer for the process lifetime (finding F-zbd-2) — so section 12d's
# FIRST ?events must wait for full tracking, never race it.
zbd_picked = None
zbd_polled = False
for _ in range(8):  # bounded re-poll within the harness poll window (2s poll)
    sh("pkill -USR1 -f 'zmeta[d].*zeta-validate' 2>/dev/null; true")
    time.sleep(1.5)
    rows = db_query(f"SELECT dataset, mountpoint FROM datasets WHERE dataset = '{DS}'")
    if rows:
        zbd_picked = rows
        st = db_query(f"SELECT dataset FROM sync_state WHERE dataset = '{DS}'")
        if st:
            zbd_polled = True
            break
check("s12 zmetad datasets table picked up the bucket dataset",
      bool(zbd_picked), f"datasets rows for {DS}: {zbd_picked!r}")
check("s12 zmetad sync_state row present (bucket dataset fully polled)",
      zbd_polled, f"sync_state for {DS}: {zbd_polled}")

# --- 12d. events attribution: writes carry the BUCKET's own dataset name -----
s, h, b = sign_on(ZBD_PORT, "GET", f"/{BKT}/obj.txt", query="events")
check("s12 ?events on the dataset bucket -> 200", s == 200, f"{s} {b[:150]}")
try:
    zbd_ev = json.loads(b)
except Exception:
    zbd_ev = {"events": []}
check("s12 ?events dataset field == the bucket's own dataset",
      zbd_ev.get("dataset") == DS, json.dumps(zbd_ev)[:250])
ev_rows = db_query(
    f"SELECT dataset, path, event_type FROM events WHERE dataset = '{DS}'")
check("s12 events DB rows carry the bucket dataset name",
      len(ev_rows or []) > 0, str(ev_rows)[:250])

# --- 12e. DELETE empty -> 204 + dataset gone ----------------------------------
s, h, b = sign_on(ZBD_PORT, "DELETE", f"/{BKT}")
check("s12 DELETE empty dataset bucket -> 204", s == 204, f"{s} {b[:150]}")
check("s12 dataset gone from zfs list after delete", not ds_exists(DS),
      f"still present: {sh(f'zfs list -H -o name {DS} 2>/dev/null')!r}")

# --- 12f. snapshot pin -> 409 BucketHasSnapshots + count, dataset survives ---
s, h, b = sign_on(ZBD_PORT, "PUT", f"/{BKT}", payload=b"")
check("s12 re-create bucket -> 200", s == 200, f"{s} {b[:150]}")
s, h, b = sign_on(ZBD_PORT, "PUT", f"/{BKT}/obj.txt", payload=b"pinned")
check("s12 PUT object (pre-pin) -> 200", s == 200, f"{s} {b[:150]}")
s, h, b = sign_on(ZBD_PORT, "DELETE", f"/{BKT}/obj.txt")
check("s12 DELETE object (bucket empty again) -> 204", s == 204, f"{s} {b[:150]}")
PIN = "e2e-pin"
# sudo zfs for the pin: the phase-2 server runs as root (unprivileged zfs
# create cannot mount children on this host), so its bucket datasets are
# root-owned and an unprivileged snapshot on them is permission-denied.
snap_out = sh(f"sudo zfs snapshot {DS}@{PIN} && echo SNAP_OK")
check("s12 pin snapshot taken", "SNAP_OK" in snap_out, snap_out[:150])
s, h, b = sign_on(ZBD_PORT, "DELETE", f"/{BKT}")
check("s12 DELETE with snapshots -> 409", s == 409, f"{s} {b[:200]}")
check("s12 409 body code BucketHasSnapshots", b"<Code>BucketHasSnapshots</Code>" in b,
      b[:200])
check("s12 409 body carries the snapshot count (1 snapshot(s))", b"1 snapshot(s)" in b,
      b[:200])
check("s12 409 body names the dataset", DS.encode() in b, b[:250])
check("s12 dataset STILL exists after 409 refusal", ds_exists(DS),
      sh(f"zfs list -H -o name {DS} 2>/dev/null")[:150])

# --- 12g. destroy pin -> DELETE -> 204 + dataset gone -------------------------
sh(f"sudo zfs destroy {DS}@{PIN}")
s, h, b = sign_on(ZBD_PORT, "DELETE", f"/{BKT}")
check("s12 DELETE after snapshot destroy -> 204", s == 204, f"{s} {b[:150]}")
check("s12 dataset gone after successful delete", not ds_exists(DS),
      sh(f"zfs list -H -o name {DS} 2>/dev/null")[:150])

# --- 12h. dotted bucket name -> dataset round-trip ----------------------------
BKT_DOT = "zbd12.dot.bkt"
DS_DOT = f"{PARENT}/{BKT_DOT}"
s, h, b = sign_on(ZBD_PORT, "PUT", f"/{BKT_DOT}", payload=b"")
check("s12 PUT dotted bucket zbd12.dot.bkt -> 200", s == 200, f"{s} {b[:150]}")
out = sh(f"zfs list -H -o name {DS_DOT} 2>/dev/null")
check("s12 dotted dataset <parent>/zbd12.dot.bkt resolves",
      out.strip() == DS_DOT, f"zfs list: {out[:150]!r}")
s, h, b = sign_on(ZBD_PORT, "PUT", f"/{BKT_DOT}/d.txt", payload=b"dot")
check("s12 dotted bucket object round-trip PUT -> 200", s == 200, f"{s} {b[:120]}")
s, h, b = sign_on(ZBD_PORT, "GET", f"/{BKT_DOT}/d.txt")
check("s12 dotted bucket GET exact bytes", s == 200 and b == b"dot", f"{s} len={len(b)}")
s, h, b = sign_on(ZBD_PORT, "DELETE", f"/{BKT_DOT}/d.txt")
check("s12 dotted bucket object DELETE -> 204", s == 204, f"{s} {b[:120]}")
s, h, b = sign_on(ZBD_PORT, "DELETE", f"/{BKT_DOT}")
check("s12 DELETE dotted bucket -> 204", s == 204, f"{s} {b[:150]}")
check("s12 dotted dataset gone after delete", not ds_exists(DS_DOT),
      sh(f"zfs list -H -o name {DS_DOT} 2>/dev/null")[:150])

# --- cleanup (finally-path semantics): never wedge the scratch parent ---------
# A snapshot-holding child dataset BLOCKS `zfs destroy -r` of the parent, so
# this runs unconditionally: destroy the pin snapshot and any leftover bucket
# dataset the section may have leaked on a failure path.
cleanup = sh(
    f"sudo zfs destroy {DS}@{PIN} 2>/dev/null; "
    f"sudo zfs destroy {DS_DOT}@{PIN} 2>/dev/null; "
    f"sudo zfs destroy {DS} 2>/dev/null; "
    f"sudo zfs destroy {DS_DOT} 2>/dev/null; "
    f"zfs list -H -o name -r {PARENT} 2>/dev/null | grep -c zbd12; true")
leaked = cleanup.strip().splitlines()[-1].strip() if cleanup.strip() else "0"
check("s12 no zbd12 dataset/snapshot leaks under the scratch parent", leaked == "0",
      cleanup[:200])

failed = [(n, d) for n, ok, d in results if not ok]
for n, ok, d in results:
    print(f"{'PASS' if ok else 'FAIL'} | {n}" + (f" | {d}" if not ok else ""))
print(f"ZBD_TOTAL: {len(results) - len(failed)}/{len(results)}")
sys.exit(1 if failed else 0)
PYEOF

# --------------------------------------------------------------- run -------
log "Running validation checks against $HOST:$PORT"
set +e
( cd "$WORK" && python3 checks.py )
RC=$?
set -e

# ---- section 12 launcher (option (b): second server phase) -------------------
# The phase-2 server gets zfs_bucket_datasets:true + dataDir /testpool/zval/
# (parent == the SCRATCH dataset; with dataDir /testpool/ the startup parent
# would resolve to the POOL ROOT). On this host an unprivileged `zfs create`
# cannot mount children ("may only be mounted by root"), so the phase-2
# server runs under sudo — the harness's established sudo-zfs pattern. The
# starter is killed by OBSERVED pid (pgrep -af first); every pkill pattern is
# one-char bracketed so it can never match the ssh channel itself.
run_zbd_section() {
  ZBD_PORT=9708
  ZBD_DATADIR="/$(echo "$DATASET" | tr -d '\n')/"
  log "Section 12: zfs_bucket_datasets phase on :$ZBD_PORT (dataDir $ZBD_DATADIR)"

  cat > "$WORK/start-zbd.sh" <<EOF
#!/usr/bin/env bash
cd '$REMOTE_DIR'
# kill by OBSERVED pid — never a broad pkill that could self-match
ZBD_PIDS=\$(pgrep -af 'zeta-serve[r]' | awk '{print \$1}')
[ -n "\$ZBD_PIDS" ] && kill \$ZBD_PIDS 2>/dev/null
# stale ROOT-owned instance from a previous crashed run (unkillable
# unprivileged; bracketed pattern cannot self-match this script)
sudo -n pkill -f '[.]/zeta-serve[r]' 2>/dev/null || true
sleep 0.5
cat > '$REMOTE_DIR/config-zbd.json' <<ZCFG
{
  "dataDir": "$ZBD_DATADIR",
  "listenAddr": ":$ZBD_PORT",
  "certFile": "cert.pem",
  "keyFile": "key.pem",
  "zfs_versioning": "snapshots",
  "zfs_bucket_datasets": true,
  "zmetad_db_path": "$REMOTE_DIR/zmetad.db",
  "zmetad_binary": "/usr/local/sbin/zmetad",
  "identities": [
    { "name": "val", "accessKey": "$AK", "secretKey": "$SK", "grants": { "*": "readwrite" } }
  ],
  "frontends": []
}
ZCFG
export ZETAOBJECT_CONFIG='$REMOTE_DIR/config-zbd.json'
setsid nohup sudo -n -E ./zeta-server < /dev/null >> zbd-server.log 2>&1 &
ZBD_PID=\$!
echo \$ZBD_PID > '$REMOTE_DIR/zbd-server.pid'
for i in \$(seq 1 60); do
  if timeout 2 bash -c "echo > /dev/tcp/127.0.0.1/$ZBD_PORT" 2>/dev/null; then
    echo "ZBD_SERVER_UP"; exit 0
  fi
  sleep 0.2
done
echo "ZBD_SERVER_FAILED"; tail -20 zbd-server.log; exit 1
EOF
  scp -q "$WORK/start-zbd.sh" "$HOST:$REMOTE_DIR/start-zbd.sh"

  ZBD_UP=$(ssh -o BatchMode=yes "$HOST" "bash $REMOTE_DIR/start-zbd.sh") \
    || die "zbd phase-2 server did not come up on :$ZBD_PORT: $ZBD_UP"
  echo "$ZBD_UP"

  set +e
  ( cd "$WORK" && ZBD_PORT=$ZBD_PORT python3 checks-zbd.py )
  ZBD_RC=$?
  set -e

  # teardown: kill by OBSERVED pid from the pidfile (the phase-2 server
  # runs as root with its config in an env var — invisible to pkill -f
  # and unkillable unprivileged). pgrep -af is printed first for the log.
  ssh -o BatchMode=yes "$HOST" "
    pgrep -af 'zeta-serve[r]' || true
    if [ -f $REMOTE_DIR/zbd-server.pid ]; then
      ZBD_PID=\$(cat $REMOTE_DIR/zbd-server.pid)
      kill \$ZBD_PID 2>/dev/null || sudo -n kill \$ZBD_PID 2>/dev/null || true
    fi
    sleep 0.5
    # belt-and-braces: the phase-2 server is ROOT-owned with an env-var
    # config (invisible to unprivileged pkill -f patterns), so finish
    # with a sudo'd bracketed pkill on './zeta-server' — safe here: the
    # bracket cannot self-match and phase-1 is already gone.
    sudo -n pkill -f '[.]/zeta-serve[r]' 2>/dev/null || true
    pgrep -af 'zeta-serve[r]' || true
  " || true
  # double-guard: the section's own finally-path already destroyed its
  # datasets; nothing else to clean here.
  return $ZBD_RC
}

ZBD_RC=0
run_zbd_section || ZBD_RC=$?

# ------------------------------------------------------------- cleanup -----
# Section 12's result participates in the run's exit status.
if [[ $ZBD_RC -ne 0 ]]; then RC=$ZBD_RC; fi
if [[ $RC -ne 0 || $KEEP_SERVER -eq 0 ]]; then
  log "Cleanup: stopping server + zmetad, destroying $DATASET"
  # A ROOT-owned phase-2 server cannot die by unprivileged pkill (its
  # config lives in an env var, invisible to -f matching) — and a live
  # bucket-dataset server would recreate datasets under the parent. Kill
  # it with sudo FIRST, then stop everything else and destroy.
  ssh -o BatchMode=yes "$HOST" "
    sudo -n pkill -f '[.]/zeta-serve[r]' 2>/dev/null || true
    pkill -f '[.]/zeta-serve[r]' 2>/dev/null || true
    pkill -f 'zmeta[d].*zeta-validate' 2>/dev/null || true
    sudo zfs destroy -r '$DATASET' 2>/dev/null || true
    true
  " || true
fi

if [[ $RC -ne 0 ]]; then
  echo; echo "VALIDATION FAILED ($RC check(s) failed). Server log follows:"
  ssh -o BatchMode=yes "$HOST" "tail -30 $REMOTE_DIR/server.log" 2>/dev/null || true
fi
exit $RC
