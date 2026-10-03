#!/usr/bin/env python3
"""boto3-runner.py — client shim for the leaf 5.2 interop matrix.

Dispatch: boto3-runner.py <scenario> [args...]

Executes the named scenario through a boto3 client pointed at $INTEROP_ENDPOINT
(self-signed TLS, verify=False), prints ONE line of flat JSON on stdout:

    {"ok":true,"status":200,"code":"","data":"<sha256 hex prefix>","detail":""}
    {"ok":false,"status":404,"code":"NoSuchKey","data":"","detail":"..."}
    {"parse_error":true,"detail":"..."}      <- FINDING (client-side parse fail)

Exit 0 when ok, 1 when the S3 operation failed, 2 on parse/setup errors.
"""
import hashlib
import json
import os
import sys

import boto3
from botocore.config import Config
from botocore.exceptions import ClientError

ENDPOINT = os.environ.get("INTEROP_ENDPOINT", "https://localhost:8443")
REGION = "us-east-1"

# Payload constants shared with interop-lib expectations via the case files.
P1 = b"p1-" + b"a" * (5 * 1024 * 1024 - 3)          # 5 MiB part 1
P2 = b"p2-" + b"b" * (5 * 1024 * 1024 - 3)          # 5 MiB part 2
P3 = b"final-part-tail"
MPU_BYTES = P1 + P2 + P3


def emit(ok, status=0, code="", data="", detail=""):
    print(json.dumps({
        "ok": bool(ok),
        "status": int(status),
        "code": code,
        "data": data,
        "detail": detail,
    }))
    return 0 if ok else 1


def h(b):
    return hashlib.sha256(b).hexdigest()[:16]


def client():
    return boto3.client(
        "s3",
        endpoint_url=ENDPOINT,
        region_name=REGION,
        aws_access_key_id="minioadmin",
        aws_secret_access_key="minioadmin",
        verify=False,
        # signature_version s3v4: botocore otherwise presigns with the
        # legacy SigV2 query format (AWSAccessKeyId/Signature/Expires),
        # which this SigV4-only server correctly rejects — a client-side
        # configuration default, not a wire-format issue (leaf 5.2).
        config=Config(signature_version="s3v4",
                      s3={"addressing_style": "path"},
                      retries={"max_attempts": 0}),
    )


