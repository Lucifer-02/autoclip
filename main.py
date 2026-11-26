import argparse
import hashlib
import logging
import math
import os
import queue
import signal
import tempfile
import threading
import time
from collections.abc import Callable
from enum import Enum, auto
from pathlib import Path

import pyperclip

__version__ = "0.6.1"

MAX_CONTENT_BYTES = 5 * 1024 * 1024  # 5 MiB
MIN_INTERVAL = 0.05
MAX_INTERVAL = 3600.0
DEFAULT_INTERVAL = 0.5
IO_RETRIES = 3
RETRY_DELAY = 0.05

logger = logging.getLogger(__name__)


class State(Enum):
    WAITING = auto()
    WRITING_CLIP_TO_FILE = auto()
    COPYING_FILE_TO_CLIP = auto()


def parse_log_level(value: str) -> tuple[int, bool]:
    levels = {
        "DEBUG": logging.DEBUG,
        "INFO": logging.INFO,
        "WARN": logging.WARNING,
        "WARNING": logging.WARNING,
        "ERROR": logging.ERROR,
    }
    key = value.strip().upper()
    if key in levels:
        return levels[key], True
    return logging.INFO, False


def validate_interval(interval: float) -> float:
    if math.isnan(interval) or math.isinf(interval):
        logger.warning(
            "Interval %r is not finite, defaulting to %ss.", interval, DEFAULT_INTERVAL
        )
        return DEFAULT_INTERVAL
    if interval < MIN_INTERVAL:
        logger.warning(
            "Interval %ss is too small, clamping to %ss.", interval, MIN_INTERVAL
        )
        return MIN_INTERVAL
    if interval > MAX_INTERVAL:
        logger.warning(
            "Interval %ss is too large, clamping to %ss.", interval, MAX_INTERVAL
        )
        return MAX_INTERVAL
    return interval


def compact_message(content: str, limit: int = 64) -> str:
    """Format a short preview for desktop notifications (never alters clipboard)."""
    if not content or limit <= 0:
        return ""
    preview = content.replace("\r\n", " ").replace("\r", " ").replace("\n", " ").strip()
    if len(preview) <= limit:
        return preview
    if limit <= 3:
        return preview[:limit]
    return preview[: limit - 3] + "..."


def _encode_clip(clip: str) -> tuple[bytes, bool]:
    """Encode clipboard text as UTF-8; return (bytes, valid).

    Lone surrogates (e.g. a truncated emoji on Windows) cannot be encoded strictly;
    they are passed through so the size can still be measured, and valid=False.
    """
    try:
        return clip.encode("utf-8"), True
    except UnicodeEncodeError:
        return clip.encode("utf-8", "surrogatepass"), False


