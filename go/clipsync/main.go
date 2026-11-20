package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/atotto/clipboard"
	"github.com/gen2brain/beeep"
)

const Version = "0.5.0"

const (
	maxContentBytes = 5 * 1024 * 1024 // 5 MiB
	minInterval     = 0.05
	maxInterval     = 3600.0
	defaultInterval = 0.5
	ioRetries       = 3
	retryDelay      = 50 * time.Millisecond
)

// ------------------------------------------------------------------
// Logging & CLI helpers
// ------------------------------------------------------------------

func parseLogLevel(s string) (slog.Level, error) {
	key := strings.ToUpper(strings.TrimSpace(s))
	if key == "WARNING" {
		key = "WARN"
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(key)); err != nil {
		return slog.LevelInfo, fmt.Errorf("unknown log level %q (use debug, info, warn or error)", s)
	}
	return lvl, nil
}

func setupLogging(level slog.Level) {
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				a.Value = slog.StringValue(a.Value.Time().Format("2006-01-02 15:04:05"))
			}
			return a
		},
	})
	slog.SetDefault(slog.New(handler))
}

func validateInterval(interval float64) float64 {
	if math.IsNaN(interval) || math.IsInf(interval, 0) {
		slog.Warn("Interval is not finite, using default", "requested", interval, "default", defaultInterval)
		return defaultInterval
	}
	if interval < minInterval {
		slog.Warn("Interval too small, clamping", "requested", interval, "min", minInterval)
		return minInterval
	}
	if interval > maxInterval {
		slog.Warn("Interval too large, clamping", "requested", interval, "max", maxInterval)
		return maxInterval
	}
	return interval
}

// compactMessage formats a short preview for desktop notifications (never alters clipboard).
func compactMessage(content string, limit int) string {
	if content == "" || limit <= 0 {
		return ""
	}
	preview := strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(content)
	preview = strings.TrimSpace(preview)
	runes := []rune(preview)
	if len(runes) <= limit {
		return preview
	}
	if limit <= 3 {
		return string(runes[:limit])
	}
	return string(runes[:limit-3]) + "..."
}

// resolveFilePath resolves symlinks (including dangling links and symlinked
// parent dirs) so atomic renames never destroy a symlink.
func resolveFilePath(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		return resolved, nil
	}
	if info, err := os.Lstat(absPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if target, err := os.Readlink(absPath); err == nil {
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(absPath), target)
			}
			absPath = filepath.Clean(target)
		}
	}
	if resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(absPath)); err == nil {
		return filepath.Join(resolvedDir, filepath.Base(absPath)), nil
	}
	return absPath, nil
}

// ------------------------------------------------------------------
// State machine & ClipboardSync
// ------------------------------------------------------------------

type State int

const (
	Waiting State = iota
	WritingClipToFile
	CopyingFileToClip
)

type notifyPayload struct {
	title   string
	message string
}

type ioBackends struct {
	readClip  func() (string, error)
	writeClip func(string) error
	notify    func(title, message string, icon any) error
}

func defaultBackends() ioBackends {
	return ioBackends{
		readClip:  clipboard.ReadAll,
		writeClip: clipboard.WriteAll,
		notify:    beeep.Notify,
	}
}

// ClipboardSync bridges clipboard and file while preserving exact bytes verbatim.
type ClipboardSync struct {
	filePath                string
	dir                     string
	fileName                string
	enableNotifications     bool
	hideNotificationContent bool

	io       ioBackends
	notifyCh chan notifyPayload

	// Sync caches (updated only after reads/writes succeed).
	clipReady         bool
	lastClip          string
	oversizedClipHash [sha256.Size]byte
	lastMtime         time.Time
	lastFileContent   string

	// Pending payloads waiting for the next state step.
	pendingClip        string
	pendingFileContent string
	pendingMtime       time.Time

	// Log/scan deduplication state.
	lastDirMtime   time.Time
	lastConflicts  []string
	lastEmptyMtime time.Time
}

