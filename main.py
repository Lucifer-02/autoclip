import argparse
import logging
import time
from enum import Enum, auto
from pathlib import Path

import pyperclip
from plyer import notification

__version__ = "0.0.2"


class State(Enum):
    WAITING = auto()
    WRITING_CLIP_TO_FILE = auto()
    COPYING_FILE_TO_CLIP = auto()


def compact_message(content: str, limit: int = 64) -> str:
    """Truncates string for cleaner log/notification output."""
    if not content:
        return ""
    content = content.replace("\n", " ").strip()
    if len(content) > limit:
        return content[: (limit - 3)] + "..."
    return content


class ClipboardSync:
    def __init__(self, file_path: Path, enable_notifications: bool = True):
        self.file_path = file_path
        self.enable_notifications = enable_notifications

        # Ensure file exists
        if not self.file_path.exists():
            try:
                self.file_path.touch()
            except OSError as e:
                logging.critical(f"Could not create file {self.file_path}: {e}")
                raise

        # Cache initial state
        self.last_clip = self._safe_paste()
        self.last_mtime = self._get_mtime()
        self.last_file_content = self._safe_read()

        logging.info(f"Sync initialized on: {self.file_path}")

    def _show_notification(self, title: str, message: str, timeout: int = 3):
        if not self.enable_notifications:
            return
        try:
            notification.notify(
                title=title,
                message=message,
                timeout=timeout,
            )
        except Exception as e:
            logging.warning(f"Notification failed: {e}")

    def _get_mtime(self) -> float:
        """Returns the file modification timestamp."""
        try:
            return self.file_path.stat().st_mtime
        except OSError:
            return 0.0

    def _safe_paste(self) -> str:
        """Safely paste from clipboard, handling locks/errors."""
        try:
            return pyperclip.paste() or ""
        except Exception as e:
            logging.warning(f"Clipboard access failed: {e}")
            return self.last_clip or ""

    def _safe_read(self) -> str:
        """Safely read file, returning empty string on failure."""
        try:
            return self.file_path.read_text(encoding="utf-8")
        except Exception:
            return ""

    def transition(self, state: State) -> State:
        """Main State Machine Logic."""

        if state == State.WAITING:
            # 1. Check Clipboard Change
            current_clip = self._safe_paste()
            if current_clip != self.last_clip:
                self.last_clip = current_clip
                return State.WRITING_CLIP_TO_FILE

            # 2. File existence check (Handle syncing/deletion)
            if not self.file_path.exists():
                logging.info(f"File {self.file_path} vanished (syncing?), waiting...")
                return State.WAITING

            # 3. Check for Conflicts (Logging only)
            sync_files = [
                f
                for f in self.file_path.parent.glob(f"{self.file_path.name}*")
                if f.is_file() and f != self.file_path
            ]
            if sync_files:
                logging.warning(
                    f"Potential conflict files detected: {[f.name for f in sync_files]}"
                )

            # 4. Check File Change (Optimization: Check Metadata first)
            current_mtime = self._get_mtime()
            if current_mtime != self.last_mtime:
                self.last_mtime = current_mtime

                # Check if file is empty (often happens during cloud sync lock)
                if self.file_path.stat().st_size == 0:
                    logging.info(f"File {self.file_path} is empty, waiting...")
                    return State.WAITING

                current_file_content = self._safe_read()

                if current_file_content != self.last_file_content:
                    self.last_file_content = current_file_content
                    return State.COPYING_FILE_TO_CLIP
                else:
                    logging.info(f"The file {self.file_path} content has not changed.")

            return State.WAITING

        if state == State.WRITING_CLIP_TO_FILE:
            logging.info(f"Clipboard -> File ({len(self.last_clip)} chars)")

            try:
                self.file_path.write_text(self.last_clip, encoding="utf-8")

                # CRITICAL: Update file caches immediately after we write
                # to prevent the script from detecting its own edit as a new change.
                self.last_file_content = self.last_clip
                self.last_mtime = self._get_mtime()

            except IOError as e:
                logging.error(f"Write failed: {e}")

            return State.WAITING

        if state == State.COPYING_FILE_TO_CLIP:
            logging.info(f"File -> Clipboard ({len(self.last_file_content)} chars)")

            try:
                pyperclip.copy(self.last_file_content)

                self._show_notification(
                    title="Synced to Clipboard",
                    message=compact_message(self.last_file_content),
                    timeout=2,
                )

                # Update clipboard cache immediately to prevent echo
                self.last_clip = self.last_file_content

            except Exception as e:
                logging.error(f"Copy failed: {e}")

            return State.WAITING


def main():
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(levelname)s] %(message)s",
        datefmt="%H:%M:%S",
    )

    parser = argparse.ArgumentParser(description="Bidirectional Clipboard Sync")

    # Arguments
    parser.add_argument(
        "-f",
        "--file-path",
        type=Path,
        default=Path("./sync_clipboard.txt"),
        help="Path to the file used for syncing",
    )
    parser.add_argument(
        "-i", "--interval", type=float, default=0.5, help="Polling interval in seconds"
    )
    parser.add_argument(
        "--no-notify", action="store_true", help="Disable desktop notifications"
    )
    # Version Flag
    parser.add_argument(
        "-v", "--version", action="version", version=f"%(prog)s {__version__}"
    )

    args = parser.parse_args()

    # Use resolve() to handle symlinks and absolute paths better
    sync_file = args.file_path.resolve()

    syncer = ClipboardSync(file_path=sync_file, enable_notifications=not args.no_notify)

    logging.info(f"Sync started (v{__version__}). Ctrl+C to stop.")

    state = State.WAITING
    try:
        while True:
            state = syncer.transition(state)
            time.sleep(args.interval)
    except KeyboardInterrupt:
        logging.info("Stopped.")


if __name__ == "main":
    main()
