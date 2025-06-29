#!/bin/bash
# Liveness probe for smee container
# Checks both file age and success status

set -euo pipefail

HEALTH_FILE="${HEALTH_FILE_PATH:-/shared/health-status.txt}"
MAX_AGE_SECONDS=${1:-60}

# Get the directory of this script to find the shared utility
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Check file age using shared utility
FILE_AGE=$("$SCRIPT_DIR/check-file-age.sh" "$HEALTH_FILE" "$MAX_AGE_SECONDS") || exit 1

# Check health status (simple text format: status=success|failure)
STATUS=$(grep "^status=" "$HEALTH_FILE" 2>/dev/null | cut -d'=' -f2 || echo "unknown")
if [[ "$STATUS" != "success" ]]; then
    MESSAGE=$(grep "^message=" "$HEALTH_FILE" 2>/dev/null | cut -d'=' -f2- || echo "no message")
    echo "Health check failed: $STATUS - $MESSAGE"
    exit 1
fi

echo "Health check passed (${FILE_AGE}s old)"
exit 0 
