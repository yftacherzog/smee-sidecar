#!/bin/bash
# Shared utility for checking file age
# Usage: check-file-age.sh <file_path> [max_age_seconds]

set -euo pipefail

if [[ $# -lt 1 ]]; then
    echo "Usage: $0 <file_path> [max_age_seconds]" >&2
    exit 1
fi

HEALTH_FILE="$1"
MAX_AGE_SECONDS="${2:-60}"

if [[ ! -f "$HEALTH_FILE" ]]; then
    echo "Health file missing: $HEALTH_FILE" >&2
    exit 1
fi

FILE_AGE=$(( $(date +%s) - $(stat -c %Y "$HEALTH_FILE") ))
if [[ $FILE_AGE -gt $MAX_AGE_SECONDS ]]; then
    echo "Health file stale: ${FILE_AGE}s old (max: ${MAX_AGE_SECONDS}s)" >&2
    exit 1
fi

echo "$FILE_AGE"  # Output the age for callers 
