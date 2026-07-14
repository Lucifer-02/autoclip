#!/bin/bash
set -uo pipefail

LOCAL_PATH="/data/IMPORTANT/autoclip/"
LOCAL_FILE="${LOCAL_PATH}sync_clipboard.txt"
DRIVE="vcb"
INCLUDE_PATTERN="sync_clipboard.txt"

# Fallback poll interval: bounds how long we wait for a remote-side change
# when no local edit occurs. Local edits trigger a sync immediately instead
# of waiting out this interval.
POLL_INTERVAL="${POLL_INTERVAL:-2}"
RETRY_INTERVAL="${RETRY_INTERVAL:-5}"
DEBOUNCE="${DEBOUNCE:-0.2}"

RCLONE_COMMON_OPTS=(
  --include "$INCLUDE_PATTERN"
  --fast-list
  --resilient
  --recover
  --verbose
)

log() {
  printf '%(%Y-%m-%d %H:%M:%S)T %s\n' -1 "$*"
}

run_bisync() {
  /usr/bin/rclone bisync "$DRIVE:sync" "$LOCAL_PATH" "${RCLONE_COMMON_OPTS[@]}" \
    --force --ignore-checksum --transfers 1 --checkers 1 --drive-use-trash=false
}

log "Running initial resync..."
if ! /usr/bin/rclone bisync "$DRIVE:sync" "$LOCAL_PATH" "${RCLONE_COMMON_OPTS[@]}" --resync; then
  log "Initial resync failed, aborting."
  exit 1
fi

while true; do
  if [ ! -e "$LOCAL_FILE" ]; then
    # Nothing to watch yet (e.g. deleted mid-sync); fall back to a plain wait.
    sleep "$POLL_INTERVAL"
  else
    # Block until either the local file changes (fast path) or POLL_INTERVAL
    # elapses (slow path, catches remote-side changes). Exit status: 0 = event
    # fired, 1 = error (e.g. file vanished mid-wait), 2 = timeout.
    inotifywait -q -t "$POLL_INTERVAL" -e modify,close_write,create,moved_to,delete_self \
      "$LOCAL_FILE" >/dev/null 2>&1
    status=$?

    if [ "$status" -eq 0 ]; then
      # Let the writer finish and coalesce any burst of rapid edits.
      sleep "$DEBOUNCE"
      while inotifywait -q -t "$DEBOUNCE" -e modify,close_write,create,moved_to,delete_self \
        "$LOCAL_FILE" >/dev/null 2>&1; do :; done
    fi
  fi

  if run_bisync; then
    log "Bisync complete. Resuming watch..."
  else
    log "Bisync failed, retrying in ${RETRY_INTERVAL}s..."
    sleep "$RETRY_INTERVAL"
  fi
done
