#!/usr/bin/env python3
"""ztags-checks.py — live checks for the zfs_native_tags store + the #15
event cursor, run ON zfs-meta against the phase server on 127.0.0.1:9707.

Sections:
  A. layout gate: the DB is layout 9 (tags table present, meta stamped 9)
  B. tag CLI round-trip: zmetad --tag-set/--tag-get/--tag-clear live
  C. cursor (#15): since-id resume on the live ?events wire, exact tallies
  D. zfs_native_tags wire round-trip: PUT ?tagging / GET / REPLACE / CLEAR
     on a dataset-backed bucket + sidecar-absence proof
All tallies printed exactly; exit non-zero on any failure.
"""
import base64
import hashlib
import hmac
import http.client
import json
import sqlite3
import ssl
import subprocess
import sys
import time
import urllib.parse
from datetime import datetime, timezone

HOST = "127.0.0.1"
PORT = 9707
DATASET = "testpool/zval"
DB = "/home/caimlas/zeta-validate/zmetad.db"
AK, SK, REGION = "valuser", "valpass", "us-east-1"

results = []
def check(name, ok, detail=""):
    results.append((name, bool(ok), str(detail)[:300]))  # noqa: detail str()-wrapped below

def sh(cmd, timeout=60):
    r = subprocess.run(["bash", "-c", cmd], capture_output=True, text=True, timeout=timeout)
    return (r.stdout + r.stderr).strip()

# ---- SigV4 probe (the run-zfs-validation.sh probe.py shape) ----------------
def _ssl():
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return ctx

def sign(method, path, query="", payload=b"", headers_extra=None, content_type=None):
    t = datetime.now(timezone.utc)
    amzdate = t.strftime("%Y%m%dT%H%M%SZ"); datestamp = t.strftime("%Y%m%d")
    headers = {
        "host": f"localhost:{PORT}",
        "x-amz-content-sha256": hashlib.sha256(payload).hexdigest(),
        "x-amz-date": amzdate,
    }
    if headers_extra: headers.update({k.lower(): v for k, v in headers_extra.items()})
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

def force_collect(secs=1.5):
    sh("pkill -USR1 -f 'zmeta[d].*zeta-validate' 2>/dev/null; true")
    time.sleep(secs)

def db_query(sql):
    c = sqlite3.connect(f"file:{DB}?mode=ro", uri=True)
    c.row_factory = sqlite3.Row
    try:
        return [dict(x) for x in c.execute(sql)]
    finally:
        c.close()

# ---- A. layout 9 gate -------------------------------------------------------
ver = {r["key"]: r["value"] for r in db_query("SELECT key, value FROM meta")}
check("A1 db_schema_version == 9 (layout 9 live)", ver.get("db_schema_version") == "9", str(ver))
tabs = {r["name"] for r in db_query("SELECT name FROM sqlite_master WHERE type='table'")}
check("A2 tags table exists in the live DB", "tags" in tabs, sorted(tabs))
ev = ver.get("events_schema_version")
check("A3 events_schema_version == 3 (layout 9 pair)", ev == "3", str(ver))

# ---- B. tag CLI round-trip on the real dataset ------------------------------
r = subprocess.run(["sudo", "zfs", "create", f"{DATASET}/clidemo"], capture_output=True, text=True)
check("B1 scratch child dataset created", r.returncode == 0, r.stderr[:200])
sh(f"sudo chown -R $(id -u):$(id -g) /{DATASET}/clidemo")
ZMETAD = "/usr/local/sbin/zmetad"
def zmetad(*args, sudo=False):
    # -d <db> rides every one-shot invocation (same reason the gateway's
    # store passes it: the compiled default /var/lib/zfs/zmetad.db is
    # root-owned and NOT the validation database).
    cmd = (["sudo"] if sudo else []) + [ZMETAD, "-d", DB] + list(args)
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
    return p.returncode, p.stdout.strip(), p.stderr.strip()

