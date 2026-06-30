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
uv run main.py -f /path/to/file -i 1.0
uv run main.py --no-notify
uv run main.py --hide-notify-content
```

**Go (from pre-built binary):**
```bash
./clipsync_amd64_linux -f ./sync_clipboard.txt -i 0.5
```

**Cloud sync bridge** (rclone, runs alongside either implementation):
```bash
bash bisync.sh   # bisync against rclone remote "vcb", includes only sync_clipboard.txt
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
- Clipboard and file reads both return `""` on failure rather than stale content.
- Notifications fire in a background thread/goroutine to keep the poll loop non-blocking.
- Empty file content is treated as a cloud-sync lock artifact and skipped (state stays `WAITING`).

## Go dependencies

- `github.com/atotto/clipboard` — cross-platform clipboard R/W
- `github.com/gen2brain/beeep` — cross-platform desktop notifications

## Version

Both implementations track the same version constant (`__version__` in Python, `Version` in Go). Bump both together.
