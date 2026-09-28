#!/bin/bash
# =============================================================================
# Mini-S3 Full Integration Test Suite
# =============================================================================
# Starts the mini-s3 server with a temporary test directory, runs a
# comprehensive set of S3 operations via AWS CLI, and stops the server.
#
# Usage: ./scripts/test-s3-full.sh
#
# Prerequisites:
#   - AWS CLI (v2) installed and in PATH
#   - Go toolchain (for building the server)
#   - openssl (for generating test certs)
#
# Environment variables:
#   PORT           - Server port (default: 8443)
#   SKIP_SERVER    - Set to 1 to use an already-running server
#   EXTENDED_TESTS - Set to 1 to run multipart and large file tests
#   VERBOSE        - Set to 1 for detailed output
# =============================================================================

set -euo pipefail

# ---- Configuration ----
PORT="${PORT:-8443}"
ENDPOINT="https://localhost:${PORT}"
AWS_ACCESS_KEY_ID="${MINIS3_ACCESS_KEY:-minioadmin}"
AWS_SECRET_ACCESS_KEY="${MINIS3_SECRET_KEY:-minioadmin}"
REGION="us-east-1"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
TEST_DIR="$(mktemp -d /tmp/minis3-full-test-XXXXXX)"
CONFIG_FILE="$TEST_DIR/config.json"
CERTS_DIR="$TEST_DIR/certs"
BUCKET="full-test-bucket"
TEST_FILES_DIR="$TEST_DIR/test-files"
TOTAL_TESTS=0
PASSED_TESTS=0
FAILED_TESTS=0
SKIPPED_TESTS=0
SERVER_PID=""

# Also honor MINIS3_CERT_FILE / MINIS3_KEY_FILE if the operator relocated certs
CERT_FILE="${MINIS3_CERT_FILE:-certs/cert.pem}"
KEY_FILE="${MINIS3_KEY_FILE:-certs/key.pem}"

export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY

# Suppress Python SSL warnings from AWS CLI
export PYTHONWARNINGS=ignore

# ---- Color Output ----
if [[ -t 1 ]]; then
    RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'
    BOLD='\033[1m'; NC='\033[0m'
else
    RED=''; GREEN=''; YELLOW=''; CYAN=''; BOLD=''; NC=''
fi

# ---- Helper Functions ----
info() { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; }
header() { echo -e "\n${BOLD}${CYAN}========================================${NC}"; echo -e "${BOLD}${CYAN}  $*${NC}"; echo -e "${BOLD}${CYAN}========================================${NC}\n"; }

run_s3() {
    if [[ "${VERBOSE:-0}" == "1" ]]; then
        aws s3 "$@" --endpoint-url "$ENDPOINT" --no-verify-ssl --region "$REGION"
    else
        aws s3 "$@" --endpoint-url "$ENDPOINT" --no-verify-ssl --region "$REGION" 2>&1
    fi
}

run_s3api() {
    if [[ "${VERBOSE:-0}" == "1" ]]; then
        aws s3api "$@" --endpoint-url "$ENDPOINT" --no-verify-ssl --region "$REGION"
    else
        aws s3api "$@" --endpoint-url "$ENDPOINT" --no-verify-ssl --region "$REGION" 2>&1
    fi
}

pass() {
    TOTAL_TESTS=$((TOTAL_TESTS + 1))
    PASSED_TESTS=$((PASSED_TESTS + 1))
    echo -e "  ${GREEN}PASS${NC} $1"
}

skip() {
    SKIPPED_TESTS=$((SKIPPED_TESTS + 1))
    echo -e "  ${YELLOW}SKIP${NC} $1 — $2"
}

fail() {
    TOTAL_TESTS=$((TOTAL_TESTS + 1))
    FAILED_TESTS=$((FAILED_TESTS + 1))
    echo -e "  ${RED}FAIL${NC} $1 — $2"
}

run_test() {
    local desc="$1"; shift
    TOTAL_TESTS=$((TOTAL_TESTS + 1))
    echo -n "  ${YELLOW}TEST${NC} $desc ... "
    local output
    if ! output=$("$@" 2>&1); then
        echo -e "${RED}FAIL${NC}"
        echo "    $output"
        FAILED_TESTS=$((FAILED_TESTS + 1))
    else
        echo -e "${GREEN}PASS${NC}"
        PASSED_TESTS=$((PASSED_TESTS + 1))
    fi
}

