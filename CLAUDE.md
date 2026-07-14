# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**autoclip** / **clipsync** — a bidirectional clipboard sync tool. It bridges the system clipboard and a plain text file (`sync_clipboard.txt`), so cloud sync tools (e.g. rclone) can relay clipboard content across machines.

The project has two parallel implementations with identical feature sets:
- **Python** (`main.py`) — uses `uv`, `pyperclip`, `plyer`
- **Go** (`go/clipsync/`) — pre-built binaries included for Linux/macOS/Windows

## Running

**Python:**
```bash
uv run main.py                          # default: ./sync_clipboard.txt, 0.5s interval
uv run main.py --file-path /path/to/file --interval 1.0
uv run main.py --no-notify
uv run main.py --hide-notify-content
uv run main.py --version
```

**Go (from pre-built binary):**
```bash
./clipsync_amd64_linux --file-path ./sync_clipboard.txt --interval 0.5
./clipsync_amd64_linux --log-level debug   # debug|info|warn|error, default info
./clipsync_amd64_linux --version
```

**Cloud sync bridge** (rclone, runs alongside either implementation). Two variants, both restricted to `sync_clipboard.txt` via `--include`:
```bash
bash bisync.sh      # simple: fixed poll loop (sleep 0.4s) between bisync runs
bash bisync_v2.sh   # event-driven: inotifywait on the local file (debounced) triggers an
                     # immediate bisync, falling back to a POLL_INTERVAL timeout for
                     # remote-side changes; retries on failure after RETRY_INTERVAL
```

## Building (Go)

```bash
cd go/clipsync
make build       # compiles all three targets: arm64/macOS, amd64/Windows, amd64/Linux
```

Individual build:
```bash
GOOS=linux GOARCH=amd64 go build -o clipsync_amd64_linux main.go
```

## Python dependency management

Uses `uv` (not pip/poetry). Requires Python >=3.14.

```bash
uv sync          # install deps from uv.lock
uv add <pkg>     # add a dependency
```

## Architecture: state machine

Both implementations share the same event-driven state machine with three states:

```
WAITING ──(clipboard changed)──► WRITING_CLIP_TO_FILE ──► WAITING
WAITING ──(file mtime changed)──► COPYING_FILE_TO_CLIP ──► WAITING
```

**Echo-loop prevention**: after each write, the caches (`lastClip`, `lastMtime`, `lastFileContent`) are updated immediately so the next poll doesn't re-trigger the opposite direction.

**Key design invariants:**
- File mtime is checked first (cheap); content is read only when mtime differs — avoids TOCTOU race by doing a single read (no separate size check).
- Clipboard and file reads both return `""` on failure rather than stale content; a failed clipboard read is distinguished from a genuinely empty clipboard (`(text, ok)` / `(string, bool)` return) so a transient read error is never written to the file as an empty string.
- File writes go through a temp-file-plus-atomic-rename (`.<filename>.tmp` -> rename), so a cloud sync tool never observes a half-written file. The temp name deliberately doesn't share the watched file's prefix, so it's excluded from the rclone `--include` filter and not flagged as a sync conflict.
- Content is capped at `MAX_CONTENT_BYTES` / `maxContentBytes` (5 MiB); oversized content is skipped (not truncated) and logged as a warning.
- Poll interval is floored at `MIN_INTERVAL` / `minInterval` (0.05s) to prevent a busy-wait loop.
- Notifications fire in a background thread/goroutine to keep the poll loop non-blocking.
- Empty file content is treated as a cloud-sync lock artifact and skipped (state stays `WAITING`).

## Go dependencies

- `github.com/atotto/clipboard` — cross-platform clipboard R/W
- `github.com/gen2brain/beeep` — cross-platform desktop notifications

## Version

Both implementations track the same version constant (`__version__` in Python, `Version` in Go). Bump both together.

## Testing

There is no automated test suite for either implementation. Verify changes by running the tool manually (see Running) and checking the log output / notifications.