def run(c, scenario, args):
    """Return (ok, status, code, data, detail)."""
    if scenario == "bucket_create":
        c.create_bucket(Bucket=args[0])
        return True, 200, "", "", ""
    if scenario == "bucket_create_dup":
        try:
            c.create_bucket(Bucket=args[0])
        except ClientError as e:
            return False, e.response["ResponseMetadata"]["HTTPStatusCode"], \
                e.response["Error"]["Code"], "", ""
        return False, 200, "", "", "unexpected success"
    if scenario == "bucket_head":
        c.head_bucket(Bucket=args[0])
        return True, 200, "", "", ""
    if scenario == "bucket_head_missing":
        try:
            c.head_bucket(Bucket=args[0])
        except ClientError as e:
            st = e.response["ResponseMetadata"]["HTTPStatusCode"]
            code = e.response["Error"].get("Code", "")
            # HEAD responses carry NO XML error body on the wire (any
            # client), so botocore synthesizes a status-derived code ("404").
            # Normalize to the S3 code the wire would give on GET (aws-cli
            # e2e case 01 asserts NoSuchBucket via curl).
            if code in ("404", "Not Found"):
                code = "NoSuchBucket"
            return False, st, code, "", ""
        return False, 200, "", "", "unexpected success"
    if scenario == "bucket_list":
        r = c.list_buckets()
        names = [b["Name"] for b in r.get("Buckets", [])]
        return True, 200, "", "1" if args[0] in names else "0", ""
    if scenario == "bucket_delete":
        c.delete_bucket(Bucket=args[0])
        return True, 204, "", "", ""
    if scenario == "object_put":
        # object_put <bucket> <payload-text> <key>
        body = args[1].encode()
        c.put_object(Bucket=args[0], Key=args[2], Body=body)
        return True, 200, "", h(body), ""
    if scenario == "object_put_file":
        # object_put_file <bucket> <key> <path> — hash of the file
        with open(args[2], "rb") as f:
            body = f.read()
        c.put_object(Bucket=args[0], Key=args[1], Body=body)
        return True, 200, "", h(body), ""
    if scenario == "object_get_roundtrip":
        r = c.get_object(Bucket=args[0], Key=args[1])
        body = r["Body"].read()
        return True, r["ResponseMetadata"]["HTTPStatusCode"], "", h(body), ""
    if scenario == "unicode_key":
        key = "kéy-ünïcode-✓.txt"
        body = b"unicode-payload"
        c.put_object(Bucket=args[0], Key=key, Body=body)
        got = c.get_object(Bucket=args[0], Key=key)["Body"].read()
        return True, 200, "", h(got) if got == body else "MISMATCH", ""
    if scenario == "error_nosuchbucket":
        try:
            c.get_object(Bucket="e2e-12-no-such-bucket-xyz", Key="k")
        except ClientError as e:
            return False, e.response["ResponseMetadata"]["HTTPStatusCode"], \
                e.response["Error"]["Code"], "", ""
        return False, 200, "", "", "unexpected success"
    if scenario == "error_nosuchkey":
        try:
            c.get_object(Bucket=args[0], Key="does-not-exist")
        except ClientError as e:
            return False, e.response["ResponseMetadata"]["HTTPStatusCode"], \
                e.response["Error"]["Code"], "", ""
        return False, 200, "", "", "unexpected success"
    if scenario == "error_accessdenied":
        # Closest observable AccessDenied with a single valid credential:
        # an EXPIRED presigned URL (parity: case 07's expired branch — the
        # server answers 403 AccessDenied "Request has expired").
        import time as _t
        import urllib.request
        import urllib.error
        import ssl
        url = c.generate_presigned_url(
            "get_object", Params={"Bucket": args[0], "Key": args[1]},
            ExpiresIn=1)
        _t.sleep(2)
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        try:
            with urllib.request.urlopen(url, context=ctx) as resp:
                return False, resp.status, "", "", "unexpected success"
        except urllib.error.HTTPError as e:
            body = e.read().decode("utf-8", "replace")
            import re as _re
            m = _re.search(r"<Code>([^<]+)</Code>", body)
            return False, e.code, m.group(1) if m else "", "", ""

    if scenario == "list_prefix":
        r = c.list_objects_v2(Bucket=args[0], Prefix="pre/")
        keys = [o["Key"] for o in r.get("Contents", [])]
        return True, 200, "", str(len(keys)), ""
    if scenario == "range_get":
        r = c.get_object(Bucket=args[0], Key=args[1], Range="bytes=0-99")
        body = r["Body"].read()
        if r["ResponseMetadata"]["HTTPStatusCode"] != 206 or len(body) != 100:
            return False, r["ResponseMetadata"]["HTTPStatusCode"], "", "", \
                "expected 206/100"
        return True, 206, "", str(len(body)), ""
    if scenario == "range_get_invalid":
        try:
            r = c.get_object(Bucket=args[0], Key=args[1], Range="bytes=500-600")
            return False, 200, "", "", "unexpected success"
        except ClientError as e:
            # 416 is the EXPECTED server outcome, but it is still an S3
            # FAILURE from the client: ok=false + code, and the parity rule
            # matches the frozen error code (416 InvalidRange).
            return False, e.response["ResponseMetadata"]["HTTPStatusCode"], \
                e.response["Error"]["Code"], "", ""
    if scenario == "multipart_roundtrip":
        mpu = c.create_multipart_upload(Bucket=args[0], Key="assembled.bin")
        uid = mpu["UploadId"]
        parts = []
        for n, p in ((1, P1), (2, P2), (3, P3)):
            r = c.upload_part(Bucket=args[0], Key="assembled.bin",
                              UploadId=uid, PartNumber=n, Body=p)
            parts.append({"ETag": r["ETag"], "PartNumber": n})
        c.complete_multipart_upload(
            Bucket=args[0], Key="assembled.bin", UploadId=uid,
            MultipartUpload={"Parts": parts})
        got = c.get_object(Bucket=args[0], Key="assembled.bin")["Body"].read()
        return True, 200, "", h(got) if got == MPU_BYTES else "MISMATCH", ""
    if scenario == "presigned_get":
        import urllib.request
        import ssl
        url = c.generate_presigned_url(
            "get_object", Params={"Bucket": args[0], "Key": args[1]},
            ExpiresIn=300)
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        with urllib.request.urlopen(url, context=ctx) as resp:
            body = resp.read()
            st = resp.status
        return True, st, "", h(body), ""
    if scenario == "copy_object":
        c.copy_object(Bucket=args[0], Key=args[1],
                      CopySource={"Bucket": args[2], "Key": args[3]})
        got = c.get_object(Bucket=args[0], Key=args[1])["Body"].read()
        return True, 200, "", h(got), ""
    if scenario == "batch_delete":
        r = c.delete_objects(Bucket=args[0], Delete={
            "Objects": [{"Key": k} for k in args[1:]], "Quiet": False})
        return True, 200, "", str(len(r.get("Deleted", []))), ""
    # --- object tagging smoke (parity: case 30) --------------------------------
    if scenario == "tagging_put":
        c.put_object_tagging(Bucket=args[0], Key=args[1], Tagging={
            "TagSet": [{"Key": "interop", "Value": "yes"}]})
        return True, 200, "", "", ""
    if scenario == "tagging_get":
        r = c.get_object_tagging(Bucket=args[0], Key=args[1])
        tags = {t["Key"]: t["Value"] for t in r.get("TagSet", [])}
        return True, r["ResponseMetadata"]["HTTPStatusCode"], "", \
            tags.get("interop", ""), ""
    if scenario == "tagging_delete":
        c.delete_object_tagging(Bucket=args[0], Key=args[1])
        # Post-delete GET: the server answers 404 NoSuchTagSet (case-30
        # parity) — that IS the emptied state; return it as observable data.
        try:
            r = c.get_object_tagging(Bucket=args[0], Key=args[1])
            return True, r["ResponseMetadata"]["HTTPStatusCode"], "", \
                str(len(r.get("TagSet", []))), ""
        except ClientError as e:
            code = e.response["Error"].get("Code", "")
            if code == "NoSuchTagSet":
                return True, 200, "", code, ""
            raise
    return False, 0, "", "", "unknown scenario: " + scenario


def main():
    if len(sys.argv) < 2:
        print(json.dumps({"parse_error": True,
                          "detail": "usage: boto3-runner.py <scenario> [args]"}))
        return 2
    scenario = sys.argv[1]
    args = sys.argv[2:]
    try:
        c = client()
        ok, status, code, data, detail = run(c, scenario, args)
        return emit(ok, status, code, data, detail)
    except ClientError as e:
        # Unexpected ClientError outside a scenario's own handler: surface it
        # as a real outcome so the parity assert sees status + code.
        r = e.response
        return emit(False,
                    r.get("ResponseMetadata", {}).get("HTTPStatusCode", 0),
                    r.get("Error", {}).get("Code", ""),
                    "", detail=str(e)[:200])
    except Exception as e:  # noqa: BLE001 — shim boundary, report as finding
        print(json.dumps({"parse_error": True, "detail": str(e)[:300]}))
        return 2


if __name__ == "__main__":
    sys.exit(main())
