#!/bin/bash
# Test script for health check scripts
# This runs locally without Kubernetes to test script logic

set -euo pipefail

echo "--- Testing Health Check Scripts ---"

# Create temporary test directory for health status file
TEST_DIR=$(mktemp -d)
echo "Using test directory: $TEST_DIR"

# Set environment variable to use temp directory for testing
export HEALTH_FILE_PATH="$TEST_DIR/health-status.txt"

cleanup() {
    rm -rf "$TEST_DIR"
}
trap cleanup EXIT

# Test 1: Missing health file (should fail)
echo "Testing missing health file..."
if cmd/scripts/check-smee-health.sh 2>/dev/null; then
    echo "ERROR: check-smee-health.sh should fail with missing file"
    exit 1
fi

if cmd/scripts/check-sidecar-health.sh 2>/dev/null; then
    echo "ERROR: check-sidecar-health.sh should fail with missing file"
    exit 1
fi
echo "✓ Missing file test passed"

# Test 2: Valid health file (should pass)
echo "Testing valid health file..."
cat > "$HEALTH_FILE_PATH" << 'EOF'
status=success
message=Health check completed successfully
EOF

if ! cmd/scripts/check-smee-health.sh >/dev/null 2>&1; then
    echo "ERROR: check-smee-health.sh should pass with valid file"
    exit 1
fi

if ! cmd/scripts/check-sidecar-health.sh >/dev/null 2>&1; then
    echo "ERROR: check-sidecar-health.sh should pass with valid file"
    exit 1
fi
echo "✓ Valid file test passed"

# Test 3: Failed health status (smee should fail, sidecar should pass)
echo "Testing failed health status..."
cat > "$HEALTH_FILE_PATH" << 'EOF'
status=failure
message=Connection timeout
EOF

if cmd/scripts/check-smee-health.sh 2>/dev/null; then
    echo "ERROR: check-smee-health.sh should fail with failure status"
    exit 1
fi

if ! cmd/scripts/check-sidecar-health.sh >/dev/null 2>&1; then
    echo "ERROR: check-sidecar-health.sh should pass regardless of status"
    exit 1
fi
echo "✓ Failed status test passed"

# Test 4: Stale file (both should fail)
echo "Testing stale health file..."
touch -d "2 minutes ago" "$HEALTH_FILE_PATH"

if cmd/scripts/check-smee-health.sh 2>/dev/null; then
    echo "ERROR: check-smee-health.sh should fail with stale file"
    exit 1
fi

if cmd/scripts/check-sidecar-health.sh 2>/dev/null; then
    echo "ERROR: check-sidecar-health.sh should fail with stale file"
    exit 1
fi
echo "✓ Stale file test passed"

# Test 5: Custom timeout parameters
echo "Testing custom timeout parameters..."
cat > "$HEALTH_FILE_PATH" << 'EOF'
status=success
message=All good
EOF
touch -d "75 seconds ago" "$HEALTH_FILE_PATH"

# Should fail with default 60s timeout
if cmd/scripts/check-smee-health.sh 2>/dev/null; then
    echo "ERROR: check-smee-health.sh should fail with 60s timeout"
    exit 1
fi

# Should pass with 100s timeout
if ! cmd/scripts/check-smee-health.sh 100 >/dev/null 2>&1; then
    echo "ERROR: check-smee-health.sh should pass with 100s timeout"
    exit 1
fi
echo "✓ Custom timeout test passed"

# Test 6: File age utility directly
echo "Testing file age utility..."
if ! cmd/scripts/check-file-age.sh "$HEALTH_FILE_PATH" 100 >/dev/null 2>&1; then
    echo "ERROR: check-file-age.sh should pass with 100s timeout"
    exit 1
fi

if cmd/scripts/check-file-age.sh "$HEALTH_FILE_PATH" 30 2>/dev/null; then
    echo "ERROR: check-file-age.sh should fail with 30s timeout"
    exit 1
fi
echo "✓ File age utility test passed"

# Test 7: Edge cases
echo "Testing edge cases..."

# Empty file
: > "$HEALTH_FILE_PATH"
if cmd/scripts/check-smee-health.sh 2>/dev/null; then
    echo "ERROR: check-smee-health.sh should fail with empty file"
    exit 1
fi

# Malformed file
echo "invalid content" > "$HEALTH_FILE_PATH"
if cmd/scripts/check-smee-health.sh 2>/dev/null; then
    echo "ERROR: check-smee-health.sh should fail with malformed file"
    exit 1
fi

# Missing status line
echo "message=test message" > "$HEALTH_FILE_PATH"
if cmd/scripts/check-smee-health.sh 2>/dev/null; then
    echo "ERROR: check-smee-health.sh should fail with missing status"
    exit 1
fi
echo "✓ Edge cases test passed"

echo "--- All Script Tests Passed! ---" 
