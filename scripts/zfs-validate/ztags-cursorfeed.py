#!/usr/bin/env python3
"""ztags-cursorfeed.py — live verification of the zeta-cache CursorFeed
(leaf 05) against the REAL gateway ?events endpoint on zfs-meta:9707.

Drives the module's OWN transport.Client + sync.NewCursorFeed (the
zeta-cache/cmd/zcfharness driver, cross-built and deployed) against the
live server: two Delta passes with real writes between, and the
plain-bucket 503 -> fullScan contract.
"""
import re
import subprocess
import sys

results = []
def check(name, ok, detail=""):
    results.append((name, bool(ok), str(detail)[:300]))

def sh(cmd, timeout=180):
    r = subprocess.run(["bash", "-c", cmd], capture_output=True, text=True, timeout=timeout)
    return (r.stdout + r.stderr).strip()

def run(mode, bucket):
    # The zeta-cache transport rides the WEBDAV frontend (Basic auth) —
    # the client protocol per the client-cache plan tree. The webdav
    # frontend is bucket-pinned (mode B): the URL path is the resource,
    # not /<bucket>/.
    return sh("/home/caimlas/zeta-validate/zcf https://127.0.0.1:9713 "
              "valuser valpass " + bucket + " " + mode + " /tmp/zcf-tmp")

sh("mkdir -p /tmp/zcf-tmp /testpool/plainbkt")
sh("echo -n x > /testpool/plainbkt/f.txt")

# --- 1. live ZFS bucket: probe + two-phase delta with real writes ------------
# First-contact semantics (leaf 05, cursor.go): the FIRST Delta establishes
# the cursor at the newest id and answers fullScan=true (the index was just
# reconciled; feeding the full stream would re-diff the world). The SECOND
# Delta — with a new write in between — must run the CURSOR path
# (fullScan=false) and surface the new write's path.
sh("rm -f /tmp/zcf-tmp/idx-zval.db")
sh("echo -n cfa > /testpool/zval/cf-a.txt")
sh("pkill -USR1 -f 'zmeta[d]' 2>/dev/null; sleep 2")
out1 = run("delta", "zval")
check("CF1 first Delta (cursor establishment) answers fullScan=true",
      "DELTA:" in out1 and "fullScan=true" in out1, out1[:250])
sh("echo -n cfb > /testpool/zval/cf-b.txt")
sh("pkill -USR1 -f 'zmeta[d]' 2>/dev/null; sleep 2")
out2 = run("delta", "zval")
check("CF2 second Delta answers after new writes",
      "DELTA:" in out2, out2[:250])
m1 = re.search(r"fullScan=(\w+)", out1) if "DELTA:" in out1 else None
m2 = re.search(r"fullScan=(\w+)", out2) if "DELTA:" in out2 else None
check("CF3 Delta reports a parseable fullScan flag both runs",
      bool(m1) and bool(m2), f"{out1[:80]} | {out2[:80]}")
check("CF4 both runs completed with no feed error (never DELTA_ERR)",
      "DELTA_ERR" not in out1 and "DELTA_ERR" not in out2,
      f"{out1[:120]} | {out2[:120]}")
check("CF5 second Delta ran the CURSOR path (fullScan=false)",
      bool(m2) and m2.group(1) == "false", out2[:250])
check("CF6 second Delta surfaced the new write's path (cf-b.txt)",
      "cf-b.txt" in out2, out2[:250])

# --- 2. plain-dir bucket: 503 -> scan-only contract --------------------------
out3 = run("delta", "plainbkt")
check("CF7 plain-dir bucket Delta degrades to fullScan (503 contract)",
      "DELTA:" in out3 and "fullScan=true" in out3, out3[:250])

# --- 3. probe freshness over the live wire -----------------------------------
probe = run("probe", "zval")
check("CF8 ProbeFresh over the live wire answers", "PROBE_FRESH:" in probe, probe[:150])

for n, ok, d in results:
    print(f"{'PASS' if ok else 'FAIL'} | {n}" + (f" | {d}" if not ok else ""))
print(f"CF_TOTAL: {sum(1 for _, ok, _ in results if ok)}/{len(results)}")
sys.exit(1 if any(not ok for _, ok, _ in results) else 0)
