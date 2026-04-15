#!/bin/bash 
LOCAL_PATH="/data/IMPORTANT/autoclip/" 
DRIVE="vcb" 

/usr/bin/rclone bisync $DRIVE:sync "$LOCAL_PATH" --include "sync_clipboard.txt" --verbose --resync 

while true; do 
  /usr/bin/rclone bisync $DRIVE:sync "$LOCAL_PATH" --include "sync_clipboard.txt" --force --ignore-checksum --transfers 1 --checkers 1 --drive-use-trash=false --verbose 
  echo "Bisync complete. Resuming watch..." 
  sleep 0.3 
done