func NewClipboardSync(path string, enableNotifications, hideNotificationContent bool) (*ClipboardSync, error) {
	return newClipboardSync(path, enableNotifications, hideNotificationContent, defaultBackends())
}

func newClipboardSync(path string, enableNotifications, hideNotificationContent bool, io ioBackends) (*ClipboardSync, error) {
	resolvedPath, err := resolveFilePath(path)
	if err != nil {
		return nil, err
	}
	if err := ensureFileExists(resolvedPath); err != nil {
		return nil, err
	}

	c := &ClipboardSync{
		filePath:                resolvedPath,
		dir:                     filepath.Dir(resolvedPath),
		fileName:                filepath.Base(resolvedPath),
		enableNotifications:     enableNotifications,
		hideNotificationContent: hideNotificationContent,
		io:                      io,
		notifyCh:                make(chan notifyPayload, 1),
	}

	if enableNotifications {
		go c.notificationWorker()
	}

	// Seed initial caches without modifying either clipboard or file.
	if clip, ok := c.safePaste(); ok {
		c.initClip(clip)
	}
	if mtime, exists := c.getMtime(); exists {
		if content, readOK, tooLarge := c.safeRead(); readOK {
			c.lastMtime = mtime
			if !tooLarge {
				c.lastFileContent = content
			}
		}
	}

	return c, nil
}

func ensureFileExists(path string) error {
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("could not create file %s: %w", path, err)
	}
	return f.Close()
}

func (c *ClipboardSync) initClip(clip string) {
	if len(clip) > maxContentBytes {
		c.oversizedClipHash = sha256.Sum256([]byte(clip))
		c.lastClip = ""
	} else {
		c.lastClip = clip
	}
	c.clipReady = true
}

// ------------------------------------------------------------------
// Low-level I/O helpers (preserve exact bytes, never alter content)
// ------------------------------------------------------------------

func (c *ClipboardSync) notificationWorker() {
	for p := range c.notifyCh {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Warn("Notification panic recovered", "err", r)
				}
			}()
			if err := c.io.notify(p.title, p.message, ""); err != nil {
				slog.Warn("Notification failed", "err", err)
			}
		}()
	}
}

func (c *ClipboardSync) showNotification(title, message string) {
	if !c.enableNotifications {
		return
	}
	select {
	case <-c.notifyCh:
	default:
	}
	select {
	case c.notifyCh <- notifyPayload{title: title, message: message}:
	default:
	}
}

func (c *ClipboardSync) getMtime() (time.Time, bool) {
	info, err := os.Stat(c.filePath)
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

func (c *ClipboardSync) safePaste() (string, bool) {
	for attempt := 0; attempt < ioRetries; attempt++ {
		text, err := c.io.readClip()
		if err == nil {
			return text, true
		}
		if attempt < ioRetries-1 {
			time.Sleep(retryDelay)
			continue
		}
		if strings.Contains(err.Error(), "completed successfully") {
			slog.Debug("Clipboard locked by another app", "err", err)
		} else {
			slog.Debug("Clipboard access failed", "err", err)
		}
	}
	return "", false
}

func (c *ClipboardSync) safeCopy(content string) bool {
	for attempt := 0; attempt < ioRetries; attempt++ {
		err := c.io.writeClip(content)
		if err == nil {
			return true
		}
		if attempt < ioRetries-1 {
			time.Sleep(retryDelay)
			continue
		}
		slog.Error("Copy failed", "err", err)
	}
	return false
}

// safeRead reads at most maxContentBytes+1 raw bytes in a single open/read pass.
func (c *ClipboardSync) safeRead() (content string, ok bool, tooLarge bool) {
	f, err := os.Open(c.filePath)
	if err != nil {
		slog.Debug("File read failed", "file", c.filePath, "err", err)
		return "", false, false
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxContentBytes+1))
	if err != nil {
		slog.Debug("File read failed", "file", c.filePath, "err", err)
		return "", false, false
	}
	if len(data) > maxContentBytes {
		return "", true, true
	}
	return string(data), true, false
}

