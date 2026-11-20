# AGENTS.md

**autoclip** / **clipsync** — bidirectional clipboard sync bridging the system clipboard and a plain text file (`sync_clipboard.txt`) so cloud sync tools (e.g. `rclone`) can relay it across machines.

## Layout
- `main.py` / `test_main.py` — Python implementation (`uv`, Python >=3.14, `pyperclip`, `plyer`).
- `go/clipsync/` — parallel Go implementation (`atotto/clipboard`, `gen2brain/beeep`); `make build` compiles local binaries and updates `./clipsync_amd64_linux`.
- `bisync.sh` / `bisync_v2.sh` — `rclone bisync` bridges restricted to `/sync_clipboard.txt` (fixed poll vs. `inotifywait` directory watch + `stat` debounce).

## Commands
```bash
# Run (shared flags: --file-path, --interval, --log-level debug|info|warn|error, --no-notify, --hide-notify-content, --version)
uv run main.py
./clipsync_amd64_linux
bash bisync_v2.sh

# Build Go targets (macOS arm64, Windows amd64, Linux amd64 + atomic update of ./clipsync_amd64_linux)
cd go/clipsync && make build

# Test & lint
cd go/clipsync && go vet ./... && go test -v ./...
.venv/bin/python -m unittest test_main.py -v
ruff check . && ruff format --check . && pyrefly check
```

## Architecture & Invariants
Both implementations share a 3-state machine (`WAITING -> WRITING_CLIP_TO_FILE | COPYING_FILE_TO_CLIP -> WAITING`). Only `WAITING` sleeps for `--interval`; write/copy steps run immediately.

- **Verbatim clipboard**: read/write raw UTF-8 bytes without altering line endings (`\r\n`/`\n`) or whitespace; never modify `sync_clipboard.txt` during tests.
- **Retry & echo safety**: commit caches (`lastClip`, `lastMtime`, `lastFileContent`) only after I/O succeeds, with 3 retries on clipboard/rename locks. Capture `mtime` from the temp file before rename so concurrent remote writes aren't masked.
- **Atomic & private writes**: resolve symlinks first, write via unique `0600` temp file (`.<name>.*.tmp`, excluded from conflict/rclone filters), then atomically replace.
- **Defensive limits**: bound file reads to `5 MiB + 1` byte (no TOCTOU/OOM); skip oversized payloads without retaining them in RAM; skip empty clipboard/file writes; clamp `--interval` to `[0.05s, 3600s]` (reject `NaN`/`Inf`).
- **Non-blocking & quiet**: notifications use a single bounded worker (size 1); conflict scans check parent dir `mtime` first with prefix matching and log only on change.
- **Version parity**: keep `__version__` (`main.py`), `Version` (`go/clipsync/main.go`), and `version` (`pyproject.toml`) in sync.