class ClipboardSync:
    """Bidirectional clipboard <-> file sync preserving exact UTF-8 content verbatim."""

    def __init__(
        self,
        file_path: Path,
        enable_notifications: bool = True,
        hide_notification_content: bool = False,
        *,
        paste_fn: Callable[[], str | None] = pyperclip.paste,
        copy_fn: Callable[[str], None] = pyperclip.copy,
        notify_fn: Callable[..., None] | None = None,
    ) -> None:
        self.file_path = file_path.resolve()
        self.enable_notifications = enable_notifications
        self.hide_notification_content = hide_notification_content
        self._dir = self.file_path.parent
        self._file_name = self.file_path.name

        self._paste_fn = paste_fn
        self._copy_fn = copy_fn
        self._notify_fn: Callable[..., None] | None = notify_fn

        self._ensure_file_exists()

        # Single bounded worker thread prevents unbounded threads if notifications hang.
        # Lazy-import plyer only when notifications are enabled so --help/--version/--no-notify
        # stay silent on headless or non-dbus Linux setups.
        self._notify_queue: queue.Queue[tuple[str, str, int]] = queue.Queue(maxsize=1)
        if self.enable_notifications:
            if self._notify_fn is None:
                from plyer import notification

                self._notify_fn = notification.notify
            threading.Thread(target=self._notification_worker, daemon=True).start()

        # Sync caches (updated only after reads/writes succeed).
        self.last_clip = ""
        self._clip_ready = False
        self._oversized_clip_hash: bytes | None = None

        self.last_mtime: float | None = None
        self.last_file_content = ""

        # Pending payloads waiting for the next state step.
        self._pending_clip = ""
        self._pending_file_content = ""
        self._pending_mtime: float | None = None

        # Log/scan deduplication state.
        self._last_dir_mtime: float | None = None
        self._last_conflicts: tuple[str, ...] = ()
        self._last_empty_mtime: float | None = None
        self._last_copy_fail_mtime: float | None = None

        # Seed initial state without modifying either clipboard or file.
        clip, clip_ok = self._safe_paste()
        if clip_ok:
            self._init_clip(clip)

        mtime, exists = self._get_mtime()
        if exists:
            content, read_ok, skip_reason = self._safe_read()
            if read_ok:
                self.last_mtime = mtime
                if skip_reason is None:
                    self.last_file_content = content

    # ------------------------------------------------------------------
    # Low-level I/O helpers (preserve exact bytes, never alter content)
    # ------------------------------------------------------------------

    def _ensure_file_exists(self) -> None:
        if self.file_path.exists():
            return
        try:
            fd = os.open(self.file_path, os.O_CREAT | os.O_WRONLY, 0o600)
            os.close(fd)
        except OSError as e:
            logger.critical("Could not create file %s: %s", self.file_path, e)
            raise

    def _init_clip(self, clip: str) -> None:
        """Seed last_clip. Invalid UTF-8 within the size limit is retained too, so
        the last_clip equality check skips it until the clipboard changes."""
        raw, _ = _encode_clip(clip)
        if len(raw) > MAX_CONTENT_BYTES:
            self._oversized_clip_hash = hashlib.sha256(raw).digest()
            self.last_clip = ""
        else:
            self.last_clip = clip
        self._clip_ready = True

    def _notification_worker(self) -> None:
        while True:
            title, message, timeout = self._notify_queue.get()
            if self._notify_fn is None:
                continue
            try:
                self._notify_fn(title=title, message=message, timeout=timeout)
            except Exception as e:  # noqa: BLE001 — plyer backends raise varied OS/DBus errors
                logger.warning("Notification failed: %s", e)

    def _show_notification(self, title: str, message: str, timeout: int = 1) -> None:
        if not self.enable_notifications:
            return
        try:
            self._notify_queue.get_nowait()
        except queue.Empty:
            pass
        try:
            self._notify_queue.put_nowait((title, message, timeout))
        except queue.Full:
            pass

    def _get_mtime(self) -> tuple[float, bool]:
        try:
            return self.file_path.stat().st_mtime, True
        except OSError:
            return 0.0, False

    def _safe_paste(self) -> tuple[str, bool]:
        for attempt in range(IO_RETRIES):
            try:
                return (self._paste_fn() or ""), True
            except Exception as e:  # noqa: BLE001 — pyperclip backends raise varied OS errors
                if attempt < IO_RETRIES - 1:
                    time.sleep(RETRY_DELAY)
                    continue
                msg = str(e)
                if "completed successfully" in msg:
                    logger.debug("Clipboard locked by another app: %s", msg)
                else:
                    logger.debug("Clipboard access failed: %s", msg)
        return "", False

    def _safe_copy(self, content: str) -> Exception | None:
        """Copy to clipboard with retries; return the last error, or None on success."""
        error: Exception | None = None
        for attempt in range(IO_RETRIES):
            try:
                self._copy_fn(content)
                return None
            except Exception as e:  # noqa: BLE001 — pyperclip backends raise varied OS errors
                error = e
                if attempt < IO_RETRIES - 1:
                    time.sleep(RETRY_DELAY)
        return error

    def _safe_read(self) -> tuple[str, bool, str | None]:
        """Read up to MAX_CONTENT_BYTES+1 raw bytes; return (text, ok, skip_reason).

        A skip_reason means the file was read but cannot be synced (too large or
        not valid UTF-8); its content is not returned.
        """
        try:
            with open(self.file_path, "rb") as f:
                raw = f.read(MAX_CONTENT_BYTES + 1)
        except OSError as e:
            logger.debug("File read failed for %s: %s", self.file_path, e)
            return "", False, None

        if len(raw) > MAX_CONTENT_BYTES:
            return "", True, f"exceeds {MAX_CONTENT_BYTES}-byte limit"
        try:
            return raw.decode("utf-8"), True, None
        except UnicodeDecodeError:
            return "", True, "is not valid UTF-8"

    def _atomic_write(self, content: str) -> float:
        """Write exact UTF-8 bytes via a unique 0600 temp file + atomic rename."""
        data = content.encode("utf-8")
        fd, tmp_name = tempfile.mkstemp(
            dir=self._dir,
            prefix=f".{self._file_name}.",
            suffix=".tmp",
        )
        tmp_path = Path(tmp_name)
        try:
            try:
                f = os.fdopen(fd, "wb")
            except BaseException:
                os.close(fd)
                raise
            with f:
                f.write(data)

            written_mtime = tmp_path.stat().st_mtime
            for attempt in range(IO_RETRIES):
                try:
                    os.replace(tmp_path, self.file_path)
                    break
                except OSError:
                    if attempt < IO_RETRIES - 1:
                        time.sleep(RETRY_DELAY)
                        continue
                    raise
            return written_mtime
        except BaseException:
            try:
                tmp_path.unlink(missing_ok=True)
            except OSError:
                pass
            raise

    def _check_conflicts(self) -> None:
        try:
            dir_mtime = self._dir.stat().st_mtime
        except OSError:
            return
        if dir_mtime == self._last_dir_mtime:
            return
        self._last_dir_mtime = dir_mtime

        try:
            conflicts = tuple(
                sorted(
                    entry.name
                    for entry in os.scandir(self._dir)
                    if entry.name != self._file_name
                    and entry.name.startswith(self._file_name)
                    and entry.is_file()
                )
            )
        except OSError:
            return

        if conflicts != self._last_conflicts:
            self._last_conflicts = conflicts
            if conflicts:
                logger.warning("Potential conflict files detected: %s", list(conflicts))
            else:
                logger.info("Conflict files cleared.")

    # ------------------------------------------------------------------
    # State machine
    # ------------------------------------------------------------------

    def transition(self, state: State) -> State:
        if state == State.WAITING:
            return self._poll()
        if state == State.WRITING_CLIP_TO_FILE:
            return self._write_clip_to_file()
        if state == State.COPYING_FILE_TO_CLIP:
            return self._copy_file_to_clip()
        return State.WAITING

    def _poll(self) -> State:
        next_state = self._check_clipboard()
        if next_state != State.WAITING:
            return next_state
        return self._check_file()

    def _check_clipboard(self) -> State:
        current_clip, ok = self._safe_paste()
        if not ok:
            return State.WAITING

        if not self._clip_ready:
            self._init_clip(current_clip)
            return State.WAITING

        if current_clip == self.last_clip:
            return State.WAITING

        if not current_clip:
            logger.debug("Clipboard is empty or non-text, skipping file write.")
            self.last_clip = ""
            self._oversized_clip_hash = None
            return State.WAITING

        raw, valid = _encode_clip(current_clip)
        if len(raw) > MAX_CONTENT_BYTES:
            digest = hashlib.sha256(raw).digest()
            if digest != self._oversized_clip_hash:
                logger.warning(
                    "Clipboard content (%d bytes) exceeds %d-byte limit, "
                    "skipping file write.",
                    len(raw),
                    MAX_CONTENT_BYTES,
                )
                self._oversized_clip_hash = digest
            return State.WAITING
        if not valid:
            logger.warning(
                "Clipboard content is not valid UTF-8 (lone surrogates), "
                "skipping file write."
            )
            self.last_clip = current_clip  # within the size limit, cheap to retain
            return State.WAITING

        self._oversized_clip_hash = None
        self._pending_clip = current_clip
        return State.WRITING_CLIP_TO_FILE

    def _check_file(self) -> State:
        current_mtime, exists = self._get_mtime()
        if not exists:
            logger.debug("File %s vanished (syncing?), waiting...", self.file_path)
            return State.WAITING

        self._check_conflicts()

        if current_mtime == self.last_mtime:
            return State.WAITING

        content, read_ok, skip_reason = self._safe_read()
        if not read_ok:
            return State.WAITING

        if skip_reason is not None:
            # Consume this file version: warn once, re-read only when the mtime changes.
            logger.warning(
                "File %s %s, skipping clipboard copy.", self.file_path, skip_reason
            )
            self.last_mtime = current_mtime
            return State.WAITING

        if not content:
            if current_mtime != self._last_empty_mtime:
                logger.debug(
                    "File %s is empty (cloud sync lock?), waiting...", self.file_path
                )
                self._last_empty_mtime = current_mtime
            return State.WAITING

        self._last_empty_mtime = None

        if content != self.last_file_content:
            self._pending_file_content = content
            self._pending_mtime = current_mtime
            return State.COPYING_FILE_TO_CLIP

        self.last_mtime = current_mtime
        logger.debug("File %s mtime changed but content is unchanged.", self.file_path)
        return State.WAITING

    def _write_clip_to_file(self) -> State:
        content = self._pending_clip
        self._pending_clip = ""
        if not content:
            return State.WAITING

        logger.info("Clipboard -> File (%d chars)", len(content))
        try:
            written_mtime = self._atomic_write(content)
            self.last_clip = content
            self.last_file_content = content
            self.last_mtime = written_mtime
        except OSError as e:
            logger.error("Write failed: %s", e)
        return State.WAITING

    def _copy_file_to_clip(self) -> State:
        content = self._pending_file_content
        mtime = self._pending_mtime
        self._pending_file_content = ""
        self._pending_mtime = None
        if not content:
            return State.WAITING

        error = self._safe_copy(content)
        if error is not None:
            # Caches stay untouched so the next poll retries; log loudly once per file version.
            if mtime != self._last_copy_fail_mtime:
                logger.error("Copy failed, will keep retrying: %s", error)
                self._last_copy_fail_mtime = mtime
            else:
                logger.debug("Copy failed again: %s", error)
            return State.WAITING

        logger.info("File -> Clipboard (%d chars)", len(content))
        self._last_copy_fail_mtime = None
        self.last_clip = content
        self._clip_ready = True
        self.last_file_content = content
        self.last_mtime = mtime
        self._oversized_clip_hash = None

        notify_msg = (
            "***" if self.hide_notification_content else compact_message(content)
        )
        self._show_notification("Synced to Clipboard", notify_msg, timeout=1)
        return State.WAITING


