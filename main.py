import argparse
import json
import logging
import os
import shutil
import signal
import sys
import time
from pathlib import Path

# PyQt6 for Cross-Platform Event-Driven Clipboard (Text, Images, Files)
from PyQt6.QtCore import QMimeData, QObject, QTimer, QUrl, pyqtSignal
from PyQt6.QtGui import QGuiApplication, QImage

# Watchdog for Cross-Platform Event-Driven File Monitoring
from watchdog.events import FileSystemEventHandler
from watchdog.observers import Observer

__version__ = "1.0.0"


class WatcherSignals(QObject):
    """Bridge to send signals from watchdog's background thread to Qt's Main Thread."""

    meta_changed = pyqtSignal()


class SyncDirWatcher(FileSystemEventHandler):
    """Listens for file system changes triggered by Cloud Sync engines."""

    def __init__(self, meta_path: Path, signals: WatcherSignals):
        super().__init__()
        self.meta_path = meta_path.resolve()
        self.signals = signals

    def on_modified(self, event):
        if event.is_directory:
            return
        try:
            if Path(event.src_path).resolve() == self.meta_path:
                self.signals.meta_changed.emit()
        except Exception:
            pass

    def on_created(self, event):
        self.on_modified(event)


class ClipboardSyncer(QObject):
    def __init__(self, sync_dir: Path):
        super().__init__()
        self.sync_dir = sync_dir.resolve()
        self.meta_path = self.sync_dir / "meta.json"
        self.files_dir = self.sync_dir / "files"

        # Setup directories
        self.sync_dir.mkdir(parents=True, exist_ok=True)
        self.files_dir.mkdir(parents=True, exist_ok=True)

        # Initialize PyQt GUI Application (Required for OS clipboard access)
        self.app = QGuiApplication.instance() or QGuiApplication(sys.argv)
        self.app.setQuitOnLastWindowClosed(False)
        self.clipboard = self.app.clipboard()

        # States to prevent infinite echo loops
        self.last_timestamp = 0.0
        self.is_syncing_to_clipboard = False

        # 1. Setup Event-Driven File Monitoring (Watchdog)
        self.signals = WatcherSignals()
        self.signals.meta_changed.connect(self.sync_from_file_to_clipboard)

        self.observer = Observer()
        self.handler = SyncDirWatcher(self.meta_path, self.signals)
        self.observer.schedule(self.handler, str(self.sync_dir), recursive=False)
        self.observer.start()

        # 2. Setup Event-Driven Clipboard Monitoring (PyQt Signals)
        self.clipboard.dataChanged.connect(self.sync_from_clipboard_to_file)

        logging.info(f"Event-Driven Sync Started on: {self.sync_dir}")

        # Sync on startup if data exists
        if self.meta_path.exists():
            self.sync_from_file_to_clipboard()

    # ------------------------------------------------------------------
    # Helpers
    # ------------------------------------------------------------------

    def _clear_directory(self, dir_path: Path):
        """Empties the temporary files directory."""
        for item in dir_path.iterdir():
            try:
                if item.is_file():
                    item.unlink()
                elif item.is_dir():
                    shutil.rmtree(item, ignore_errors=True)
            except OSError:
                pass

    def _write_meta(self, meta: dict):
        """Atomic write ensures cloud-sync doesn't grab a half-written JSON."""
        tmp = self.meta_path.with_suffix(".tmp")
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(meta, f)
        os.replace(tmp, self.meta_path)

    def _read_meta(self) -> dict:
        """Safely read metadata, retrying if cloud sync has it temporarily locked."""
        for _ in range(3):
            try:
                with open(self.meta_path, "r", encoding="utf-8") as f:
                    return json.load(f)
            except (json.JSONDecodeError, OSError):
                time.sleep(0.1)
        return {}

    def _wait_for_file(self, path: Path, timeout: int = 15):
        """Pause to let cloud engines (Dropbox/Drive) finish downloading payload files."""
        start = time.time()
        while time.time() - start < timeout:
            if path.exists():
                try:
                    # Test if we can open it (ensures file is no longer locked by OS)
                    with open(path, "rb"):
                        return True
                except OSError:
                    pass
            # Keep Qt alive while we wait
            self.app.processEvents()
            time.sleep(0.2)
        raise TimeoutError(f"Cloud sync timeout waiting for {path}")

    def _unblock_clipboard(self):
        self.is_syncing_to_clipboard = False

    # ------------------------------------------------------------------
    # Event Callbacks
    # ------------------------------------------------------------------

    def sync_from_clipboard_to_file(self):
        """Triggered instantly by OS when User Copies something."""
        if self.is_syncing_to_clipboard:
            return  # Ignore events generated by ourselves

        mime = self.clipboard.mimeData()
        timestamp = time.time()
        meta = {"timestamp": timestamp, "type": "none", "data": None}

        try:
            # 1. Handle Copied Files
            if mime.hasUrls():
                local_files = [u.toLocalFile() for u in mime.urls() if u.isLocalFile()]
                if local_files:
                    self._clear_directory(self.files_dir)
                    copied_files = []
                    for f in local_files:
                        p = Path(f)
                        if p.exists() and p.is_file():
                            dest = self.files_dir / p.name
                            shutil.copy2(p, dest)
                            copied_files.append(p.name)
                    if copied_files:
                        meta["type"] = "files"
                        meta["data"] = copied_files

            # 2. Handle Copied Images (Screenshots, browser image copy, etc.)
            elif mime.hasImage():
                img = self.clipboard.image()
                if not img.isNull():
                    img_path = self.sync_dir / "clip.png"
                    img.save(str(img_path), "PNG")
                    meta["type"] = "image"
                    meta["data"] = "clip.png"

            # 3. Handle Text
            elif mime.hasText():
                txt = self.clipboard.text()
                if txt:
                    txt_path = self.sync_dir / "clip.txt"
                    txt_path.write_text(txt, encoding="utf-8")
                    meta["type"] = "text"
                    meta["data"] = "clip.txt"

            # Finalize Sync
            if meta["type"] != "none":
                self._write_meta(meta)
                self.last_timestamp = timestamp

                log_data = str(meta["data"])
                if meta["type"] == "files":
                    log_data = f"{len(meta['data'])} file(s)"
                elif meta["type"] == "text":
                    log_data = f"{len(txt)} chars"

                logging.info(f"Local Copy -> Cloud[{meta['type']}] ({log_data})")

        except Exception as e:
            logging.error(f"Failed to capture clipboard: {e}")

    def sync_from_file_to_clipboard(self):
        """Triggered instantly by Watchdog when Cloud updates meta.json."""
        meta = self._read_meta()
        if not meta:
            return

        timestamp = meta.get("timestamp", 0)
        # Debounce duplicate Watchdog events or ignore our own writes
        if timestamp <= self.last_timestamp:
            return

        self.last_timestamp = timestamp
        clip_type = meta.get("type")
        data = meta.get("data")

        self.is_syncing_to_clipboard = True
        try:
            mime_new = QMimeData()

            if clip_type == "text":
                txt_path = self.sync_dir / data
                self._wait_for_file(txt_path)
                mime_new.setText(txt_path.read_text(encoding="utf-8"))
                self.clipboard.setMimeData(mime_new)

            elif clip_type == "image":
                img_path = self.sync_dir / data
                self._wait_for_file(img_path)
                self.clipboard.setImage(QImage(str(img_path)))

            elif clip_type == "files":
                urls = []
                for fname in data:
                    fpath = self.files_dir / fname
                    self._wait_for_file(fpath)
                    urls.append(QUrl.fromLocalFile(str(fpath)))
                mime_new.setUrls(urls)
                self.clipboard.setMimeData(mime_new)

            logging.info(f"Cloud Sync -> Local Clipboard [{clip_type}]")

        except TimeoutError as te:
            logging.warning(te)
        except Exception as e:
            logging.error(f"Failed to sync to clipboard: {e}")
        finally:
            # Unblock clipboard events after Qt has processed them (prevents Echo loop)
            QTimer.singleShot(200, self._unblock_clipboard)

    def stop(self):
        self.observer.stop()
        self.observer.join()


def main():
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s[%(levelname)s] %(message)s",
        datefmt="%H:%M:%S",
    )

    parser = argparse.ArgumentParser(description="Rich Multi-format Clipboard Sync")
    parser.add_argument(
        "-d",
        "--dir",
        type=Path,
        default=Path("./sync_clipboard_dir"),
        help="Directory path used for syncing (e.g., inside Dropbox)",
    )
    args = parser.parse_args()

    # Qt manages the event loop, no 'while True:' required.
    app = QGuiApplication.instance() or QGuiApplication(sys.argv)
    syncer = ClipboardSyncer(sync_dir=args.dir)

    # Graceful exit handling for Ctrl+C
    def handle_sigint(sig, frame):
        logging.info("Shutting down...")
        syncer.stop()
        app.quit()

    signal.signal(signal.SIGINT, handle_sigint)
    signal.signal(signal.SIGTERM, handle_sigint)

    # Python + PyQt integration trick:
    # A dummy timer is required so Ctrl+C triggers properly in the console.
    timer = QTimer()
    timer.timeout.connect(lambda: None)
    timer.start(500)

    sys.exit(app.exec())


if __name__ == "__main__":
    main()
