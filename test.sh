#!/bin/bash

LOCAL_FILE="/media/lucifer/STORAGE/IMPORTANT/autoclip/sync_clipboard.txt"
REMOTE_FILE="vcb:sync/sync_clipboard.txt"

# Perform an initial sync (optional, ensures both sides have a file to check)
touch "$LOCAL_FILE"

while true; do
  # 1. Pull from Drive (Updates local file ONLY if remote is newer)
  /usr/bin/rclone copyto "$REMOTE_FILE" "$LOCAL_FILE" --update --quiet --drive-pacer-min-sleep=10ms

  # 2. Push to Drive (Updates remote file ONLY if local is newer)
  /usr/bin/rclone copyto "$LOCAL_FILE" "$REMOTE_FILE" --update --quiet --drive-pacer-min-sleep=10ms

  # Warning: 0.2s WILL trigger Google Drive API bans. 1.5s to 2s is the safest minimum.
  sleep 1.5
done
