#!/bin/bash
# Liveness probe for sidecar container
# Only checks that health checker is running (file being updated)

set -euo pipefail

HEALTH_FILE="${HEALTH_FILE_PATH:-/shared/health-status.txt}"
# Default to 90 seconds to allow for some delay in the health check.
MAX_AGE_SECONDS=${1:-90}

# Get the directory of this script to find the shared utility
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Check file age using shared utility
FILE_AGE=$("$SCRIPT_DIR/check-file-age.sh" "$HEALTH_FILE" "$MAX_AGE_SECONDS") || exit 1

echo "Health checker active (${FILE_AGE}s ago)"
exit 0 
