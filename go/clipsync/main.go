package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/atotto/clipboard"
	"github.com/gen2brain/beeep"
)

const Version = "0.4.1"

// maxContentBytes caps how much clipboard/file content we are willing to sync.
// This protects disk, network and the OS from huge payloads (e.g. when an image
// lands on the clipboard and is read back as a large blob).
const maxContentBytes = 5 * 1024 * 1024 // 5 MiB

// minInterval is the smallest polling interval we accept. Anything lower turns
// the loop into a busy-wait that spawns clipboard subprocesses (xclip/xsel/etc.)
// hundreds of times per second and pins the CPU.
const minInterval = 0.05

// ------------------------------------------------------------------
// Logging (log/slog)
// ------------------------------------------------------------------

// parseLogLevel maps a flag string (case-insensitive) to a slog.Level. Unknown
// values return an error so the caller can warn and fall back to the default.
func parseLogLevel(s string) (slog.Level, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToUpper(strings.TrimSpace(s)))); err != nil {
		return slog.LevelInfo, fmt.Errorf("unknown log level %q (use debug, info, warn or error)", s)
	}
	return lvl, nil
}

// setupLogging installs a slog text handler on stderr that emits messages at or
// above the given level. The timestamp is shortened to a time-only format to
// keep the console output compact for an interactive CLI.
func setupLogging(level slog.Level) {
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				a.Value = slog.StringValue(a.Value.Time().Format("15:04:05"))
			}
			return a
		},
	})
	slog.SetDefault(slog.New(handler))
}

// State represents the states of the sync machine
type State int

const (
	Waiting State = iota
	WritingClipToFile
	CopyingFileToClip
)

// compactMessage truncates string for cleaner log/notification output
func compactMessage(content string, limit int) string {
	if content == "" {
		return ""
	}
	content = strings.TrimSpace(strings.ReplaceAll(content, "\n", " "))
	runes := []rune(content)
	if len(runes) > limit {
		return string(runes[:limit-3]) + "..."
	}
	return content
}

// ClipboardSync holds the state and configuration for the sync process
type ClipboardSync struct {
	filePath                string
	enableNotifications     bool
	hideNotificationContent bool
	dir                     string
	globPattern             string
	lastClip                string
	lastMtime               time.Time
	lastFileContent         string
}

func NewClipboardSync(path string, enableNotifications bool, hideNotificationContent bool) (*ClipboardSync, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	dir := filepath.Dir(absPath)
	fileName := filepath.Base(absPath)

	// Ensure file exists
	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		if err := os.WriteFile(absPath, []byte(""), 0644); err != nil {
			return nil, fmt.Errorf("could not create file %s: %v", absPath, err)
		}
	}

	sync := &ClipboardSync{
		filePath:                absPath,
		enableNotifications:     enableNotifications,
		hideNotificationContent: hideNotificationContent,
		dir:                     dir,
		globPattern:             fileName + "*",
	}

	sync.lastClip, _ = sync.safePaste()
	sync.lastMtime = sync.getMtime()
	sync.lastFileContent = sync.safeRead()

	slog.Info("Sync initialized", "file", sync.filePath)
	return sync, nil
}

// ------------------------------------------------------------------
// Helpers
// ------------------------------------------------------------------

func (c *ClipboardSync) showNotification(title, message string) {
	if !c.enableNotifications {
		return
	}
	// Run in a goroutine to avoid blocking the loop. Recover from panics inside
	// the notification backend so a flaky desktop environment can never crash
	// the whole sync daemon.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("Notification panic recovered", "err", r)
			}
		}()
		// beeep doesn't natively support custom timeouts, OS handles it
		if err := beeep.Notify(title, message, ""); err != nil {
			slog.Warn("Notification failed", "err", err)
		}
	}()
}

func (c *ClipboardSync) getMtime() time.Time {
	info, err := os.Stat(c.filePath)
	if err != nil {
		return time.Time{} // Returns zero value if error
	}
	return info.ModTime()
}

// safePaste reads the clipboard with retries. The second return value reports
// whether the read actually succeeded: callers MUST distinguish a real empty
// clipboard (text == "", ok == true) from a failed read (ok == false), because
// treating a failed read as an empty clipboard would overwrite the synced file
// with an empty string and wipe content on every other machine.
func (c *ClipboardSync) safePaste() (string, bool) {
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		text, err := clipboard.ReadAll()
		if err == nil {
			return text, true
		}
		if attempt < maxRetries-1 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		// On Windows, a clipboard held open by another app fails with a bogus
		// "The operation completed successfully." error (GetLastError == 0).
		if strings.Contains(err.Error(), "completed successfully") {
			slog.Debug("Clipboard locked by another app", "err", err)
		} else {
			slog.Debug("Clipboard access failed", "err", err)
		}
	}
	return "", false
}

func (c *ClipboardSync) safeRead() string {
	data, err := os.ReadFile(c.filePath)
	if err != nil {
		return ""
	}
	return string(data)
}

// atomicWrite writes content to a temp file in the same directory and renames
// it onto the target. The rename is atomic on POSIX (and Windows), so a cloud
// sync tool watching the file never observes a half-written or truncated state.
// The temp name is hidden and does NOT share the watched file's prefix, so it is
// neither flagged as a conflict file nor picked up by the rclone include filter.
func (c *ClipboardSync) atomicWrite(content string) error {
	tmp := filepath.Join(c.dir, "."+filepath.Base(c.filePath)+".tmp")
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.filePath); err != nil {
		os.Remove(tmp) // best-effort cleanup; ignore error
		return err
	}
	return nil
}

