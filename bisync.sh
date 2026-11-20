#!/bin/bash
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOCAL_PATH="${LOCAL_PATH:-${SCRIPT_DIR}/}"
LOCAL_PATH="${LOCAL_PATH%/}/"
DRIVE="${DRIVE:-vcb}"
INCLUDE_PATTERN="/sync_clipboard.txt"
POLL_INTERVAL="${POLL_INTERVAL:-0.4}"
RETRY_INTERVAL="${RETRY_INTERVAL:-5}"

log() {
  printf '%(%Y-%m-%d %H:%M:%S)T %s\n' -1 "$*"
}

log "Running initial resync..."
if ! /usr/bin/rclone bisync "$DRIVE:sync" "$LOCAL_PATH" --include "$INCLUDE_PATTERN" --verbose --resync; then
  log "Initial resync failed, aborting."
  exit 1
fi

while true; do
  if /usr/bin/rclone bisync "$DRIVE:sync" "$LOCAL_PATH" --include "$INCLUDE_PATTERN" --force --ignore-checksum --transfers 1 --checkers 1 --drive-use-trash=false; then
    log "Bisync complete. Resuming watch..."
    sleep "$POLL_INTERVAL"
  else
    log "Bisync failed, retrying in ${RETRY_INTERVAL}s..."
    sleep "$RETRY_INTERVAL"
  fi
done
