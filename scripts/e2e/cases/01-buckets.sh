# 01-buckets.sh — create, head, list, delete empty, delete non-empty 409,
# location, invalid names 400, BucketAlreadyOwnedByYou 409.
set -u
BKT='e2e-01-buckets'

aws_ok 'create-bucket' s3api create-bucket --bucket "$BKT"
assert_status 'head-bucket exists → 200' 200 GET "/$BKT"
aws_cap LS_OUT s3 ls
assert_contains 'list-buckets shows bucket' "$LS_OUT" "$BKT"
assert_status 'get-bucket-location → 200' 200 GET "/$BKT?location"
assert_contains 'location XML present' "$S3_BODY" 'LocationConstraint'

# BucketAlreadyOwnedByYou → 409
assert_status 're-create same bucket → 409' 409 PUT "/$BKT"
assert_s3code 'error code BucketAlreadyOwnedByYou' 'BucketAlreadyOwnedByYou'

# Delete non-empty → 409 BucketNotEmpty
s3req PUT "/$BKT/blocker.txt" --data-binary 'data'
assert_eq 'setup: put blocker object' 200 "$S3_STATUS"
assert_status 'delete non-empty bucket → 409' 409 DELETE "/$BKT"
assert_s3code 'error code BucketNotEmpty' 'BucketNotEmpty'

# Invalid bucket names → 400 InvalidBucketName
assert_status 'invalid name uppercase → 400' 400 PUT '/Bad_Uppercase'
assert_s3code 'error code InvalidBucketName (uppercase)' 'InvalidBucketName'
assert_status 'invalid name too short → 400' 400 PUT '/ab'
assert_s3code 'error code InvalidBucketName (short)' 'InvalidBucketName'
assert_status 'invalid name traversal → 400' 400 PUT '/..escape'
assert_s3code 'error code InvalidBucketName (traversal)' 'InvalidBucketName'

# cleanup
assert_status 'delete blocker object → 204' 204 DELETE "/$BKT/blocker.txt"
assert_status 'delete bucket (empty now) → 204' 204 DELETE "/$BKT"
assert_status 'head after delete → 404' 404 GET "/$BKT"