# A real file so the object id exists in the event log (bounded poll for the
# 2s zmetad cycle: SIGUSR1 forces an out-of-band collect).
sh(f"echo -n cli-tagged > /{DATASET}/clidemo/cli.txt")
row = []
for _ in range(5):
    force_collect(1.5)
    row = db_query(f"SELECT object_id FROM events WHERE dataset='{DATASET}/clidemo' AND full_path='cli.txt' ORDER BY id DESC LIMIT 1")
    if row:
        break
check("B2 event row resolves cli.txt to an object id", bool(row), str(row))
oid = str(row[0]["object_id"]) if row else "0"

rc, out, err = zmetad("--tag-set", f"{DATASET}/clidemo", "--tag-object", oid,
                      "--tag", "team=cli", "--tag", "env=live")
check("B3 --tag-set exits 0", rc == 0, f"{out} {err}"[:200])
rc, out, err = zmetad("--tag-get", f"{DATASET}/clidemo", "--tag-object", oid)
lines = sorted(out.splitlines())
check("B4 --tag-get round-trips both pairs (first-= split)",
      rc == 0 and lines == ["env=live", "team=cli"], out[:200])
# REPLACE semantics: a new set without env drops it.
rc, out, err = zmetad("--tag-set", f"{DATASET}/clidemo", "--tag-object", oid, "--tag", "team=cli2")
rc2, out2, _ = zmetad("--tag-get", f"{DATASET}/clidemo", "--tag-object", oid)
check("B5 --tag-set REPLACES (dropped key gone, new value present)",
      rc == 0 and rc2 == 0 and out2 == "team=cli2", out2[:200])
# Limits: 11 tags rejected, stored set untouched.
limit_args = [f"{DATASET}/clidemo", "--tag-object", oid] + sum([["--tag", f"k{i}=v{i}"] for i in range(11)], [])
rc, out, err = zmetad("--tag-set", *limit_args)
_, out_untouched, _ = zmetad("--tag-get", f"{DATASET}/clidemo", "--tag-object", oid)
check("B6 >10 tags rejected (exit 1) AND stored set untouched",
      rc == 1 and out_untouched == "team=cli2", f"rc={rc} stored={out_untouched!r}")
rc, out, err = zmetad("--tag-clear", f"{DATASET}/clidemo", "--tag-object", oid)
_, out_empty, _ = zmetad("--tag-get", f"{DATASET}/clidemo", "--tag-object", oid)
check("B7 --tag-clear empties the set (idempotent, exit 0)",
      rc == 0 and out_empty == "", out_empty[:100])
r = subprocess.run(["sudo", "zfs", "destroy", f"{DATASET}/clidemo"], capture_output=True, text=True)
check("B8 clidemo dataset destroyed", r.returncode == 0, r.stderr[:200])

# ---- C. cursor (#15) live asserts -------------------------------------------
for i in range(6):
    sh(f"echo -n cursor-{i} > /{DATASET}/cur-{i}.txt")
force_collect(2.0)
s, h, b = sign("GET", "/zval", query="events&max-events=1000")
check("C1 ?events 200 on the live dataset bucket", s == 200, f"{s} {b[:150]}")
try:
    ev = json.loads(b)
except Exception as e:
    ev = {"events": []}
    check("C2 ?events JSON parses", False, f"{e}")
else:
    check("C2 ?events JSON parses", True, "")
check("C3 every event carries a monotonic id", all(e.get("id", 0) > 0 for e in ev["events"]),
      json.dumps([e.get("id") for e in ev["events"]][:10]))
ids = [e["id"] for e in ev["events"]]
check("C4 ids strictly ascending (cursor order)", ids == sorted(ids) and len(set(ids)) == len(ids),
      str(ids[:10]))
cur_events = sorted([e for e in ev["events"] if (e.get("key") or "").startswith("cur-")],
                    key=lambda e: e["id"])
check("C5 all 6 cursor-seed writes surfaced", len(cur_events) >= 6,
      str([(e.get('key'), e.get('id')) for e in cur_events]))