# expects exit code 0 and output to contain the substring
assert_contains() {
    local desc="$1" cmd="$2" expect="$3"
    TOTAL_TESTS=$((TOTAL_TESTS + 1))
    echo -n "  ${YELLOW}TEST${NC} $desc ... "
    local output
    if ! output=$(eval "$cmd" 2>&1); then
        echo -e "${RED}FAIL${NC} (command failed)"
        echo "    $output"
        FAILED_TESTS=$((FAILED_TESTS + 1))
    elif echo "$output" | grep -qF "$expect"; then
        echo -e "${GREEN}PASS${NC}"
        PASSED_TESTS=$((PASSED_TESTS + 1))
    else
        echo -e "${RED}FAIL${NC} (missing: '$expect')"
        echo "    $output"
        FAILED_TESTS=$((FAILED_TESTS + 1))
    fi
}

# expects exit code != 0
assert_fails() {
    local desc="$1" cmd="$2"
    TOTAL_TESTS=$((TOTAL_TESTS + 1))
    echo -n "  ${YELLOW}TEST${NC} $desc ... "
    if eval "$cmd" > /dev/null 2>&1; then
        echo -e "${RED}FAIL${NC} (expected failure)"
        FAILED_TESTS=$((FAILED_TESTS + 1))
    else
        echo -e "${GREEN}PASS${NC}"
        PASSED_TESTS=$((PASSED_TESTS + 1))
    fi
}

