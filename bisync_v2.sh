#!/bin/bash
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOCAL_PATH="${LOCAL_PATH:-${SCRIPT_DIR}/}"
LOCAL_PATH="${LOCAL_PATH%/}/"
FILE_NAME="${FILE_NAME:-sync_clipboard.txt}"
LOCAL_FILE="${LOCAL_PATH}${FILE_NAME}"
DRIVE="${DRIVE:-vcb}"
INCLUDE_PATTERN="/${FILE_NAME}"

# Fallback poll interval in whole seconds (inotifywait -t requires an integer).
# Local edits trigger a sync immediately via inotify instead of waiting this out.
POLL_INTERVAL="${POLL_INTERVAL:-2}"
RETRY_INTERVAL="${RETRY_INTERVAL:-5}"
DEBOUNCE="${DEBOUNCE:-0.2}"
DEBOUNCE_MAX_STEPS="${DEBOUNCE_MAX_STEPS:-5}"

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

# Validate POLL_INTERVAL as a positive integer for inotifywait -t.
if ! [[ "$POLL_INTERVAL" =~ ^[1-9][0-9]*$ ]]; then
  log "POLL_INTERVAL must be a positive integer for inotifywait (got '$POLL_INTERVAL'); defaulting to 2."
  POLL_INTERVAL=2
fi

HAS_INOTIFY=true
if ! command -v inotifywait >/dev/null 2>&1; then
  log "WARNING: inotifywait not found; falling back to plain ${POLL_INTERVAL}s polling."
  HAS_INOTIFY=false
fi

# Escape regex metacharacters in FILE_NAME for inotifywait --include.
FILE_NAME_REGEX="$(printf '%s' "$FILE_NAME" | sed 's/[.[\\*^$()+?{|]/\\&/g')"

# Coalesce rapid local edits using file mtime+size without passing fractional
# seconds to inotifywait -t (which only accepts integers).
debounce_local_edits() {
  local prev curr step
  prev="$(stat -c '%y %s' "$LOCAL_FILE" 2>/dev/null || true)"
  for ((step = 0; step < DEBOUNCE_MAX_STEPS; step++)); do
    sleep "$DEBOUNCE"
    curr="$(stat -c '%y %s' "$LOCAL_FILE" 2>/dev/null || true)"
    if [ "$curr" = "$prev" ]; then
      break
    fi
    prev="$curr"
  done
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
  if [ "$HAS_INOTIFY" = false ] || [ ! -d "$LOCAL_PATH" ]; then
    sleep "$POLL_INTERVAL"
  else
    # Watch the parent directory filtered to FILE_NAME so atomic writes
    # (temp file + rename -> MOVED_TO) and file creation/edits are all caught
    # reliably even if the file does not exist yet or another process holds
    # the old inode open.
    # Exit status: 0 = event fired, 2 = timeout, other = error.
    inotifywait -q -t "$POLL_INTERVAL" \
      -e modify,close_write,create,moved_to \
      --include "(^|/)${FILE_NAME_REGEX}$" \
      "$LOCAL_PATH" >/dev/null 2>&1
    status=$?

    if [ "$status" -eq 0 ]; then
      debounce_local_edits
    elif [ "$status" -ne 2 ]; then
      log "inotifywait failed (exit $status); sleeping ${POLL_INTERVAL}s before retry..."
      sleep "$POLL_INTERVAL"
    fi
  fi

  if run_bisync; then
    log "Bisync complete. Resuming watch..."
  else
    log "Bisync failed, retrying in ${RETRY_INTERVAL}s..."
    sleep "$RETRY_INTERVAL"
  fi
done
