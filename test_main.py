import importlib.util
import math
import os
import stat
import sys
import tempfile
import types
import unittest
from pathlib import Path

# Allow running tests with stdlib python3 when pyperclip/plyer are only in .venv.
if importlib.util.find_spec("pyperclip") is None:

    class _FakePyperclip(types.ModuleType):
        @staticmethod
        def paste() -> str:
            return ""

        @staticmethod
        def copy(_text: str) -> None:
            return None

    sys.modules["pyperclip"] = _FakePyperclip("pyperclip")

if importlib.util.find_spec("plyer") is None:

    class _FakePlyer(types.ModuleType):
        notification = types.SimpleNamespace(notify=lambda **_kwargs: None)

    sys.modules["plyer"] = _FakePlyer("plyer")

import main as autoclip


class FakeClipboard:
    def __init__(self, text: str = "") -> None:
        self.text = text
        self.read_error: Exception | None = None
        self.write_error: Exception | None = None

    def paste(self) -> str:
        if self.read_error is not None:
            raise self.read_error
        return self.text

    def copy(self, text: str) -> None:
        if self.write_error is not None:
            raise self.write_error
        self.text = text


class TestAutoClip(unittest.TestCase):
    def test_compact_message(self) -> None:
        self.assertEqual(autoclip.compact_message("a\r\nb\rc\nd", 64), "a b c d")
        self.assertEqual(autoclip.compact_message("abcdef", 2), "ab")
        self.assertEqual(autoclip.compact_message("abcdef", 0), "")
        self.assertEqual(autoclip.compact_message("abcdef", 5), "ab...")

    def test_validate_interval(self) -> None:
        self.assertEqual(
            autoclip.validate_interval(float("nan")), autoclip.DEFAULT_INTERVAL
        )
        self.assertEqual(
            autoclip.validate_interval(float("inf")), autoclip.DEFAULT_INTERVAL
        )
        self.assertEqual(autoclip.validate_interval(-0.1), autoclip.MIN_INTERVAL)
        self.assertEqual(autoclip.validate_interval(999999.0), autoclip.MAX_INTERVAL)
        self.assertTrue(math.isclose(autoclip.validate_interval(1.25), 1.25))

    def test_parse_log_level(self) -> None:
        lvl, ok = autoclip.parse_log_level("debug")
        self.assertTrue(ok)
        self.assertEqual(lvl, 10)
        lvl, ok = autoclip.parse_log_level("WARN")
        self.assertTrue(ok)
        self.assertEqual(lvl, 30)
        lvl, ok = autoclip.parse_log_level("bogus")
        self.assertFalse(ok)
        self.assertEqual(lvl, 20)

    def test_verbatim_bytes_preserved_and_permissions_0600(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            sync_file = Path(tmp) / "sync_clipboard.txt"
            clip = FakeClipboard("init")
            s = autoclip.ClipboardSync(
                sync_file,
                enable_notifications=False,
                paste_fn=clip.paste,
                copy_fn=clip.copy,
            )

            clip.text = "  line1\r\nline2\rline3\n  "
            state = s.transition(autoclip.State.WAITING)
            self.assertEqual(state, autoclip.State.WRITING_CLIP_TO_FILE)
            state = s.transition(state)
            self.assertEqual(state, autoclip.State.WAITING)

            self.assertEqual(sync_file.read_bytes(), b"  line1\r\nline2\rline3\n  ")
            mode = stat.S_IMODE(sync_file.stat().st_mode)
            self.assertEqual(mode, 0o600)

    def test_transient_copy_error_retries_next_cycle(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            sync_file = Path(tmp) / "sync_clipboard.txt"
            sync_file.write_bytes(b"initial")
            clip = FakeClipboard("initial")
            s = autoclip.ClipboardSync(
                sync_file,
                enable_notifications=False,
                paste_fn=clip.paste,
                copy_fn=clip.copy,
            )

            sync_file.write_bytes(b"  from-remote\r\nline2  ")
            future = sync_file.stat().st_mtime + 2.0
            os.utime(sync_file, (future, future))

            clip.write_error = RuntimeError("clipboard locked")
            state = s.transition(autoclip.State.WAITING)
            self.assertEqual(state, autoclip.State.COPYING_FILE_TO_CLIP)
            state = s.transition(state)
            self.assertEqual(state, autoclip.State.WAITING)

            clip.write_error = None
            state = s.transition(autoclip.State.WAITING)
            self.assertEqual(state, autoclip.State.COPYING_FILE_TO_CLIP)
            s.transition(state)
            self.assertEqual(clip.text, "  from-remote\r\nline2  ")

    def test_empty_clipboard_does_not_wipe_file(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            sync_file = Path(tmp) / "sync_clipboard.txt"
            sync_file.write_bytes(b"keep-me")
            clip = FakeClipboard("keep-me")
            s = autoclip.ClipboardSync(
                sync_file,
                enable_notifications=False,
                paste_fn=clip.paste,
                copy_fn=clip.copy,
            )

            clip.text = ""
            state = s.transition(autoclip.State.WAITING)
            self.assertEqual(state, autoclip.State.WAITING)
            self.assertEqual(sync_file.read_bytes(), b"keep-me")

    def test_init_clipboard_failure_does_not_clobber_file(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            sync_file = Path(tmp) / "sync_clipboard.txt"
            sync_file.write_bytes(b"remote-content")
            clip = FakeClipboard("old-local-clip")
            clip.read_error = RuntimeError("not ready")
            s = autoclip.ClipboardSync(
                sync_file,
                enable_notifications=False,
                paste_fn=clip.paste,
                copy_fn=clip.copy,
            )

            clip.read_error = None
            state = s.transition(autoclip.State.WAITING)
            self.assertEqual(state, autoclip.State.WAITING)
            self.assertEqual(sync_file.read_bytes(), b"remote-content")

    def test_oversized_file_skipped_without_retaining(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            sync_file = Path(tmp) / "sync_clipboard.txt"
            sync_file.write_bytes(b"ok")
            clip = FakeClipboard("ok")
            s = autoclip.ClipboardSync(
                sync_file,
                enable_notifications=False,
                paste_fn=clip.paste,
                copy_fn=clip.copy,
            )

            sync_file.write_bytes(b"A" * (autoclip.MAX_CONTENT_BYTES + 10))
            future = sync_file.stat().st_mtime + 2.0
            os.utime(sync_file, (future, future))

            state = s.transition(autoclip.State.WAITING)
            self.assertEqual(state, autoclip.State.WAITING)
            self.assertLessEqual(len(s.last_file_content), autoclip.MAX_CONTENT_BYTES)

    def test_conflict_check_handles_glob_metacharacters(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            subdir = Path(tmp) / "dir[1]"
            subdir.mkdir()
            sync_file = subdir / "clip[0].txt"
            conflict_file = subdir / "clip[0].txt.conflict1"
            sync_file.write_bytes(b"ok")
            conflict_file.write_bytes(b"dup")

            clip = FakeClipboard("ok")
            s = autoclip.ClipboardSync(
                sync_file,
                enable_notifications=False,
                paste_fn=clip.paste,
                copy_fn=clip.copy,
            )
            s._check_conflicts()
            self.assertEqual(s._last_conflicts, ("clip[0].txt.conflict1",))


if __name__ == "__main__":
    unittest.main()