cleanup() {
    echo ""
    info "Cleaning up..."

    # Stop server if we started it
    if [[ -n "${SERVER_PID:-}" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
        info "Stopping mini-s3 server (PID $SERVER_PID)..."
        kill "$SERVER_PID" 2>/dev/null || true
        wait "$SERVER_PID" 2>/dev/null || true
    fi

    # Remove test directory
    if [[ -d "$TEST_DIR" ]]; then
        rm -rf "$TEST_DIR"
    fi
}
trap cleanup EXIT

# ---- Setup ----
setup() {
    header "Mini-S3 Full Integration Test Suite"
    info "Test directory: $TEST_DIR"
    info "Endpoint: $ENDPOINT"

    # Create test directory structure
    mkdir -p "$TEST_DIR/data" "$CERTS_DIR" "$TEST_FILES_DIR"

    # Generate self-signed certs
    if ! openssl req -x509 -newkey rsa:2048 -nodes \
        -out "$CERTS_DIR/cert.pem" -keyout "$CERTS_DIR/key.pem" \
        -days 1 -subj "/CN=localhost" -addext "subjectAltName=DNS:localhost" \
        > /dev/null 2>&1; then
        error "Failed to generate certificates"
        exit 1
    fi
    info "Generated test certificates"

    # Create config.json
    cat > "$CONFIG_FILE" <<EOF
{
  "dataDir": "$TEST_DIR/data/"
}
EOF
    info "Created config.json"

    # Create test files of various types
    echo "Hello, Mini-S3! This is a text file for integration testing." > "$TEST_FILES_DIR/hello.txt"
    dd if=/dev/urandom of="$TEST_FILES_DIR/random.bin" bs=1024 count=32 2>/dev/null
    echo '{"key": "value", "numbers": [1,2,3,4,5]}' > "$TEST_FILES_DIR/data.json"
    # Create a file with special characters in name
    echo "special chars content" > "$TEST_FILES_DIR/special-chars.txt"

    # Pre-create bucket directory so .bucket-actions can be placed there
    mkdir -p "$TEST_DIR/data/$BUCKET/.metadata"

    # Create bucket-actions config in the bucket directory for testing action hooks
    cat > "$TEST_DIR/data/$BUCKET/.bucket-actions" <<'EBUCKET'
{
  "version": "1.0",
  "after_upload": [
    {
      "name": "test-upload-log",
      "description": "Logs upload events for testing",
      "patterns": ["*"],
      "command": "echo \"UPLOAD:$OBJECT_KEY:$SIZE\" >> %TEST_DIR%/action-log.txt",
      "async": true
    }
  ],
  "after_download": [
    {
      "name": "test-download-log",
      "patterns": ["*"],
      "command": "echo \"DOWNLOAD:$OBJECT_KEY\" >> %TEST_DIR%/action-log.txt",
      "async": true
    }
  ],
  "after_delete": [
    {
      "name": "test-delete-log",
      "patterns": ["*"],
      "command": "echo \"DELETE:$OBJECT_KEY\" >> %TEST_DIR%/action-log.txt",
      "async": true
    }
  ]
}
EBUCKET
    # Replace placeholder with actual path
    sed -i.bak "s|%TEST_DIR%|$TEST_DIR|g" "$TEST_DIR/data/$BUCKET/.bucket-actions"
    rm -f "$TEST_DIR/data/$BUCKET/.bucket-actions.bak"

    # Build or check server
    if [[ "${SKIP_SERVER:-0}" != "1" ]]; then
        info "Building mini-s3 server..."
        (cd "$PROJECT_DIR" && go build -o "$TEST_DIR/mini-s3-server" .) || {
            error "Build failed"
            exit 1
        }

        # Symlink certs so the server can find them at ./certs/
        ln -sf "$CERTS_DIR" "$TEST_DIR/certs-link"
        # Create a symlink so server can use local certs
        (cd "$TEST_DIR" && ln -sf certs-link certs)

        # Start server — MINIS3_LISTEN_ADDR wires the PORT env var into the
        # server's bind address (config key: listenAddr)
        info "Starting mini-s3 server on port $PORT..."
        MINIS3_CONFIG="$CONFIG_FILE" \
        MINIS3_ACCESS_KEY="$AWS_ACCESS_KEY_ID" \
        MINIS3_SECRET_KEY="$AWS_SECRET_ACCESS_KEY" \
        MINIS3_LISTEN_ADDR=":$PORT" \
        MINIS3_CERT_FILE="$CERTS_DIR/cert.pem" MINIS3_KEY_FILE="$CERTS_DIR/key.pem" \
        "$TEST_DIR/mini-s3-server" &
        SERVER_PID=$!

        # Wait for server to be ready
        info "Waiting for server to start..."
        for i in $(seq 1 30); do
            if curl -k -s "$ENDPOINT" > /dev/null 2>&1; then
                info "Server is ready (PID $SERVER_PID)"
                break
            fi
            if ! kill -0 "$SERVER_PID" 2>/dev/null; then
                error "Server process died during startup"
                exit 1
            fi
            sleep 0.5
        done

        if ! curl -k -s "$ENDPOINT" > /dev/null 2>&1; then
            error "Server failed to start within 15 seconds"
            kill "$SERVER_PID" 2>/dev/null || true
            exit 1
        fi

        # Pre-populate bucket with objects directly on disk to test read path
        info "Pre-populating bucket with disk-level objects..."
        local bucket_dir="$TEST_DIR/data/$BUCKET"
        local meta_dir="$bucket_dir/.metadata"

        # Pre-existing flat file
        local pre_content="This object was written directly to disk before the server started."
        local pre_path="$bucket_dir/pre-existing.txt"
        local pre_meta="$meta_dir/pre-existing.txt.meta"
        mkdir -p "$(dirname "$pre_meta")"
        echo "$pre_content" > "$pre_path"
        local pre_md5
        pre_md5=$(md5 -q "$pre_path" 2>/dev/null || md5sum "$pre_path" | cut -d' ' -f1)
        local pre_size
        pre_size=$(wc -c < "$pre_path" | tr -d ' ')
        cat > "$pre_meta" <<PREEOF
{
  "contentType": "text/plain",
  "contentLength": $pre_size,
  "eTag": "$pre_md5",
  "customMetadata": {"x-amz-meta-source": "disk-populated"},
  "lastModified": "$(date -u +"%Y-%m-%dT%H:%M:%S.000Z")",
  "storagePath": "$pre_path"
}
PREEOF

        # Pre-existing nested binary file
        local pre_nested_path="$bucket_dir/pre-existing/nested/data.bin"
        local pre_nested_meta="$meta_dir/pre-existing/nested/data.bin.meta"
        mkdir -p "$(dirname "$pre_nested_path")" "$(dirname "$pre_nested_meta")"
        dd if=/dev/urandom of="$pre_nested_path" bs=1024 count=4 2>/dev/null
        local pre_nested_md5
        pre_nested_md5=$(md5 -q "$pre_nested_path" 2>/dev/null || md5sum "$pre_nested_path" | cut -d' ' -f1)
        local pre_nested_size
        pre_nested_size=$(wc -c < "$pre_nested_path" | tr -d ' ')
        cat > "$pre_nested_meta" <<PREEOF
{
  "contentType": "application/octet-stream",
  "contentLength": $pre_nested_size,
  "eTag": "$pre_nested_md5",
  "customMetadata": {},
  "lastModified": "$(date -u +"%Y-%m-%dT%H:%M:%S.000Z")",
  "storagePath": "$pre_nested_path"
}
PREEOF
        info "Pre-populated 2 objects on disk"
    else
        info "Using already-running server at $ENDPOINT"
    fi
}

# ---- Pre-existing Objects (Read Path) ----
test_pre_existing_objects() {
    header "Pre-existing Objects (Read Path)"

    # List objects — pre-existing ones should appear
    assert_contains "ListBuckets includes pre-existing objects" \
        "run_s3 ls --recursive 's3://$BUCKET/'" "pre-existing.txt"

    assert_contains "ListBuckets includes pre-existing nested object" \
        "run_s3 ls --recursive 's3://$BUCKET/'" "pre-existing/nested/data.bin"

    # Head pre-existing object
    local head_out
    head_out=$(run_s3api head-object --bucket "$BUCKET" --key "pre-existing.txt" 2>&1) || true
    if echo "$head_out" | grep -q "ContentLength"; then
        pass "Head pre-existing flat object"
    else
        fail "Head pre-existing flat object" "No ContentLength in response"
    fi

    # Head pre-existing nested object
    head_out=$(run_s3api head-object --bucket "$BUCKET" --key "pre-existing/nested/data.bin" 2>&1) || true
    if echo "$head_out" | grep -q "ContentLength"; then
        pass "Head pre-existing nested object"
    else
        fail "Head pre-existing nested object" "No ContentLength in response"
    fi

    # Download pre-existing flat file and verify content
    run_test "Download pre-existing flat file" \
        run_s3 cp "s3://$BUCKET/pre-existing.txt" "$TEST_DIR/verify-pre-existing.txt"

    local expected_content="This object was written directly to disk before the server started."
    if [[ "$(cat "$TEST_DIR/verify-pre-existing.txt")" == "$expected_content" ]]; then
        pass "Pre-existing flat content matches"
    else
        fail "Pre-existing flat content matches" "Content mismatch"
    fi

    # Download pre-existing nested binary and verify size
    run_test "Download pre-existing nested binary" \
        run_s3 cp "s3://$BUCKET/pre-existing/nested/data.bin" "$TEST_DIR/verify-pre-existing.bin"

    local expected_size=4096  # 4 * 1024
    local actual_size
    actual_size=$(wc -c < "$TEST_DIR/verify-pre-existing.bin" | tr -d ' ')
    if [[ "$actual_size" -eq "$expected_size" ]]; then
        pass "Pre-existing nested binary size matches"
    else
        fail "Pre-existing nested binary size matches" "Expected $expected_size bytes, got $actual_size"
    fi

    # Verify custom metadata from pre-existing object (x-amz-meta-source)
    head_out=$(run_s3api head-object --bucket "$BUCKET" --key "pre-existing.txt" 2>&1) || true
    if echo "$head_out" | grep -q "disk-populated"; then
        pass "Pre-existing custom metadata preserved"
    else
        fail "Pre-existing custom metadata preserved" "Missing x-amz-meta-source"
    fi
}

# ---- Bucket Tests ----
test_bucket_ops() {
    header "Bucket Operations"

    # Create bucket
    run_test "Create bucket" \
        run_s3 mb "s3://$BUCKET"

    # List buckets (should include ours)
    assert_contains "ListBuckets includes our bucket" \
        "run_s3 ls" "$BUCKET"

    # Head bucket (exists)
    run_test "Head bucket (exists)" \
        run_s3api head-bucket --bucket "$BUCKET"

    # Head bucket (non-existent)
    assert_fails "Head bucket (non-existent)" \
        "run_s3api head-bucket --bucket nonexistent-bucket-999 2>&1"

    # Get bucket location
    run_test "Get bucket location" \
        run_s3api get-bucket-location --bucket "$BUCKET"

    # Create bucket again (idempotent)
    run_test "Create bucket (idempotent)" \
        run_s3 mb "s3://$BUCKET"

    # Create bucket invalid name
    assert_fails "Create bucket - too short name" \
        "run_s3 mb 's3://ab' 2>&1"

    assert_fails "Create bucket - uppercase name" \
        "run_s3 mb 's3://MyBucket' 2>&1"
}

# ---- Object Operations ----
test_object_ops() {
    header "Object Operations"

    # Upload text file
    run_test "Upload text file" \
        run_s3 cp "$TEST_FILES_DIR/hello.txt" "s3://$BUCKET/hello.txt"

    # Upload with nested path
    run_test "Upload nested path" \
        run_s3 cp "$TEST_FILES_DIR/hello.txt" "s3://$BUCKET/folder/subfolder/deep.txt"

    # Upload binary file
    run_test "Upload binary file" \
        run_s3 cp "$TEST_FILES_DIR/random.bin" "s3://$BUCKET/binary/random.bin"

    # Upload JSON file
    run_test "Upload JSON file" \
        run_s3 cp "$TEST_FILES_DIR/data.json" "s3://$BUCKET/data.json"

    # List objects (all)
    assert_contains "List all objects" \
        "run_s3 ls 's3://$BUCKET'" "hello.txt"

    # List with prefix (shows PRE subfolder/ since deep.txt is nested)
    assert_contains "List with prefix folder/" \
        "run_s3 ls 's3://$BUCKET/folder/'" "PRE"

    # List with recursive
    assert_contains "List recursive" \
        "run_s3 ls --recursive 's3://$BUCKET'" "data.json"

    # Download and verify content
    run_test "Download hello.txt" \
        run_s3 cp "s3://$BUCKET/hello.txt" "$TEST_DIR/downloaded-hello.txt"

    run_test "Verify hello.txt content" \
        diff "$TEST_FILES_DIR/hello.txt" "$TEST_DIR/downloaded-hello.txt"

    # Download nested path
    run_test "Download nested path" \
        run_s3 cp "s3://$BUCKET/folder/subfolder/deep.txt" "$TEST_DIR/downloaded-deep.txt"

    run_test "Verify nested content" \
        diff "$TEST_FILES_DIR/hello.txt" "$TEST_DIR/downloaded-deep.txt"

    # Download and verify binary
    run_test "Download binary" \
        run_s3 cp "s3://$BUCKET/binary/random.bin" "$TEST_DIR/downloaded-random.bin"

    run_test "Verify binary content" \
        diff "$TEST_FILES_DIR/random.bin" "$TEST_DIR/downloaded-random.bin"

    # Head object
    assert_contains "Head object" \
        "run_s3api head-object --bucket '$BUCKET' --key 'hello.txt'" \
        "ContentLength"

    # Head non-existent object
    assert_fails "Head non-existent object" \
        "run_s3api head-object --bucket '$BUCKET' --key 'nonexistent.txt' 2>&1"

    # Get object (non-existent)
    assert_fails "Get non-existent object" \
        "run_s3 cp 's3://$BUCKET/nonexistent.txt' /dev/null 2>&1"
}

# ---- Listing Tests ----
test_listing() {
    header "Listing Operations"

    # Upload more files for listing tests
    for i in $(seq 1 5); do
        echo "file $i content" > "$TEST_FILES_DIR/file${i}.txt"
        run_s3 cp "$TEST_FILES_DIR/file${i}.txt" "s3://$BUCKET/list-test/file${i}.txt" > /dev/null
    done

    # ListObjectsV2
    run_test "ListObjectsV2 returns objects" \
        run_s3api list-objects-v2 --bucket "$BUCKET" --prefix "list-test/"

    # ListObjectsV2 with max-keys
    local maxkeys_result
    maxkeys_result=$(run_s3api list-objects-v2 --bucket "$BUCKET" --prefix "list-test/" --max-keys 2)
    if echo "$maxkeys_result" | grep -q "NextContinuationToken"; then
        pass "ListObjectsV2 pagination (IsTruncated + NextContinuationToken)"
    else
        fail "ListObjectsV2 pagination (IsTruncated + NextContinuationToken)" "Missing NextContinuationToken"
    fi

    # ListObjectsV2 with delimiter
    local delim_result
    delim_result=$(run_s3api list-objects-v2 --bucket "$BUCKET" --delimiter "/")
    if echo "$delim_result" | grep -q "CommonPrefixes"; then
        pass "ListObjectsV2 with delimiter"
    else
        fail "ListObjectsV2 with delimiter" "Missing CommonPrefixes"
    fi
}

# ---- Delete Operations ----
test_delete_ops() {
    header "Delete Operations"

    # Delete single object
    run_test "Delete single object" \
        run_s3 rm "s3://$BUCKET/data.json"

    assert_fails "Verify object deleted (head)" \
        "run_s3api head-object --bucket '$BUCKET' --key 'data.json' 2>&1"

    # Delete objects with prefix
    run_s3 cp "$TEST_FILES_DIR/hello.txt" "s3://$BUCKET/delete-me/file1.txt" > /dev/null
    run_s3 cp "$TEST_FILES_DIR/hello.txt" "s3://$BUCKET/delete-me/file2.txt" > /dev/null

    run_test "Delete by prefix (recursive)" \
        run_s3 rm "s3://$BUCKET/delete-me/" --recursive

    # Verify prefix is empty
    local verify_output
    verify_output=$(run_s3 ls "s3://$BUCKET/delete-me/" 2>/dev/null) || true
    if [[ -z "$verify_output" ]]; then
        pass "Verify prefix deleted"
    else
        fail "Verify prefix deleted" "Expected empty listing, got: $verify_output"
    fi

    # Delete non-existent object (should succeed silently per S3 spec)
    run_test "Delete non-existent object (idempotent)" \
        run_s3 rm "s3://$BUCKET/nonexistent-obj.txt"
}

# ---- Delete Bucket ----
test_delete_bucket_ops() {
    header "Delete Bucket"

    # Create a separate bucket for deletion test
    run_s3 mb "s3://delete-me-bucket" > /dev/null

    # Delete empty bucket
    run_test "Delete empty bucket" \
        run_s3 rb "s3://delete-me-bucket"

    # Delete non-empty bucket (should fail)
    run_s3 mb "s3://nonempty-bucket" > /dev/null
    run_s3 cp "$TEST_FILES_DIR/hello.txt" "s3://nonempty-bucket/file.txt" > /dev/null

    assert_fails "Delete non-empty bucket (should fail)" \
        "run_s3 rb 's3://nonempty-bucket' 2>&1"

    # Force delete non-empty bucket
    run_test "Force delete non-empty bucket" \
        run_s3 rb "s3://nonempty-bucket" --force
}

# ---- Bucket Actions Test ----
test_bucket_actions() {
    header "Bucket Actions"

    # Create the action log file
    touch "$TEST_DIR/action-log.txt"

    # Upload should trigger after_upload action
    run_test "Upload triggers action" \
        run_s3 cp "$TEST_FILES_DIR/hello.txt" "s3://$BUCKET/action-test.txt"

    # Give async actions time to complete
    sleep 1

    # Check action log
    if [[ -f "$TEST_DIR/action-log.txt" ]]; then
        if grep -q "UPLOAD:action-test.txt" "$TEST_DIR/action-log.txt"; then
            pass "Upload action recorded in log"
        else
            fail "Upload action recorded in log" "Missing upload entry"
        fi
    else
        fail "Upload action recorded in log" "Action log file not found"
    fi

    # Download triggers after_download action
    run_test "Download file" \
        run_s3 cp "s3://$BUCKET/action-test.txt" "$TEST_DIR/downloaded-action.txt"

    sleep 1

    if grep -q "DOWNLOAD:action-test.txt" "$TEST_DIR/action-log.txt"; then
        pass "Download action recorded in log"
    else
        fail "Download action recorded in log" "Missing download entry"
    fi

    # Delete triggers after_delete action
    run_test "Delete triggers action" \
        run_s3 rm "s3://$BUCKET/action-test.txt"

    sleep 1

    if grep -q "DELETE:action-test.txt" "$TEST_DIR/action-log.txt"; then
        pass "Delete action recorded in log"
    else
        fail "Delete action recorded in log" "Missing delete entry"
    fi
}

# ---- Multipart Upload Tests (extended) ----
test_multipart_upload() {
    header "Multipart Upload"

    # Create a 10MB file
    dd if=/dev/urandom of="$TEST_FILES_DIR/large-file.bin" bs=1M count=10 2>/dev/null

    # Upload (AWS CLI auto-uses multipart for files > 8MB)
    run_test "Multipart upload (10MB)" \
        run_s3 cp "$TEST_FILES_DIR/large-file.bin" "s3://$BUCKET/large-file.bin"

    # Download and verify
    run_test "Download multipart result" \
        run_s3 cp "s3://$BUCKET/large-file.bin" "$TEST_DIR/downloaded-large.bin"

    run_test "Verify multipart content" \
        diff "$TEST_FILES_DIR/large-file.bin" "$TEST_DIR/downloaded-large.bin"

    # Clean up
    rm -f "$TEST_FILES_DIR/large-file.bin" "$TEST_DIR/downloaded-large.bin"
    run_s3 rm "s3://$BUCKET/large-file.bin" > /dev/null
}

# ---- Pre-signed URL Tests ----
# TODO: presigned URL auth lands in leaf 3.2 — until then this test is an
# explicit SKIP (tracked by the skip counter), never a silent pass.
test_presigned_urls() {
    header "Pre-signed URLs"

    skip "Pre-signed URL download" "presigned URL auth not yet implemented (leaf 3.2)"
}

# ---- Error Handling Tests ----
test_error_handling() {
    header "Error Handling"

    # Access to non-existent bucket
    assert_fails "Access non-existent bucket" \
        "run_s3 ls 's3://nonexistent-bucket-xyz-123' 2>&1"

    # Invalid bucket name in URL
    assert_fails "Invalid bucket name" \
        "run_s3 mb 's3://AB' 2>&1"

    # Empty object key via s3api (server validates key length)
    assert_fails "Empty object key (PUT)" \
        "run_s3api put-object --bucket '$BUCKET' --key '' --body '$TEST_FILES_DIR/hello.txt' 2>&1"
}

# ---- Final Cleanup ----
final_cleanup() {
    header "Cleanup"

    # Delete remaining objects
    run_s3 rm "s3://$BUCKET" --recursive --force > /dev/null 2>&1 || true

    # Delete the test bucket
    run_test "Delete test bucket" \
        run_s3 rb "s3://$BUCKET" --force
}

# ---- Results Summary ----
print_results() {
    header "Results"
    echo "  Total:  $TOTAL_TESTS"
    echo -e "  ${GREEN}Passed: $PASSED_TESTS${NC}"
    echo "  Skipped: $SKIPPED_TESTS"
    if [[ $FAILED_TESTS -gt 0 ]]; then
        echo -e "  ${RED}Failed: $FAILED_TESTS${NC}"
    else
        echo "  Failed: 0"
    fi
    echo ""

    if [[ $FAILED_TESTS -eq 0 ]]; then
        echo -e "${GREEN}${BOLD}All tests passed!${NC}"
        return 0
    else
        echo -e "${RED}${BOLD}$FAILED_TESTS test(s) failed.${NC}"
        return 1
    fi
}

# ---- Main ----
main() {
    setup

    test_pre_existing_objects
    test_bucket_ops
    test_object_ops
    test_listing
    test_delete_ops

    # Extended tests (multipart upload 10MB)
    if [[ "${EXTENDED_TESTS:-0}" == "1" ]]; then
        test_multipart_upload
    else
        info "Skipping extended tests (set EXTENDED_TESTS=1 to enable)"
    fi

    # Pre-signed URL tests (may not be fully implemented)
    if [[ "${EXTENDED_TESTS:-0}" == "1" ]]; then
        test_presigned_urls
    fi

    test_bucket_actions
    test_error_handling
    test_delete_bucket_ops
    final_cleanup

    # Exit non-zero when any test failed. print_results still prints its
    # summary before we propagate the code (no `|| true` masking).
    print_results
    RESULT=$?
    if [[ $RESULT -ne 0 ]]; then
        exit 1
    fi
}

main "$@"
