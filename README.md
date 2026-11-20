# autoclip / clipsync

Lightweight bidirectional clipboard sync that bridges your system clipboard and a plain text file (`sync_clipboard.txt`), letting cloud sync tools like `rclone` relay clipboard content across Linux, macOS, and Windows.

Ships with two feature-equivalent implementations:
- **Python** (`main.py`) — managed with `uv` (Python >=3.14, `pyperclip`, `plyer`)
- **Go** (`go/clipsync/`) — single static binary (`atotto/clipboard`, `gen2brain/beeep`)

## Features
- **Verbatim UTF-8 sync** — preserves exact bytes, whitespace, and line endings (`\r\n` / `\n`) without modification.
- **Echo-free & retry-safe** — commits state caches only after I/O succeeds, retrying transient clipboard/file locks automatically.
- **Atomic & private writes** — resolves symlinks, writes via unique `0600` temp files, and atomically replaces the target.
- **Defensive guards** — caps payloads at 5 MiB (bounded reads without OOM), skips empty clipboard writes (e.g. screenshots), clamps poll intervals to `[0.05s, 3600s]`, and runs desktop notifications on a single bounded worker.

## Quick Start

### Python
```bash
uv sync
uv run main.py
```

### Go
```bash
cd go/clipsync && make build
cd ../.. && ./clipsync_amd64_linux
```

### Cloud Sync Bridge (`rclone`)
Run alongside either implementation to sync `/sync_clipboard.txt` with a remote:
```bash
# Event-driven (Linux, requires inotify-tools)
DRIVE=myremote bash bisync_v2.sh

# Simple fixed-interval polling
DRIVE=myremote bash bisync.sh
```

Environment variables supported by `bisync_v2.sh` / `bisync.sh`:
- `DRIVE` — `rclone` remote name (default: `vcb`)
- `LOCAL_PATH` — directory containing `sync_clipboard.txt` (default: script directory)
- `POLL_INTERVAL`, `RETRY_INTERVAL`, `DEBOUNCE` — polling/retry/debounce timings

## CLI Options (Python & Go)

| Flag | Default | Description |
| --- | --- | --- |
| `--file-path` | `./sync_clipboard.txt` | Path to the synced text file |
| `--interval` | `0.5` | Poll interval in seconds (clamped to `0.05..3600`) |
| `--log-level` | `info` | `debug`, `info`, `warn`, or `error` |
| `--no-notify` | `false` | Disable desktop notifications |
| `--hide-notify-content` | `false` | Mask clipboard preview as `***` in notifications |
| `--version` | — | Print version and exit |

## Development

```bash
# Go tests & vet
cd go/clipsync && go vet ./... && go test -v ./...

# Python tests, lint, format & type check
.venv/bin/python -m unittest test_main.py -v
ruff check . && ruff format --check . && pyrefly check
```
