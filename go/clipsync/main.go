package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gen2brain/beeep"
	"golang.design/x/clipboard"
)

const version = "0.4.0"

func compactMessage(content string, limit int) string {
	if content == "" {
		return ""
	}
	content = strings.ReplaceAll(content, "\n", " ")
	content = strings.TrimSpace(content)
	if len(content) > limit {
		return content[:limit-3] + "..."
	}
	return content
}

type ClipboardSync struct {
	filePath            string
	dirPath             string
	globPattern         string
	enableNotifications bool

	// lastFileContent is the last content we wrote to OR read from the file.
	// It is used to suppress echoing a file-triggered write back to the file watcher,
	// and to suppress echoing a clipboard-triggered write back to the clipboard watcher.
	lastFileContent string
	lastMtime       time.Time
}

func NewClipboardSync(filePath string, enableNotifications bool) (*ClipboardSync, error) {
	// Resolve symlinks so path comparisons are always against the real file.
	resolved, err := filepath.EvalSymlinks(filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("could not resolve symlinks for %s: %w", filePath, err)
		}
		resolved = filePath
	}

	// Ensure file exists.
	if _, err := os.Stat(resolved); os.IsNotExist(err) {
		f, err := os.Create(resolved)
		if err != nil {
			return nil, fmt.Errorf("could not create file %s: %w", resolved, err)
		}
		f.Close()
	}

	cs := &ClipboardSync{
		filePath:            resolved,
		dirPath:             filepath.Dir(resolved),
		globPattern:         filepath.Base(resolved) + "*",
		enableNotifications: enableNotifications,
	}

	cs.lastFileContent = cs.safeRead()
	cs.lastMtime = cs.getMtime()

	log.Printf("Sync initialized on: %s", resolved)
	return cs, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (cs *ClipboardSync) showNotification(title, message string) {
	if !cs.enableNotifications {
		return
	}
	go func() {
		if err := beeep.Notify(title, message, ""); err != nil {
			log.Printf("WARNING: Notification failed: %v", err)
		}
	}()
}

func (cs *ClipboardSync) getMtime() time.Time {
	info, err := os.Stat(cs.filePath)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// safeRead performs a single read so the emptiness check and content are
// always consistent (avoids the TOCTOU race of stat-then-read).
func (cs *ClipboardSync) safeRead() string {
	data, err := os.ReadFile(cs.filePath)
	if err != nil {
		return ""
	}
	return string(data)
}

func (cs *ClipboardSync) checkConflicts() {
	matches, err := filepath.Glob(filepath.Join(cs.dirPath, cs.globPattern))
	if err != nil {
		return
	}
	var conflicts []string
	for _, m := range matches {
		if m == cs.filePath {
			continue
		}
		if info, err := os.Stat(m); err == nil && !info.IsDir() {
			conflicts = append(conflicts, filepath.Base(m))
		}
	}
	if len(conflicts) > 0 {
		log.Printf("WARNING: Potential conflict files detected: %v", conflicts)
	}
}

// ---------------------------------------------------------------------------
// Clipboard -> File  (event-driven via clipboard.Watch)
// ---------------------------------------------------------------------------

// watchClipboard runs in its own goroutine. It receives clipboard change
// events from the library and writes new content to the file, unless the
// content originated from the file watcher (echo suppression).
func (cs *ClipboardSync) watchClipboard(ctx context.Context) {
	ch := clipboard.Watch(ctx, clipboard.FmtText)
	for {
		select {
		case <-ctx.Done():
			return
		case data, ok := <-ch:
			if !ok {
				return
			}
			text := string(data)

			// Suppress echo: this change was triggered by us writing to the
			// clipboard from the file watcher — do not write it back to the file.
			if text == cs.lastFileContent {
				log.Printf("Clipboard echo suppressed (%d chars)", len(text))
				continue
			}

			log.Printf("Clipboard -> File (%d chars)", len(text))
			if err := os.WriteFile(cs.filePath, data, 0644); err != nil {
				log.Printf("ERROR: Write to file failed: %v", err)
				continue
			}
			// Update caches so the file watcher does not re-read what we just wrote.
			cs.lastFileContent = text
			cs.lastMtime = cs.getMtime()
		}
	}
}

// ---------------------------------------------------------------------------
// File -> Clipboard  (polling via ticker, mtime-gated)
// ---------------------------------------------------------------------------

// watchFile runs in its own goroutine. It polls the file's mtime and copies
// new content to the clipboard when the file changes externally.
func (cs *ClipboardSync) watchFile(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cs.checkConflicts()

			// File existence check.
			if _, err := os.Stat(cs.filePath); os.IsNotExist(err) {
				log.Printf("File %s vanished (syncing?), waiting...", cs.filePath)
				continue
			}

			currentMtime := cs.getMtime()
			if time.Time.Equal(currentMtime, cs.lastMtime) {
				continue
			}
			cs.lastMtime = currentMtime

			// Single read — consistent size + content, no TOCTOU race.
			content := cs.safeRead()
			if content == "" {
				log.Printf("File %s is empty (cloud sync lock?), waiting...", cs.filePath)
				continue
			}
			if content == cs.lastFileContent {
				log.Printf("File %s mtime changed but content is unchanged.", cs.filePath)
				continue
			}

			log.Printf("File -> Clipboard (%d chars)", len(content))
			// Update lastFileContent BEFORE writing to clipboard so that
			// watchClipboard's echo-suppression check sees the new value
			// by the time the Watch event fires.
			cs.lastFileContent = content
			clipboard.Write(clipboard.FmtText, []byte(content))
			cs.showNotification("Synced to Clipboard", compactMessage(content, 64))
		}
	}
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	log.SetFlags(log.Ldate | log.Ltime)

	filePath := flag.String("file-path", "./sync_clipboard.txt", "Path to the file used for syncing")
	flag.StringVar(filePath, "f", "./sync_clipboard.txt", "Path to the file used for syncing (shorthand)")

	interval := flag.Float64("interval", 0.5, "File polling interval in seconds")
	flag.Float64Var(interval, "i", 0.5, "File polling interval in seconds (shorthand)")

	noNotify := flag.Bool("no-notify", false, "Disable desktop notifications")

	printVersion := flag.Bool("version", false, "Print version and exit")
	flag.BoolVar(printVersion, "v", false, "Print version and exit (shorthand)")

	flag.Parse()

	if *printVersion {
		fmt.Printf("clipsyncer %s\n", version)
		os.Exit(0)
	}

	// Initialize the clipboard library (required by golang.design/x/clipboard).
	if err := clipboard.Init(); err != nil {
		log.Fatalf("CRITICAL: clipboard.Init() failed: %v", err)
	}

	absPath, err := filepath.Abs(*filePath)
	if err != nil {
		log.Fatalf("CRITICAL: Could not resolve path: %v", err)
	}

	log.Printf("Sync (v%s) starting — clipboard: event-driven, file poll: %.2fs, file: %s. Press Ctrl+C to stop.",
		version, *interval, absPath)

	syncer, err := NewClipboardSync(absPath, !*noNotify)
	if err != nil {
		log.Fatalf("CRITICAL: %v", err)
	}

	// Cancelable context — cancelled on SIGINT / SIGTERM.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		log.Printf("Received %s. Stopping...", sig)
		cancel()
	}()

	// Start both watchers concurrently.
	go syncer.watchClipboard(ctx)
	syncer.watchFile(ctx, time.Duration(*interval*float64(time.Second)))

	log.Println("Stopped.")
}