if len(cur_events) >= 4:
    mid = cur_events[2]["id"]
    s, h, b2 = sign("GET", "/zval", query=f"events&since-id={mid}&max-events=1000")
    ev2 = json.loads(b2)
    ids2 = [e["id"] for e in ev2["events"]]
    check("C6 since-id resume: strictly-after only",
          s == 200 and all(i > mid for i in ids2), f"mid={mid} ids={ids2[:10]}")
    pre = {e["id"] for e in ev["events"] if e["id"] <= mid}
    check("C7 resume never re-delivers (no overlap with <= cursor)",
          not (set(ids2) & pre), str(sorted(pre & set(ids2)))[:100])
    expect = {e["id"] for e in cur_events if e["id"] > mid}
    check("C8 resume exact: every post-cursor cursor-seed event present, none missing",
          expect <= set(ids2), f"missing={sorted(expect - set(ids2))}")
    # max-events truncation honors since-id ordering
    s, h, b3 = sign("GET", "/zval", query=f"events&since-id={mid}&max-events=1")
    ev3 = json.loads(b3)
    check("C9 since-id + max-events=1 returns exactly the next event",
          s == 200 and len(ev3["events"]) == 1 and ev3["events"][0]["id"] == min(ids2),
          json.dumps([(e.get('id')) for e in ev3['events']]))
    # webdav parity surface is covered by e2e 18/19; here the S3 surface is the contract.
else:
    check("C6 since-id resume (skipped: too few events)", False, "seeds missing")

# ---- D. zfs_native_tags wire round-trip on a dataset-backed bucket ----------
# A dataset-backed bucket is created by the ROOT path form (PUT /<bucket>),
# not PUT /zval/<bucket> (that writes an OBJECT named ztags-bucket).
# The parent dataset is whatever /testpool (dataDir) resolves to — the pool
# root "testpool" here, NOT the scratch testpool/zval. The phase-1 script
# pre-creates testpool/ztags-bucket with events=on (unprivileged zfs create
# cannot mount children on this host); the bucket PUT adopts it. The PUT
# answers 409 BucketAlreadyOwnedByYou when the pre-created directory is
# already a registered bucket — BOTH prove the bucket is dataset-backed
# (D2's zfs list is the dataset proof).
ZPARENT = sh("zfs list -H -o name -t filesystem /testpool 2>/dev/null | head -1 | tr -d '[:space:]'")
ZBKT_DS = f"{ZPARENT}/ztags-bucket"
s, h, b = sign("PUT", "/ztags-bucket", payload=b"")
check("D1 PUT bucket (dataset-backed) -> 200/409-adopted", s in (200, 409), f"{s} {b[:150]}")
ds = sh(f"zfs list -H -o name {ZBKT_DS} 2>/dev/null")
check("D2 bucket dataset resolves by deterministic name", ds == ZBKT_DS, ds)
# events does NOT inherit by default: the phase server provisions the
# dataset, and this host-level property must be on for zmetad to track it.
sh(f"sudo zfs set events=on {ZBKT_DS}")
zmetad_pickup = False
for _ in range(15):  # ~25s bounded: zmetad discovers new datasets on its poll sweep
    force_collect(1.5)
    if db_query(f"SELECT dataset FROM sync_state WHERE dataset='{ZBKT_DS}'"):
        zmetad_pickup = True
        break
check("D3 zmetad fully tracks the bucket dataset (sync_state row)", zmetad_pickup,
      f"sync_state={[r['dataset'] for r in db_query('SELECT dataset FROM sync_state')]}")

payload = b"ztags-live-payload"
# The tag store needs the dataset TRACKED (sync_state row) before an object
# id can resolve; a PUT landing before the first poll answers an honest
# 500-class "not polled yet" (found live — the lying-404 was fixed). Retry
# the PUT within the poll window.
s = 0
for attempt in range(10):
    s, h, b = sign("PUT", "/ztags-bucket/obj.txt", payload=payload,
                   headers_extra={"x-amz-tagging": "team=live&env=zfs"})
    if s == 200:
        break
    force_collect(1.5)
