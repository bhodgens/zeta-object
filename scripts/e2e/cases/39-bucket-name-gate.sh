# 39-bucket-name-gate.sh — the bucket-name gate on the s3 wire (bughunt
# 2026-10-05 C1). A traversal-shaped BUCKET name reached getBucketPath, whose
# filepath.Join(dataDir, name) CLEANS "..", so ".." resolved to the data
# root's PARENT and the request read, wrote, listed and deleted OUTSIDE the
# data root. http.ServeMux cleans a literal /.. into a 307 redirect, but
# percent-encoded forms (/%2e%2e) survive to the handler, which is why every
# traversal path below is percent-encoded.
#
# The unit pin lives in internal/frontend/s3/bucket_name_gate_test.go (it
# asserts the FILESYSTEM after each request, not just statuses); this case
# pins the same refusal on the real wire, through curl + SigV4, on every
# surface that resolves a bucket path. A POSITIVE control per surface keeps
# the case honest: it cannot pass by refusing everything.
#
# Surfaces covered: POST ?delete (the C1 deletion arm), object PUT/GET/HEAD/
# DELETE, GET ?list-type=2 / ?versions / ?uploads, the already-gated ?batch
# (pinned so it STAYS gated), and the refusals' error class.
set -u
BKT='e2e-39-name-gate'
aws s3api create-bucket --bucket "$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl >/dev/null 2>&1

TMP=$(mktemp /tmp/e2e39.XXXXXX)
printf 'canary-39\n' > "$TMP"

# Positive control first: the same operations on a real bucket must work, so
# the traversal refusals below are the gate and not a broken server.
s3req PUT "/$BKT/inside.txt" --data-binary "@$TMP"
assert_eq 'positive control: PUT to a real bucket -> 200' 200 "$S3_STATUS"
s3req GET "/$BKT/inside.txt"
assert_eq 'positive control: GET from a real bucket -> 200' 200 "$S3_STATUS"
s3req GET "/$BKT?list-type=2"
assert_eq 'positive control: ?list-type=2 on a real bucket -> 200' 200 "$S3_STATUS"
s3req GET "/$BKT?versions"
assert_eq 'positive control: ?versions on a real bucket -> 200' 200 "$S3_STATUS"
s3req GET "/$BKT?uploads"
assert_eq 'positive control: ?uploads on a real bucket -> 200' 200 "$S3_STATUS"
s3req POST "/$BKT?delete" -H 'Content-Type: application/xml' \
	--data-binary '<Delete><Object><Key>inside.txt</Key></Object></Delete>'
assert_eq 'positive control: ?delete on a real bucket -> 200' 200 "$S3_STATUS"
s3req GET "/$BKT/inside.txt"
assert_eq 'positive control: ?delete really deleted the key -> 404' 404 "$S3_STATUS"

# Restore the canary for the batch positive control.
s3req PUT "/$BKT/inside.txt" --data-binary "@$TMP"
assert_eq 'positive control: re-seed -> 200' 200 "$S3_STATUS"
s3req POST "/$BKT?batch" -H 'Content-Type: application/json' \
	--data-binary '{"operations":[{"op":"delete","from":"absent-39.txt"}]}'
assert_eq 'positive control: ?batch on a real bucket -> 200' 200 "$S3_STATUS"

# --- the traversal refusals -------------------------------------------------
# NOTE on curl and paths: curl does NOT normalize the path before sending, so
# /%2e%2e reaches the server percent-encoded — exactly the form ServeMux lets
# through. (A literal /.. would be cleaned by the mux into a 307 redirect
# before any handler ran, which is NOT the hole; do not weaken these to
# literal dots.)

# ?delete (the arm that DELETED files outside the data root before the fix).
s3req POST "/%2e%2e?delete" -H 'Content-Type: application/xml' \
	--data-binary '<Delete><Object><Key>victim-39.txt</Key></Object></Delete>'
assert_eq 'traversal bucket on ?delete is refused (not 200)' 404 "$S3_STATUS"
assert_contains 'traversal ?delete error code NoSuchBucket' "$(s3_code)" 'NoSuchBucket'

# Object-level surface (the dispatch switch now gates before every handler).
s3req PUT "/%2e%2e/planted-39.txt" --data-binary 'planted by a traversal name'
assert_eq 'traversal bucket object PUT is refused' 404 "$S3_STATUS"
s3req GET "/%2e%2e/victim-39.txt"
assert_eq 'traversal bucket object GET is refused' 404 "$S3_STATUS"
s3req HEAD "/%2e%2e/victim-39.txt"
assert_eq 'traversal bucket object HEAD is refused' 404 "$S3_STATUS"
s3req DELETE "/%2e%2e/inside.txt"
assert_eq 'traversal bucket object DELETE is refused' 404 "$S3_STATUS"

# Listing surfaces.
s3req GET "/%2e%2e?list-type=2"
assert_eq 'traversal bucket ?list-type=2 is refused' 404 "$S3_STATUS"
s3req GET "/%2e%2e?versions"
assert_eq 'traversal bucket ?versions is refused' 404 "$S3_STATUS"
s3req GET "/%2e%2e?uploads"
assert_eq 'traversal bucket ?uploads is refused' 404 "$S3_STATUS"

# The already-gated batch surface: pinned so it STAYS gated (7a8ad1b).
s3req POST "/%2e%2e?batch" -H 'Content-Type: application/json' \
	--data-binary '{"operations":[{"op":"delete","from":"victim-39.txt"}]}'
assert_eq 'traversal bucket ?batch stays gated -> 404' 404 "$S3_STATUS"

# The single-dot form resolves to the data root itself — same refusal.
s3req PUT "/%2e/planted-39.txt" --data-binary 'single dot'
assert_eq 'traversal bucket "." object PUT is refused' 404 "$S3_STATUS"

# Upper-case percent-encoding reaches the handler identically.
s3req GET "/%2E%2E?list-type=2"
assert_eq 'traversal bucket %2E%2E (upper) ?list-type=2 is refused' 404 "$S3_STATUS"

# An invalid-but-not-traversal name (too short) is refused the same way.
s3req GET "/ab?list-type=2"
assert_eq 'short bucket name on ?list-type=2 is refused' 404 "$S3_STATUS"

rm -f "$TMP"
aws s3 rb "s3://$BKT" --endpoint-url "$ENDPOINT" --no-verify-ssl --force >/dev/null 2>&1