func (c *ClipboardSync) checkConflicts() {
	pattern := filepath.Join(c.dir, c.globPattern)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}

	var conflicts []string
	for _, match := range matches {
		if match != c.filePath {
			if info, err := os.Stat(match); err == nil && !info.IsDir() {
				conflicts = append(conflicts, filepath.Base(match))
			}
		}
	}

	if len(conflicts) > 0 {
		slog.Warn("Potential conflict files detected", "files", conflicts)
	}
}

// ------------------------------------------------------------------
// State machine
// ------------------------------------------------------------------

func (c *ClipboardSync) Transition(state State) State {
	switch state {
	case Waiting:
		// 1. Check clipboard change. Ignore failed reads (ok == false) so a
		// transient clipboard error can never be propagated as an empty write.
		currentClip, ok := c.safePaste()
		if ok && currentClip != c.lastClip {
			c.lastClip = currentClip
			return WritingClipToFile
		}

		// 2. File existence check
		if _, err := os.Stat(c.filePath); os.IsNotExist(err) {
			slog.Debug("File vanished (syncing?), waiting", "file", c.filePath)
			return Waiting
		}

		// 3. Conflict detection
		c.checkConflicts()

		// 4. Check file change by mtime
		currentMtime := c.getMtime()
		if currentMtime.Equal(c.lastMtime) {
			return Waiting
		}

		c.lastMtime = currentMtime

		// Single read — avoids TOCTOU race
		currentFileContent := c.safeRead()
		if currentFileContent == "" {
			slog.Debug("File is empty (cloud sync lock?), waiting", "file", c.filePath)
			return Waiting
		}

		if currentFileContent != c.lastFileContent {
			if len(currentFileContent) > maxContentBytes {
				slog.Warn("File content exceeds size limit, skipping clipboard copy", "bytes", len(currentFileContent), "limit", maxContentBytes)
				c.lastFileContent = currentFileContent // remember it to avoid repeating the warning
				return Waiting
			}
			c.lastFileContent = currentFileContent
			return CopyingFileToClip
		}

		slog.Debug("File mtime changed but content unchanged", "file", c.filePath)
		return Waiting

	case WritingClipToFile:
		if len(c.lastClip) > maxContentBytes {
			slog.Warn("Clipboard content exceeds size limit, skipping file write", "bytes", len(c.lastClip), "limit", maxContentBytes)
			return Waiting
		}
		slog.Info("Clipboard -> File", "chars", len([]rune(c.lastClip)))
		err := c.atomicWrite(c.lastClip)
		if err != nil {
			slog.Error("Write failed", "err", err)
		} else {
			// Update caches immediately to suppress self-triggered file-change detection
			c.lastFileContent = c.lastClip
			c.lastMtime = c.getMtime()
		}
		return Waiting

	case CopyingFileToClip:
		slog.Info("File -> Clipboard", "chars", len([]rune(c.lastFileContent)))
		err := clipboard.WriteAll(c.lastFileContent)
		if err != nil {
			slog.Error("Copy failed", "err", err)
		} else {
			// Update cache before notification to prevent echo
			c.lastClip = c.lastFileContent
			msg := compactMessage(c.lastFileContent, 64)
			if c.hideNotificationContent {
				msg = "***"
			}
			c.showNotification("Synced to Clipboard", msg)
		}
		return Waiting
	}

	return Waiting
}

// ------------------------------------------------------------------
// Entry point
// ------------------------------------------------------------------

func main() {
	// Argument parsing
	filePath := flag.String("file-path", "./sync_clipboard.txt", "Path to the file used for syncing")
	interval := flag.Float64("interval", 0.5, "Polling interval in seconds")
	noNotify := flag.Bool("no-notify", false, "Disable desktop notifications")
	hideNotifyContent := flag.Bool("hide-notify-content", false, "Hide clipboard content in desktop notifications")
	logLevelFlag := flag.String("log-level", "info", "Log verbosity: debug, info, warn or error")
	versionFlag := flag.Bool("version", false, "Show version")

	flag.Parse()

	if *versionFlag {
		fmt.Printf("clipboard-sync %s\n", Version)
		os.Exit(0)
	}

	// Configure logging before emitting any messages. An invalid level falls back
	// to INFO and is reported once logging is up.
	level, levelErr := parseLogLevel(*logLevelFlag)
	setupLogging(level)
	if levelErr != nil {
		slog.Warn("Invalid log level, defaulting to info", "err", levelErr)
	}

	if *interval < minInterval {
		slog.Warn("Interval too small, clamping", "requested", *interval, "min", minInterval)
		*interval = minInterval
	}

	syncFile, err := filepath.Abs(*filePath)
	if err != nil {
		slog.Error("Failed to resolve path", "err", err)
		os.Exit(1)
	}

	slog.Info("Sync starting", "version", Version, "interval", *interval, "file", syncFile)

	syncer, err := NewClipboardSync(syncFile, !*noNotify, *hideNotifyContent)
	if err != nil {
		slog.Error("Initialization failed", "err", err)
		os.Exit(1)
	}

	// Graceful shutdown on SIGTERM / SIGINT (Ctrl+C). We signal via a channel and
	// only act on it between transitions, so the process never exits in the
	// middle of a file write.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	state := Waiting
	intervalDuration := time.Duration(*interval * float64(time.Second))

	for {
		select {
		case <-sigChan:
			slog.Info("Stopped")
			return
		default:
		}

		// Record start time to keep interval drift-free
		tStart := time.Now()

		state = syncer.Transition(state)

		elapsed := time.Since(tStart)
		sleepFor := intervalDuration - elapsed

		if sleepFor > 0 {
			// Sleep, but wake up immediately on a shutdown signal.
			select {
			case <-sigChan:
				slog.Info("Stopped")
				return
			case <-time.After(sleepFor):
			}
		}
	}
}