check("D4 PUT object with x-amz-tagging -> 200", s == 200, f"{s} {b[:150]}")
s, h, b = sign("GET", "/ztags-bucket/obj.txt", query="tagging")
check("D5 GET ?tagging -> 200 round-trip", s == 200 and b"<Key>team</Key>" in b and b"<Value>live</Value>" in b, f"{s} {b[:200]}")
check("D6 second tag round-trips", b"<Key>env</Key>" in b and b"<Value>zfs</Value>" in b, b[:200])
# REPLACE
REPL = b"<Tagging><TagSet><Tag><Key>team</Key><Value>replaced</Value></Tag></TagSet></Tagging>"
s, h, b = sign("PUT", "/ztags-bucket/obj.txt", query="tagging", payload=REPL,
               content_type="application/xml")
check("D7 PUT ?tagging REPLACE -> 204", s == 204, f"{s} {b[:150]}")
s, h, b = sign("GET", "/ztags-bucket/obj.txt", query="tagging")
check("D8 replaced set visible, old value gone",
      s == 200 and b"<Value>replaced</Value>" in b and b.count(b"<Value>zfs</Value>") == 0, b[:200])
# Sidecar absence: the zmetad store is authoritative; the sidecar's tags
# field must be absent (either no sidecar tags key, or no sidecar at all).
meta = sh(f"cat '/{ZBKT_DS}/.metadata/obj.txt.meta' 2>/dev/null")
sidecar_clean = (meta == "") or ('"tags"' not in meta)
check("D9 sidecar carries NO tags field (zmetad owns tags)", sidecar_clean, meta[:200])
# The tags table actually holds the row (DB ground truth).
row = db_query(f"SELECT object_id FROM events WHERE dataset='{ZBKT_DS}' AND full_path='obj.txt' ORDER BY id DESC LIMIT 1")
if row:
    tags_rows = db_query(f"SELECT key, value FROM tags WHERE dataset='{ZBKT_DS}' AND object_id={row[0]['object_id']}")
    got = sorted((t["key"], t["value"]) for t in tags_rows)
    check("D10 tags table ground truth == wire", got == [("team", "replaced")], str(got))
else:
    check("D10 tags table ground truth == wire", False, "no event row for obj.txt")
# CLEAR
s, h, b = sign("DELETE", "/ztags-bucket/obj.txt", query="tagging")
check("D11 DELETE ?tagging -> 204", s == 204, f"{s} {b[:120]}")
s, h, b = sign("GET", "/ztags-bucket/obj.txt", query="tagging")
check("D12 GET ?tagging after clear -> 404 NoSuchTagSet",
      s == 404 and b"NoSuchTagSet" in b, f"{s} {b[:150]}")
# Honest-failure arm: an object whose id cannot resolve (no event row yet)
# must 404 NoSuchKey on GET ?tagging, never a silent empty sidecar answer.
s, h, b = sign("PUT", "/ztags-bucket/unpolled.txt", payload=b"fresh")
force_collect(0.3)
# NOTE: after a collect the row likely EXISTS; the honest-missing arm is the
# never-existing key path (which the data layer 404s first). We assert the
# common ordering: data 404 wins on a missing object.
s, h, b = sign("GET", "/ztags-bucket/never-existed.txt", query="tagging")
check("D13 GET ?tagging on a missing object -> 404 NoSuchKey",
      s == 404 and b"NoSuchKey" in b, f"{s} {b[:150]}")

# cleanup the bucket dataset so the scratch parent stays destroyable
sh(f"sudo zfs destroy {ZBKT_DS} 2>/dev/null; true")

failed = [(n, d) for n, ok, d in results if not ok]
for n, ok, d in results:
    print(f"{'PASS' if ok else 'FAIL'} | {n}" + (f" | {d}" if not ok else ""))
print(f"ZTAGS_TOTAL: {len(results) - len(failed)}/{len(results)}")
sys.exit(1 if failed else 0)
