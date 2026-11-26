#!/bin/bash
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOCAL_PATH="${LOCAL_PATH:-${SCRIPT_DIR}/}"
LOCAL_PATH="${LOCAL_PATH%/}/"
DRIVE="${DRIVE:-vcb}"
RCLONE="${RCLONE:-rclone}"
INCLUDE_PATTERN="/sync_clipboard.txt"
POLL_INTERVAL="${POLL_INTERVAL:-0.4}"
RETRY_INTERVAL="${RETRY_INTERVAL:-5}"
# After this many consecutive failures, fall back to a full --resync.
MAX_FAILURES="${MAX_FAILURES:-5}"

# Shared flags:
#   --max-lock 2m: expire lock files left behind by a killed run (otherwise every run fails).
COMMON_FLAGS=(--include "$INCLUDE_PATTERN" --max-lock 2m)
# Loop flags:
#   --resilient --recover: retry after less-serious errors / interruptions without --resync.
#   --conflict-resolve newer --conflict-loser delete: keep the newer clipboard instead of
#     renaming both sides to *.conflictN (which would make sync_clipboard.txt disappear).
LOOP_FLAGS=(--force --ignore-checksum --transfers 1 --checkers 1 --drive-use-trash=false
  --resilient --recover --conflict-resolve newer --conflict-loser delete)

log() {
  printf '%(%Y-%m-%d %H:%M:%S)T %s\n' -1 "$*"
}

if ! command -v "$RCLONE" >/dev/null 2>&1; then
  log "rclone not found (RCLONE=$RCLONE), aborting."
  exit 1
fi

resync() {
  "$RCLONE" bisync "$DRIVE:sync" "$LOCAL_PATH" "${COMMON_FLAGS[@]}" --verbose --resync
}

log "Running initial resync..."
if ! resync; then
  log "Initial resync failed, aborting."
  exit 1
fi

failures=0
while true; do
  if "$RCLONE" bisync "$DRIVE:sync" "$LOCAL_PATH" "${COMMON_FLAGS[@]}" "${LOOP_FLAGS[@]}"; then
    if ((failures > 0)); then
      log "Bisync recovered after ${failures} failure(s)."
    fi
    failures=0
    sleep "$POLL_INTERVAL"
    continue
  fi

  failures=$((failures + 1))
  if ((failures >= MAX_FAILURES)); then
    log "Bisync failed ${failures} times in a row, attempting --resync..."
    if resync; then
      log "Resync succeeded. Resuming watch..."
      failures=0
      continue
    fi
    log "Resync failed."
  fi
  log "Bisync failed (${failures}/${MAX_FAILURES}), retrying in ${RETRY_INTERVAL}s..."
  sleep "$RETRY_INTERVAL"
done
