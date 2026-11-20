# autoclip / clipsync

Cross-platform (**Linux, macOS, Windows**) bidirectional **plain-text clipboard sync**. It bridges the system clipboard with a local UTF-8 file (`sync_clipboard.txt`) so any cloud sync tool (`rclone`, Syncthing, Dropbox, Google Drive, OneDrive, iCloud Drive) can relay text across machines.

> **Plain text (UTF-8) only** — images, files, and rich formatting are not synced. Non-text or empty clipboard items (e.g. screenshots) are skipped safely without wiping synced text.

## Highlights
- **Go (production) & Python (prototype)**: **Go** (`go/clipsync/`) is the recommended production implementation compiled to standalone native binaries; **Python** (`main.py`) serves as the reference prototype. Both share identical behavior and can be mixed across Linux (`xclip`/`xsel`), macOS (`pbcopy`/`pbpaste`), and Windows (native Win32).
- **Verbatim & safe**: preserves exact UTF-8 bytes and line endings (`\r\n` / `\n`), writes atomically via `0600` temp files (preserving symlinks), retries transient locks, and caps payloads at **5 MiB**.

## Quick Start

**Go — Production (Linux / macOS / Windows):**
```bash
cd go/clipsync && make build
# Linux:   ./clipsync_amd64_linux
# macOS:   ./go/clipsync/clipsync_arm64_macos
# Windows: .\go\clipsync\clipsync_amd64_windows.exe
```

**Python — Prototype (Linux / macOS / Windows):**
```bash
uv sync && uv run main.py
```

**Optional `rclone` sync bridge** (or use any file sync client on the folder):
```bash
DRIVE=myremote bash bisync.sh      # Linux / macOS / Windows Git Bash
```

## CLI Flags (Python & Go)

| Flag | Default | Description |
| --- | --- | --- |
| `--file-path` | `./sync_clipboard.txt` | Synced text file path |
| `--interval` | `0.5` | Poll interval in seconds (`0.05..3600`) |
| `--log-level` | `info` | `debug`, `info`, `warn`, or `error` |
| `--no-notify` | `false` | Disable desktop notifications |
| `--hide-notify-content` | `false` | Mask notification preview as `***` |
| `--version` | — | Print version and exit |

## Development

```bash
cd go/clipsync && go vet ./... && go test -v ./...
.venv/bin/python -m unittest test_main.py -v
ruff check . && ruff format --check . && pyrefly check
```