# ------------------------------------------------------------------
# Entry point
# ------------------------------------------------------------------


def main() -> None:
    parser = argparse.ArgumentParser(
        prog="clipsync",
        description=(
            "Bidirectional plain-text (UTF-8) clipboard sync bridging the system "
            "clipboard and a local text file."
        ),
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    parser.add_argument(
        "--file-path",
        type=Path,
        default=Path("./sync_clipboard.txt"),
        help="Path to the synced text file",
    )
    parser.add_argument(
        "--interval",
        type=float,
        default=DEFAULT_INTERVAL,
        help=f"Polling interval in seconds ({MIN_INTERVAL}..{int(MAX_INTERVAL)})",
    )
    parser.add_argument(
        "--no-notify",
        action="store_true",
        help="Disable desktop notifications",
    )
    parser.add_argument(
        "--hide-notify-content",
        action="store_true",
        help="Mask clipboard content as '***' in notifications",
    )
    parser.add_argument(
        "--log-level",
        type=str,
        default="info",
        help="Log verbosity: debug, info, warn, or error",
    )
    parser.add_argument(
        "--version",
        action="version",
        version=f"clipsync {__version__}",
    )
    args = parser.parse_args()

    level, level_ok = parse_log_level(args.log_level)
    logging.basicConfig(
        level=level,
        format="%(asctime)s [%(levelname)s] %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S",
    )
    if not level_ok:
        logger.warning(
            "Unknown log level %r (use debug, info, warn or error), "
            "defaulting to info.",
            args.log_level,
        )

    args.interval = validate_interval(args.interval)
    sync_file = args.file_path.resolve()

    syncer = ClipboardSync(
        file_path=sync_file,
        enable_notifications=not args.no_notify,
        hide_notification_content=args.hide_notify_content,
    )

    logger.info(
        "Sync started (v%s) — file: %s, interval: %ss, notify: %s. Press Ctrl+C to stop.",
        __version__,
        syncer.file_path,
        args.interval,
        "off" if args.no_notify else "on",
    )

    stop_event = threading.Event()
    signal.signal(signal.SIGINT, lambda *_: stop_event.set())
    signal.signal(signal.SIGTERM, lambda *_: stop_event.set())

    state = State.WAITING
    while not stop_event.is_set():
        t_start = time.monotonic()
        state = syncer.transition(state)
        if state == State.WAITING:
            sleep_for = args.interval - (time.monotonic() - t_start)
            if sleep_for > 0:
                stop_event.wait(sleep_for)

    logger.info("Stopped.")


if __name__ == "__main__":
    main()
