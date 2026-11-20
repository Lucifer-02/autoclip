# autoclip / clipsync

Cross-platform bidirectional clipboard sync for **Linux, macOS, and Windows**. It bridges your system clipboard and a plain text file (`sync_clipboard.txt`), letting any cloud sync tool (`rclone`, Syncthing, Dropbox, Google Drive, OneDrive, iCloud Drive, etc.) relay clipboard text seamlessly across machines and operating systems.

Ships with two feature-equivalent implementations:
- **Python** (`main.py`) — runs on Linux, macOS, and Windows via `uv` (Python >=3.14, `pyperclip`, `plyer`)
- **Go** (`go/clipsync/`) — compiles to standalone native binaries for Linux, macOS (Apple Silicon / Intel), and Windows (`atotto/clipboard`, `gen2brain/beeep`)

## Cross-Platform Support

| Platform | Go Binary (`make build`) | Python (`uv run main.py`) | Requirements / Notes |
| --- | --- | --- | --- |
| **Linux** | `clipsync_amd64_linux` | Supported | Requires `xclip` or `xsel` for clipboard access; `inotify-tools` optional for `bisync_v2.sh` |
| **macOS** | `clipsync_arm64_macos` | Supported | Uses built-in `pbcopy` / `pbpaste` (no extra clipboard deps) |
| **Windows** | `clipsync_amd64_windows.exe` | Supported | Native Win32 clipboard & toast notifications; retries transient clipboard/file locks |

> Mix and match freely — e.g. run the Go binary on Windows/macOS and Python (or Go) on Linux against the same synced `sync_clipboard.txt`. Exact UTF-8 bytes and line endings (`\r\n` / `\n`) are preserved across platforms.

## Features
- **Verbatim UTF-8 sync** — preserves exact bytes, whitespace, and line endings (`\r\n` / `\n`) without modification.
- **Echo-free & retry-safe** — commits state caches only after I/O succeeds, retrying transient clipboard/file locks automatically.
- **Atomic & private writes** — resolves symlinks, writes via unique `0600` temp files, and atomically replaces the target.
- **Defensive guards** — caps payloads at 5 MiB (bounded reads without OOM), skips empty clipboard writes (e.g. screenshots), clamps poll intervals to `[0.05s, 3600s]`, and runs desktop notifications on a single bounded worker.

## Quick Start

### Python (Linux / macOS / Windows)
```bash
uv sync
uv run main.py
```

### Go (Linux / macOS / Windows)
Build native binaries for all three platforms:
```bash
cd go/clipsync && make build
```

Then run the binary for your OS:
```bash
# Linux
./clipsync_amd64_linux

# macOS (Apple Silicon)
./go/clipsync/clipsync_arm64_macos

# Windows (PowerShell / CMD)
.\go\clipsync\clipsync_amd64_windows.exe
```

### Cloud Sync Bridge (`rclone`)
Any file sync client works by syncing the folder containing `sync_clipboard.txt`. For `rclone`, two helper scripts restricted to `/sync_clipboard.txt` are included:
```bash
# Event-driven (Linux, requires inotify-tools)
DRIVE=myremote bash bisync_v2.sh

# Portable fixed-interval polling (Linux / macOS / Git Bash on Windows)
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