// atomicWrite writes exact bytes via a unique 0600 temp file + atomic rename
// and returns the temp file's mtime captured right before rename.
func (c *ClipboardSync) atomicWrite(content string) (time.Time, error) {
	f, err := os.CreateTemp(c.dir, "."+c.fileName+".*.tmp")
	if err != nil {
		return time.Time{}, err
	}
	tmp := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()

	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return time.Time{}, err
	}
	if err := f.Close(); err != nil {
		return time.Time{}, err
	}
	info, err := os.Stat(tmp)
	if err != nil {
		return time.Time{}, err
	}

	var renameErr error
	for attempt := 0; attempt < ioRetries; attempt++ {
		if renameErr = os.Rename(tmp, c.filePath); renameErr == nil {
			cleanup = false
			return info.ModTime(), nil
		}
		if attempt < ioRetries-1 {
			time.Sleep(retryDelay)
		}
	}
	return time.Time{}, renameErr
}

func (c *ClipboardSync) checkConflicts() {
	dirInfo, err := os.Stat(c.dir)
	if err != nil || dirInfo.ModTime().Equal(c.lastDirMtime) {
		return
	}
	c.lastDirMtime = dirInfo.ModTime()

	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}

	var conflicts []string
	for _, entry := range entries {
		name := entry.Name()
		if name == c.fileName || !strings.HasPrefix(name, c.fileName) {
			continue
		}
		if entry.Type().IsRegular() {
			conflicts = append(conflicts, name)
		} else if info, err := entry.Info(); err == nil && !info.IsDir() {
			conflicts = append(conflicts, name)
		}
	}
	sort.Strings(conflicts)

	if !slices.Equal(conflicts, c.lastConflicts) {
		c.lastConflicts = conflicts
		if len(conflicts) > 0 {
			slog.Warn("Potential conflict files detected", "files", conflicts)
		} else {
			slog.Info("Conflict files cleared")
		}
	}
}

// ------------------------------------------------------------------
// State machine transitions
// ------------------------------------------------------------------

func (c *ClipboardSync) Transition(state State) State {
	switch state {
	case Waiting:
		return c.poll()
	case WritingClipToFile:
		return c.writeClipToFile()
	case CopyingFileToClip:
		return c.copyFileToClip()
	default:
		return Waiting
	}
}

func (c *ClipboardSync) poll() State {
	if next := c.checkClipboard(); next != Waiting {
		return next
	}
	return c.checkFile()
}

func (c *ClipboardSync) checkClipboard() State {
	currentClip, ok := c.safePaste()
	if !ok {
		return Waiting
	}
	if !c.clipReady {
		c.initClip(currentClip)
		return Waiting
	}
	if currentClip == c.lastClip {
		return Waiting
	}
	if currentClip == "" {
		slog.Debug("Clipboard is empty or non-text, skipping file write")
		c.lastClip = ""
		c.oversizedClipHash = [sha256.Size]byte{}
		return Waiting
	}
	if len(currentClip) > maxContentBytes {
		digest := sha256.Sum256([]byte(currentClip))
		if digest != c.oversizedClipHash {
			slog.Warn("Clipboard content exceeds size limit, skipping file write", "bytes", len(currentClip), "limit", maxContentBytes)
			c.oversizedClipHash = digest
		}
		return Waiting
	}

	c.oversizedClipHash = [sha256.Size]byte{}
	c.pendingClip = currentClip
	return WritingClipToFile
}

func (c *ClipboardSync) checkFile() State {
	currentMtime, exists := c.getMtime()
	if !exists {
		slog.Debug("File vanished (syncing?), waiting", "file", c.filePath)
		return Waiting
	}

	c.checkConflicts()

	if !c.lastMtime.IsZero() && currentMtime.Equal(c.lastMtime) {
		return Waiting
	}

	content, readOK, tooLarge := c.safeRead()
	if !readOK {
		return Waiting
	}
	if tooLarge {
		slog.Warn("File content exceeds size limit, skipping clipboard copy", "limit", maxContentBytes)
		c.lastMtime = currentMtime
		return Waiting
	}
	if content == "" {
		if !currentMtime.Equal(c.lastEmptyMtime) {
			slog.Debug("File is empty (cloud sync lock?), waiting", "file", c.filePath)
			c.lastEmptyMtime = currentMtime
		}
		return Waiting
	}
	c.lastEmptyMtime = time.Time{}

	if content != c.lastFileContent {
		c.pendingFileContent = content
		c.pendingMtime = currentMtime
		return CopyingFileToClip
	}

	c.lastMtime = currentMtime
	slog.Debug("File mtime changed but content unchanged", "file", c.filePath)
	return Waiting
}

