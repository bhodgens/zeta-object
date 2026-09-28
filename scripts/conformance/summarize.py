#!/usr/bin/env python3
"""summarize.py — group-level pass/fail summary + baseline ratchet (leaf 5.1).

Parses a pytest -v log from the ceph/s3-tests run, buckets tests into
functional groups, prints a PASS/FAIL table, and compares against
scripts/conformance/baseline.txt.

Ratchet rule: exit 1 ONLY when a test that PASSED in the baseline now FAILS
or ERRORS (a regression). New failures vs the baseline that were already
failing are fine; newly passing tests are reported as improvements.

Usage: summarize.py <pytest.log> <baseline.txt>
"""
import re
import sys
from collections import defaultdict

# test id -> (file, group)
RESULT_RE = re.compile(r"^(?:[\w./-]*?)(s3tests/functional/\w+\.py)::(\S+)\s+(PASSED|FAILED|ERROR|SKIPPED|XFAIL|XPASS)")

GROUPS = [
    ("bucket lifecycle & create/delete", r"^test_bucket_(create|exists|recreate|delete|get_?\.?|not_exist|list.*)$|^(test_buckets|test_bucket_)(create|delete|exists)"),
    ("bucket listing", r"^(test_bucket_list|test_bucket_listv2|test_list_buckets)"),
    ("object CRUD (put/get/head/delete)", r"^(test_object_(put|get|head|delete|write|read|post)|test_basic_?key|test_get_object|test_set_key|test_post_object.*(utf|set))"),
    ("multipart upload", r"multipart|abort|moto|upload_?part|100_?continue"),
    ("copy", r"copy"),
    ("range/conditional GET", r"^test_(ranged|get_range|object_get_(if|range)|atomic)"),
    ("presigned URLs", r"presign|x_amz_expires"),
    ("headers & auth details", r"^(test_object_.*(_raw|header)|test_rgw_?#?|test_get_?object.*(auth|anon)|test_bucket_.*raw|test_access|test_no_?.*(auth|key)|test_secret|test_bad|test_empty|test_authorization|test_?auth)"),
    ("misc object behavior", r"^(test_object_.*)"),
]


def group_of(name: str) -> str:
    for label, pat in GROUPS:
        if re.search(pat, name):
            return label
    return "misc"


def main() -> int:
    log, baseline_path = sys.argv[1], sys.argv[2]
    results = {}  # nodeid -> status
    for line in open(log, encoding="utf-8", errors="replace"):
        m = RESULT_RE.match(line.strip())
        if m:
            nodeid, test, status = m.groups()
            results[f"{nodeid}::{test}"] = status

    by_group = defaultdict(lambda: defaultdict(list))
    for nodeid, status in results.items():
        test = nodeid.split("::")[-1]
        by_group[group_of(test)][status].append(test)

    # baseline: nodeid -> PASSED
    baseline_passed = set()
    try:
        for line in open(baseline_path, encoding="utf-8"):
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            nodeid, status = line.rsplit("=", 1)
            if status == "PASSED":
                baseline_passed.add(nodeid)
    except FileNotFoundError:
        print(f"note: no baseline at {baseline_path}; ratchet skipped (first run)")

    print()
    print(f"{'group':38s} {'pass':>5s} {'fail':>5s} {'err':>5s} {'skip':>5s}")
    total = defaultdict(int)
    for label in sorted(by_group):
        g = by_group[label]
        p, f, e, s = len(g.get("PASSED", [])), len(g.get("FAILED", [])), len(g.get("ERROR", [])), len(g.get("SKIPPED", []))
        for k, n in (("PASSED", p), ("FAILED", f), ("ERROR", e), ("SKIPPED", s)):
            total[k] += n
        print(f"{label:38s} {p:5d} {f:5d} {e:5d} {s:5d}")
    tp, tf, te, ts = total["PASSED"], total["FAILED"], total["ERROR"], total["SKIPPED"]
    print("-" * 60)
    print(f"{'TOTAL':38s} {tp:5d} {tf:5d} {te:5d} {ts:5d}")

    # ratchet
    regressions = []
    for nodeid, status in results.items():
        if nodeid in baseline_passed and status in ("FAILED", "ERROR"):
            regressions.append(nodeid)
    if regressions:
        print(f"\nREGRESSIONS vs baseline ({len(regressions)}):")
        for r in sorted(regressions):
            print("  " + r)
        return 1
    newly = [n for n in results if n not in baseline_passed and results[n] == "PASSED"]
    if baseline_passed and newly:
        print(f"\nimprovements vs baseline ({len(newly)} newly passing)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
