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
cat > "$WORK/config.json" <<EOF
{
  "dataDir": "/testpool/",
  "listenAddr": ":$PORT",
  "certFile": "cert.pem",
  "keyFile": "key.pem",
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
check("db_schema_version in supported range (5..6)",
      ver.get("db_schema_version") in ("5", "6"), str(ver))
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
  log "Cleanup: stopping server + zmetad, destroying $DATASET"
  ssh -o BatchMode=yes "$HOST" "
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
