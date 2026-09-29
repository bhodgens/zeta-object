#!/bin/bash
# Superseded by scripts/e2e/ — kept for reference.
# Manual test script for zeta-object server
# This script tests basic S3 operations using AWS CLI
#
# Environment variables:
#   PORT                - Server port (default: 8443; wired via ZETAOBJECT_LISTEN_ADDR)
#   SKIP_SERVER         - Set to 1 to use an already-running server (started externally)
#   RUN_MULTIPART_TEST  - Set to 1 to include the multipart upload test

set -euo pipefail

# ---- Configuration ----
PORT="${PORT:-8443}"
ENDPOINT="https://localhost:${PORT}"
AWS_ACCESS_KEY_ID="${ZETAOBJECT_ACCESS_KEY:-minioadmin}"
AWS_SECRET_ACCESS_KEY="${ZETAOBJECT_SECRET_KEY:-minioadmin}"
REGION="us-east-1"
BUCKET="test-bucket-$(date +%s)"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
TEST_DIR="$(mktemp -d /tmp/zetaobject-ops-test-XXXXXX)"
CONFIG_FILE="$TEST_DIR/config.json"
CERTS_DIR="$TEST_DIR/certs"
TEST_FILE="/tmp/test-upload-$$.txt"
DOWNLOAD_FILE="/tmp/test-download-$$.txt"
SERVER_PID=""
FAILED_STEPS=0

# Export credentials
export AWS_ACCESS_KEY_ID
export AWS_SECRET_ACCESS_KEY