func (c *ClipboardSync) writeClipToFile() State {
	content := c.pendingClip
	c.pendingClip = ""
	if content == "" {
		return Waiting
	}

	slog.Info("Clipboard -> File", "chars", len([]rune(content)))
	writtenMtime, err := c.atomicWrite(content)
	if err != nil {
		slog.Error("Write failed", "err", err)
		return Waiting
	}
	c.lastClip = content
	c.lastFileContent = content
	c.lastMtime = writtenMtime
	return Waiting
}

func (c *ClipboardSync) copyFileToClip() State {
	content := c.pendingFileContent
	mtime := c.pendingMtime
	c.pendingFileContent = ""
	c.pendingMtime = time.Time{}
	if content == "" {
		return Waiting
	}

	slog.Info("File -> Clipboard", "chars", len([]rune(content)))
	if c.safeCopy(content) {
		c.lastClip = content
		c.clipReady = true
		c.lastFileContent = content
		c.lastMtime = mtime
		c.oversizedClipHash = [sha256.Size]byte{}

		msg := compactMessage(content, 64)
		if c.hideNotificationContent {
			msg = "***"
		}
		c.showNotification("Synced to Clipboard", msg)
	}
	return Waiting
}

// ------------------------------------------------------------------
// Entry point
// ------------------------------------------------------------------

func main() {
	filePath := flag.String("file-path", "./sync_clipboard.txt", "Path to the synced text file")
	interval := flag.Float64("interval", defaultInterval, fmt.Sprintf("Polling interval in seconds (%.2f..%.0f)", minInterval, maxInterval))
	noNotify := flag.Bool("no-notify", false, "Disable desktop notifications")
	hideNotifyContent := flag.Bool("hide-notify-content", false, "Mask clipboard content as '***' in notifications")
	logLevelFlag := flag.String("log-level", "info", "Log verbosity: debug, info, warn, or error")
	versionFlag := flag.Bool("version", false, "Show version and exit")

	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "clipsync %s — bidirectional plain-text (UTF-8) clipboard sync.\n\n", Version)
		fmt.Fprintf(out, "Usage: %s [options]\n\nOptions:\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}

	flag.Parse()

	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "Error: unexpected arguments: %s\n\n", strings.Join(flag.Args(), " "))
		flag.Usage()
		os.Exit(2)
	}

	if *versionFlag {
		fmt.Printf("clipsync %s\n", Version)
		os.Exit(0)
	}

	level, levelErr := parseLogLevel(*logLevelFlag)
	setupLogging(level)
	if levelErr != nil {
		slog.Warn("Invalid log level, defaulting to info", "err", levelErr)
	}

	*interval = validateInterval(*interval)

	syncFile, err := resolveFilePath(*filePath)
	if err != nil {
		slog.Error("Failed to resolve path", "err", err)
		os.Exit(1)
	}

	syncer, err := NewClipboardSync(syncFile, !*noNotify, *hideNotifyContent)
	if err != nil {
		slog.Error("Initialization failed", "err", err)
		os.Exit(1)
	}

	slog.Info("Sync started", "version", Version, "file", syncer.filePath, "interval", *interval, "notify", !*noNotify)

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

		tStart := time.Now()
		state = syncer.Transition(state)

		if state == Waiting {
			if sleepFor := intervalDuration - time.Since(tStart); sleepFor > 0 {
				timer := time.NewTimer(sleepFor)
				select {
				case <-sigChan:
					timer.Stop()
					slog.Info("Stopped")
					return
				case <-timer.C:
				}
			}
		}
	}
}
