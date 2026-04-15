package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/atotto/clipboard"
	"github.com/gen2brain/beeep"
)

const version = "0.3.0"

// State represents the state machine states
type State int

const (
	Waiting State = iota
	WritingClipToFile
	CopyingFileToClip
)

// compactMessage truncates string for cleaner log/notification output.
// Uses runes to correctly handle multi-byte UTF-8 characters.
func compactMessage(content string, limit int) string {
	if content == "" {
		return ""
	}
	content = strings.TrimSpace(strings.ReplaceAll(content, "\n", " "))
	runes := []rune(content)
	if len(runes) > limit {
		return string(runes[:limit-3]) + "..."
	}
	return string(runes)
}

// ClipboardSync holds the state and configuration for the sync process
type ClipboardSync struct {
	filePath            string
	dir                 string
	globPattern         string
	enableNotifications bool

	lastClip         string
	lastMTime        time.Time
	lastFileContent  string
}

// NewClipboardSync initializes and returns a new ClipboardSync instance
func NewClipboardSync(path string, enableNotifications bool) (*ClipboardSync, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve path: %w", err)
	}

	dir := filepath.Dir(absPath)
	name := filepath.Base(absPath)
	globPattern := filepath.Join(dir, name+"*")

	// Ensure file exists
	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		file, err := os.Create(absPath)
		if err != nil {
			log.Fatalf("Could not create file %s: %v", absPath, err)
		}
		file.Close()
	}

	c := &ClipboardSync{
		filePath:            absPath,
		dir:                 dir,
		globPattern:         globPattern,
		enableNotifications: enableNotifications,
	}

	// Initialize caches
	c.lastClip = c.safePaste()
	c.lastMTime = c.getMTime()
	c.lastFileContent = c.safeRead()

	log.Printf("Sync initialized on: %s", c.filePath)
	return c, nil
}

// ------------------------------------------------------------------
// Helpers
// ------------------------------------------------------------------

func (c *ClipboardSync) showNotification(title, message string) {
	if !c.enableNotifications {
		return
	}
	// Run in a goroutine to avoid blocking the loop
	go func() {
		err := beeep.Notify(title, message, "")
		if err != nil {
			log.Printf("Notification failed: %v", err)
		}
	}()
}

func (c *ClipboardSync) getMTime() time.Time {
	info, err := os.Stat(c.filePath)
	if err != nil {
		return time.Time{} // Returns zero value on error
	}
	return info.ModTime()
}

func (c *ClipboardSync) safePaste() string {
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		text, err := clipboard.ReadAll()
		if err == nil {
			return text
		}
		if attempt < maxRetries-1 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		log.Printf("Clipboard access failed: %v", err)
	}
	return ""
}

func (c *ClipboardSync) safeRead() string {
	data, err := os.ReadFile(c.filePath)
	if err != nil {
		return ""
	}
	return string(data)
}

func (c *ClipboardSync) checkConflicts() {
	matches, err := filepath.Glob(c.globPattern)
	if err != nil {
		return
	}

	var conflictFiles []string
	for _, match := range matches {
		info, err := os.Stat(match)
		if err == nil && !info.IsDir() && match != c.filePath {
			conflictFiles = append(conflictFiles, filepath.Base(match))
		}
	}

	if len(conflictFiles) > 0 {
		log.Printf("Potential conflict files detected: %v", conflictFiles)
	}
}

// ------------------------------------------------------------------
// State machine
// ------------------------------------------------------------------

func (c *ClipboardSync) transition(state State) State {
	switch state {
	case Waiting:
		// 1. Check clipboard change
		currentClip := c.safePaste()
		if currentClip != c.lastClip {
			c.lastClip = currentClip
			return WritingClipToFile
		}

		// 2. File existence check
		if _, err := os.Stat(c.filePath); os.IsNotExist(err) {
			log.Printf("File %s vanished (syncing?), waiting...", c.filePath)
			return Waiting
		}

		// 3. Conflict detection
		c.checkConflicts()

		// 4. Check file change by mtime
		currentMTime := c.getMTime()
		if currentMTime.Equal(c.lastMTime) {
			return Waiting
		}
		c.lastMTime = currentMTime

		// Single read — avoids TOCTOU race between size check and content read
		currentFileContent := c.safeRead()
		if currentFileContent == "" {
			log.Printf("File %s is empty (cloud sync lock?), waiting...", c.filePath)
			return Waiting
		}

		if currentFileContent != c.lastFileContent {
			c.lastFileContent = currentFileContent
			return CopyingFileToClip
		}

		log.Printf("File %s mtime changed but content is unchanged.", c.filePath)
		return Waiting

	case WritingClipToFile:
		log.Printf("Clipboard -> File (%d chars)", len([]rune(c.lastClip)))
		err := os.WriteFile(c.filePath, []byte(c.lastClip), 0644)
		if err != nil {
			log.Printf("Write failed: %v", err)
		} else {
			// Update caches immediately to suppress self-triggered file-change detection
			c.lastFileContent = c.lastClip
			c.lastMTime = c.getMTime()
		}
		return Waiting

	case CopyingFileToClip:
		log.Printf("File -> Clipboard (%d chars)", len([]rune(c.lastFileContent)))
		err := clipboard.WriteAll(c.lastFileContent)
		if err != nil {
			log.Printf("Copy failed: %v", err)
		} else {
			// Update cache before notification (non-blocking) to prevent echo
			c.lastClip = c.lastFileContent
			c.showNotification("Synced to Clipboard", compactMessage(c.lastFileContent, 64))
		}
		return Waiting
	}

	return Waiting
}

// ------------------------------------------------------------------
// Entry point
// ------------------------------------------------------------------

func main() {
	// Setup standard logging format to match Python
	log.SetFlags(log.Ldate | log.Ltime)

	filePathPtr := flag.String("f", "./sync_clipboard.txt", "Path to the file used for syncing")
	flag.StringVar(filePathPtr, "file-path", "./sync_clipboard.txt", "Path to the file used for syncing")

	intervalPtr := flag.Float64("i", 0.5, "Polling interval in seconds")
	flag.Float64Var(intervalPtr, "interval", 0.5, "Polling interval in seconds")

	noNotifyPtr := flag.Bool("no-notify", false, "Disable desktop notifications")
	showVersion := flag.Bool("v", false, "Show version and exit")
	flag.BoolVar(showVersion, "version", false, "Show version and exit")

	flag.Parse()

	if *showVersion {
		fmt.Printf("ClipboardSync v%s\n", version)
		os.Exit(0)
	}

	absPath, _ := filepath.Abs(*filePathPtr)
	log.Printf("Sync (v%s) starting — interval: %.2fs, file: %s. Press Ctrl+C to stop.", version, *intervalPtr, absPath)

	syncer, err := NewClipboardSync(absPath, !*noNotifyPtr)
	if err != nil {
		log.Fatalf("Failed to initialize syncer: %v", err)
	}

	// Graceful shutdown on SIGTERM and SIGINT (Ctrl+C)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigs
		log.Println("Stopped.")
		os.Exit(0)
	}()

	intervalDuration := time.Duration(*intervalPtr * float64(time.Second))
	state := Waiting

	// Polling loop
	for {
		tStart := time.Now()
		state = syncer.transition(state)
		elapsed := time.Since(tStart)

		sleepFor := intervalDuration - elapsed
		if sleepFor > 0 {
			time.Sleep(sleepFor)
		}
	}
}
