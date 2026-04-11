import argparse
import logging
import signal
import sys
import threading
import time
from enum import Enum, auto
from pathlib import Path

import pyperclip
from plyer import notification

__version__ = "0.3.0"


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
        # Resolve symlinks for consistent path comparison
        self.file_path = file_path.resolve()
        self.enable_notifications = enable_notifications

        # Cache glob pattern components to avoid recomputing every cycle
        self._dir = self.file_path.parent
        self._glob_pattern = f"{self.file_path.name}*"

        # Ensure file exists
        if not self.file_path.exists():
            try:
                self.file_path.touch()
            except OSError as e:
                logging.critical(f"Could not create file {self.file_path}: {e}")
                raise

        # Initialize caches
        self.last_clip: str = self._safe_paste()
        self.last_mtime: float = self._get_mtime()
        self.last_file_content: str = self._safe_read()

        logging.info(f"Sync initialized on: {self.file_path}")

    # ------------------------------------------------------------------
    # Helpers
    # ------------------------------------------------------------------

    def _show_notification(self, title: str, message: str, timeout: int = 1) -> None:
        """Show desktop notification in a daemon thread to avoid blocking the loop."""
        if not self.enable_notifications:
            return

        def _notify():
            try:
                notification.notify(title=title, message=message, timeout=timeout)
            except Exception as e:
                logging.warning(f"Notification failed: {e}")

        t = threading.Thread(target=_notify, daemon=True)
        t.start()

    def _get_mtime(self) -> float:
        """Returns the file modification timestamp, or 0.0 on error."""
        try:
            return self.file_path.stat().st_mtime
        except OSError:
            return 0.0

    def _safe_paste(self) -> str:
        """Read clipboard with retries. Returns '' on persistent failure."""
        max_retries = 3
        for attempt in range(max_retries):
            try:
                return pyperclip.paste() or ""
            except Exception as e:
                if attempt < max_retries - 1:
                    time.sleep(0.05)
                    continue
                error_msg = str(e)
                if "completed successfully" in error_msg:
                    logging.debug(f"Clipboard locked by another app: {error_msg}")
                else:
                    logging.warning(f"Clipboard access failed: {error_msg}")
        # Return empty string rather than stale content on persistent failure
        return ""

    def _safe_read(self) -> str:
        """Read file as UTF-8 text. Returns '' on any error.

        A single read call avoids the TOCTOU race between a separate
        stat().st_size == 0 check and the actual read.
        """
        try:
            return self.file_path.read_text(encoding="utf-8")
        except Exception:
            return ""

    def _check_conflicts(self) -> None:
        """Log any conflict/duplicate files created by sync tools."""
        sync_files = [
            f
            for f in self._dir.glob(self._glob_pattern)
            if f.is_file() and f != self.file_path
        ]
        if sync_files:
            logging.warning(
                f"Potential conflict files detected: {[f.name for f in sync_files]}"
            )

    # ------------------------------------------------------------------
    # State machine
    # ------------------------------------------------------------------

    def transition(self, state: State) -> State:
        """Advance the state machine by one step."""

        if state == State.WAITING:
            # 1. Check clipboard change
            current_clip = self._safe_paste()
            if current_clip != self.last_clip:
                self.last_clip = current_clip
                return State.WRITING_CLIP_TO_FILE

            # 2. File existence check
            if not self.file_path.exists():
                logging.info(f"File {self.file_path} vanished (syncing?), waiting...")
                return State.WAITING

            # 3. Conflict detection (cached pattern, no extra stat per cycle)
            self._check_conflicts()

            # 4. Check file change by mtime
            current_mtime = self._get_mtime()
            if current_mtime == self.last_mtime:
                return State.WAITING

            self.last_mtime = current_mtime

            # Single read — avoids TOCTOU race between size check and content read
            current_file_content = self._safe_read()
            if not current_file_content:
                logging.info(
                    f"File {self.file_path} is empty (cloud sync lock?), waiting..."
                )
                return State.WAITING

            if current_file_content != self.last_file_content:
                self.last_file_content = current_file_content
                return State.COPYING_FILE_TO_CLIP

            logging.info(
                f"File {self.file_path} mtime changed but content is unchanged."
            )
            return State.WAITING

        if state == State.WRITING_CLIP_TO_FILE:
            logging.info(f"Clipboard -> File ({len(self.last_clip)} chars)")
            try:
                self.file_path.write_text(self.last_clip, encoding="utf-8")
                # Update caches immediately to suppress self-triggered file-change detection
                self.last_file_content = self.last_clip
                self.last_mtime = self._get_mtime()
            except OSError as e:
                logging.error(f"Write failed: {e}")
            return State.WAITING

        if state == State.COPYING_FILE_TO_CLIP:
            logging.info(f"File -> Clipboard ({len(self.last_file_content)} chars)")
            try:
                pyperclip.copy(self.last_file_content)
                # Update cache before notification (non-blocking) to prevent echo
                self.last_clip = self.last_file_content
                self._show_notification(
                    title="Synced to Clipboard",
                    message=compact_message(self.last_file_content),
                    timeout=1,
                )
            except Exception as e:
                logging.error(f"Copy failed: {e}")
            return State.WAITING

        return State.WAITING


# ------------------------------------------------------------------
# Entry point
# ------------------------------------------------------------------


def main() -> None:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(levelname)s] %(message)s",
        datefmt="%H:%M:%S",
    )

    parser = argparse.ArgumentParser(description="Bidirectional Clipboard Sync")
    parser.add_argument(
        "-f",
        "--file-path",
        type=Path,
        default=Path("./sync_clipboard.txt"),
        help="Path to the file used for syncing",
    )
    parser.add_argument(
        "-i",
        "--interval",
        type=float,
        default=0.5,
        help="Polling interval in seconds",
    )
    parser.add_argument(
        "--no-notify",
        action="store_true",
        help="Disable desktop notifications",
    )
    parser.add_argument(
        "-v",
        "--version",
        action="version",
        version=f"%(prog)s {__version__}",
    )
    args = parser.parse_args()

    sync_file = args.file_path.resolve()
    logging.info(
        f"Sync (v{__version__}) starting — interval: {args.interval}s, "
        f"file: {sync_file}. Press Ctrl+C to stop."
    )

    syncer = ClipboardSync(file_path=sync_file, enable_notifications=not args.no_notify)

    # Graceful shutdown on SIGTERM (Ctrl+C is handled by KeyboardInterrupt below)
    def _handle_sigterm(signum, frame):
        logging.info("Received SIGTERM. Stopped.")
        sys.exit(0)

    signal.signal(signal.SIGTERM, _handle_sigterm)

    state = State.WAITING
    try:
        while True:
            # Record start time to keep interval drift-free
            t_start = time.monotonic()
            state = syncer.transition(state)
            elapsed = time.monotonic() - t_start
            sleep_for = max(0.0, args.interval - elapsed)
            time.sleep(sleep_for)
    except KeyboardInterrupt:
        logging.info("Stopped.")


if __name__ == "__main__":
    main()