# Colors for output
if [[ -t 1 ]]; then
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[1;33m'
    NC='\033[0m' # No Color
else
    RED=''; GREEN=''; YELLOW=''; NC=''
fi

# Helper functions
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

log_test() {
    echo -e "\n${YELLOW}=== TEST: $1 ===${NC}"
}

# Run an aws command, counting a failure instead of aborting (set -e safe)
run_step() {
    local desc="$1"; shift
    if "$@"; then
        log_info "$desc OK"
    else
        log_error "$desc FAILED"
        FAILED_STEPS=$((FAILED_STEPS + 1))
        return 1
    fi
}

cleanup() {
    log_info "Cleaning up..."
    rm -f "$TEST_FILE" "$DOWNLOAD_FILE" "$TEST_FILE.large" "$DOWNLOAD_FILE.large" 2>/dev/null || true
    # Try to delete the test bucket (may fail if already deleted)
    if [[ "${SKIP_SERVER:-0}" == "1" ]]; then
        aws s3 rb "s3://$BUCKET" --force --endpoint-url "$ENDPOINT" --no-verify-ssl --region "$REGION" 2>/dev/null || true
    fi
    # Stop server if we started it
    if [[ -n "${SERVER_PID:-}" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
        kill "$SERVER_PID" 2>/dev/null || true
        wait "$SERVER_PID" 2>/dev/null || true
    fi
    rm -rf "$TEST_DIR" 2>/dev/null || true
}

trap cleanup EXIT

# ---- Server lifecycle (started locally unless SKIP_SERVER=1) ----
start_server() {
    mkdir -p "$TEST_DIR/data" "$CERTS_DIR"

    if ! openssl req -x509 -newkey rsa:2048 -nodes \
        -out "$CERTS_DIR/cert.pem" -keyout "$CERTS_DIR/key.pem" \
        -days 1 -subj "/CN=localhost" -addext "subjectAltName=DNS:localhost" \
        > /dev/null 2>&1; then
        log_error "Failed to generate certificates"
        exit 1
    fi

    cat > "$CONFIG_FILE" <<EOF
{
  "dataDir": "$TEST_DIR/data/"
}
EOF

    log_info "Building zeta-object server..."
    (cd "$PROJECT_DIR" && go build -o "$TEST_DIR/zeta-object-server" .) || {
        log_error "Build failed"
        exit 1
    }

    log_info "Starting zeta-object server on port $PORT..."
    ZETAOBJECT_CONFIG="$CONFIG_FILE" \
    ZETAOBJECT_ACCESS_KEY="$AWS_ACCESS_KEY_ID" \
    ZETAOBJECT_SECRET_KEY="$AWS_SECRET_ACCESS_KEY" \
    ZETAOBJECT_LISTEN_ADDR=":$PORT" \
    ZETAOBJECT_CERT_FILE="$CERTS_DIR/cert.pem" \
    ZETAOBJECT_KEY_FILE="$CERTS_DIR/key.pem" \
    "$TEST_DIR/zeta-object-server" &
    SERVER_PID=$!

    for i in $(seq 1 30); do
        if curl -k -s "$ENDPOINT" > /dev/null 2>&1; then
            log_info "Server is ready (PID $SERVER_PID)"
            return 0
        fi
        if ! kill -0 "$SERVER_PID" 2>/dev/null; then
            log_error "Server process died during startup"
            exit 1
        fi
        sleep 0.5
    done
    log_error "Server failed to start within 15 seconds"
    exit 1
}

# Check prerequisites
check_prerequisites() {
    log_test "Prerequisites Check"

    if ! command -v aws &> /dev/null; then
        log_error "AWS CLI not found. Please install it first."
        exit 1
    fi

    # Check if server is running (starts one unless SKIP_SERVER=1)
    if [[ "${SKIP_SERVER:-0}" == "1" ]]; then
        if ! curl -k -s "$ENDPOINT" > /dev/null 2>&1; then
            log_error "Mini-S3 server is not running at $ENDPOINT"
            log_info "Start the server with: make run (or unset SKIP_SERVER to let this script start one)"
            exit 1
        fi
        log_info "Using already-running server at $ENDPOINT"
    else
        start_server
    fi

    log_info "Prerequisites OK"
}

# AWS CLI arguments shared by every call below
AWS_ARGS=(--endpoint-url "$ENDPOINT" --no-verify-ssl --region "$REGION")

# Test 1: Create bucket
test_create_bucket() {
    log_test "Create Bucket"
    run_step "Create bucket" aws s3 mb "s3://$BUCKET" "${AWS_ARGS[@]}"
}

# Test 2: List buckets
test_list_buckets() {
    log_test "List Buckets"
    aws s3 ls "${AWS_ARGS[@]}"
    log_info "Buckets listed successfully"
}

# Test 3: Upload object
test_upload_object() {
    log_test "Upload Object"

    # Create test file
    echo "Hello, Mini-S3! This is a test file." > "$TEST_FILE"

    run_step "Upload object" aws s3 cp "$TEST_FILE" "s3://$BUCKET/test.txt" "${AWS_ARGS[@]}"
}

# Test 4: List objects
test_list_objects() {
    log_test "List Objects"
    aws s3 ls "s3://$BUCKET" "${AWS_ARGS[@]}"
    log_info "Objects listed successfully"
}

# Test 5: Download object
test_download_object() {
    log_test "Download Object"
    run_step "Download object" aws s3 cp "s3://$BUCKET/test.txt" "$DOWNLOAD_FILE" "${AWS_ARGS[@]}"
}

# Test 6: Verify content integrity
test_verify_content() {
    log_test "Verify Content Integrity"

    if diff "$TEST_FILE" "$DOWNLOAD_FILE" > /dev/null 2>&1; then
        log_info "Content verification PASSED - files match!"
    else
        log_error "Content verification FAILED - files don't match!"
        FAILED_STEPS=$((FAILED_STEPS + 1))
        return 1
    fi
}

# Test 7: Head object
test_head_object() {
    log_test "Head Object (Get Metadata)"
    run_step "Head object" aws s3api head-object --bucket "$BUCKET" --key "test.txt" "${AWS_ARGS[@]}"
}

# Test 8: Upload object with nested path
test_nested_object() {
    log_test "Upload Object with Nested Path"

    run_step "Upload nested object" aws s3 cp "$TEST_FILE" "s3://$BUCKET/folder/subfolder/nested.txt" "${AWS_ARGS[@]}"

    # List with prefix
    log_info "Listing objects with prefix 'folder/':"
    aws s3 ls "s3://$BUCKET/folder/" "${AWS_ARGS[@]}"
}

# Test 9: Delete object
test_delete_object() {
    log_test "Delete Object"
    run_step "Delete object" aws s3 rm "s3://$BUCKET/test.txt" "${AWS_ARGS[@]}"
}

# Test 10: Delete nested objects
test_delete_nested() {
    log_test "Delete Nested Objects"
    run_step "Delete nested objects" aws s3 rm "s3://$BUCKET" --recursive "${AWS_ARGS[@]}"
}

# Test 11: Delete bucket
test_delete_bucket() {
    log_test "Delete Bucket"
    run_step "Delete bucket" aws s3 rb "s3://$BUCKET" "${AWS_ARGS[@]}"
}

# Test 12: Multipart upload (for larger files)
test_multipart_upload() {
    log_test "Multipart Upload"

    # Create a larger test file (10MB)
    dd if=/dev/urandom of="$TEST_FILE.large" bs=1M count=10 2>/dev/null

    # Upload (AWS CLI will automatically use multipart for larger files)
    run_step "Multipart upload" aws s3 cp "$TEST_FILE.large" "s3://$BUCKET/large-file.bin" "${AWS_ARGS[@]}"

    # Download and verify — a content mismatch must fail the suite
    run_step "Multipart download" aws s3 cp "s3://$BUCKET/large-file.bin" "$DOWNLOAD_FILE.large" "${AWS_ARGS[@]}"

    if diff "$TEST_FILE.large" "$DOWNLOAD_FILE.large" > /dev/null 2>&1; then
        log_info "Large file content verification PASSED"
    else
        log_error "Large file content verification FAILED"
        FAILED_STEPS=$((FAILED_STEPS + 1))
        return 1
    fi

    # Cleanup
    rm -f "$TEST_FILE.large" "$DOWNLOAD_FILE.large"
    run_step "Delete multipart object" aws s3 rm "s3://$BUCKET/large-file.bin" "${AWS_ARGS[@]}"
}

# Main execution
main() {
    echo "============================================"
    echo "  Mini-S3 Server Integration Test Suite"
    echo "============================================"
    echo ""
    log_info "Endpoint: $ENDPOINT"
    log_info "Test Bucket: $BUCKET"
    echo ""

    check_prerequisites

    # All tests run; a failure is counted, not fatal, so the whole suite
    # reports. Exit code reflects total failures at the end.
    local test_funcs=(
        test_create_bucket
        test_list_buckets
        test_upload_object
        test_list_objects
        test_download_object
        test_verify_content
        test_head_object
        test_nested_object
        test_delete_object
        test_delete_nested
        test_delete_bucket
    )
    for fn in "${test_funcs[@]}"; do
        "$fn" || true
    done

    # Optional: Run multipart test
    if [ "${RUN_MULTIPART_TEST:-0}" = "1" ]; then
        test_multipart_upload || true
    else
        log_info "Skipping multipart upload test (set RUN_MULTIPART_TEST=1 to enable)"
    fi

    echo ""
    echo "============================================"
    if [[ $FAILED_STEPS -eq 0 ]]; then
        echo -e "  ${GREEN}All tests passed!${NC}"
        echo "============================================"
        return 0
    else
        echo -e "  ${RED}$FAILED_STEPS step(s) failed.${NC}"
        echo "============================================"
        return 1
    fi
}

# Run main; propagate its exit status honestly (no `|| true` here)
main "$@"
