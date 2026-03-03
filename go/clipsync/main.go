package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/gen2brain/beeep"
)

const Version = "0.0.2"

// State Enum equivalent
type State int

const (
	StateWaiting State = iota
	StateWritingClipToFile
	StateCopyingFileToClip
)

// CompactMessage truncates string for cleaner log/notification output.
func compactMessage(content string, limit int) string {
	if content == "" {
		return ""
	}
	content = strings.TrimSpace(strings.ReplaceAll(content, "\n", " "))
	if len(content) > limit {
		return content[:(limit-3)] + "..."
	}
	return content
}

type ClipboardSync struct {
	filePath           string
	enableNotifications bool
	lastClip           string
	lastMtime          time.Time
	lastFileContent    string
}

func NewClipboardSync(path string, enableNotifications bool) *ClipboardSync {
	// Ensure file exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		file, err := os.Create(path)
		if err != nil {
			log.Fatalf("Could not create file %s: %v", path, err)
		}
		file.Close()
	}

	cs := &ClipboardSync{
		filePath:           path,
		enableNotifications: enableNotifications,
	}

	// Cache initial state
	cs.lastClip = cs.safePaste()
	cs.lastMtime = cs.getMtime()
	cs.lastFileContent = cs.safeRead()

	log.Printf("Sync initialized on: %s", cs.filePath)
	return cs
}

func (cs *ClipboardSync) showNotification(title, message string) {
	if !cs.enableNotifications {
		return
	}
	// Beeep doesn't support specific timeouts consistently across platforms,
	// but it handles the system default well.
	err := beeep.Notify(title, message, "")
	if err != nil {
		log.Printf("Notification failed: %v", err)
	}
}

func (cs *ClipboardSync) getMtime() time.Time {
	info, err := os.Stat(cs.filePath)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

func (cs *ClipboardSync) safePaste() string {
	text, err := clipboard.ReadAll()
	if err != nil {
		log.Printf("Clipboard access failed: %v", err)
		return cs.lastClip
	}
	return text
}

func (cs *ClipboardSync) safeRead() string {
	content, err := os.ReadFile(cs.filePath)
	if err != nil {
		return ""
	}
	return string(content)
}

func (cs *ClipboardSync) Transition(state State) State {
	switch state {
	case StateWaiting:
		// 1. Check Clipboard Change
		currentClip := cs.safePaste()
		if currentClip != cs.lastClip {
			cs.lastClip = currentClip
			return StateWritingClipToFile
		}

		// 2. File existence check
		if _, err := os.Stat(cs.filePath); os.IsNotExist(err) {
			log.Printf("File %s vanished (syncing?), waiting...", cs.filePath)
			return StateWaiting
		}

		// 3. Check for Conflicts (Logging only)
		// Glob logic: find files starting with the filename in the same directory
		dir := filepath.Dir(cs.filePath)
		baseName := filepath.Base(cs.filePath)
		matches, _ := filepath.Glob(filepath.Join(dir, baseName+"*"))
		
		var conflicts []string
		for _, m := range matches {
			if m != cs.filePath {
				conflicts = append(conflicts, filepath.Base(m))
			}
		}
		if len(conflicts) > 0 {
			log.Printf("Potential conflict files detected: %v", conflicts)
		}

		// 4. Check File Change
		currentMtime := cs.getMtime()
		// Compare UnixNano to ensure precision
		if !currentMtime.Equal(cs.lastMtime) {
			cs.lastMtime = currentMtime

			info, _ := os.Stat(cs.filePath)
			if info.Size() == 0 {
				log.Printf("File %s is empty, waiting...", cs.filePath)
				return StateWaiting
			}

			currentFileContent := cs.safeRead()

			if currentFileContent != cs.lastFileContent {
				cs.lastFileContent = currentFileContent
				return StateCopyingFileToClip
			} else {
				log.Printf("The file %s content has not changed.", cs.filePath)
			}
		}
		return StateWaiting

	case StateWritingClipToFile:
		log.Printf("Clipboard -> File (%d chars)", len(cs.lastClip))

		err := os.WriteFile(cs.filePath, []byte(cs.lastClip), 0644)
		if err != nil {
			log.Printf("Write failed: %v", err)
		} else {
			// CRITICAL: Update caches immediately
			cs.lastFileContent = cs.lastClip
			cs.lastMtime = cs.getMtime()
		}

		return StateWaiting

	case StateCopyingFileToClip:
		log.Printf("File -> Clipboard (%d chars)", len(cs.lastFileContent))

		err := clipboard.WriteAll(cs.lastFileContent)
		if err != nil {
			log.Printf("Copy failed: %v", err)
		} else {
			cs.showNotification("Synced to Clipboard", compactMessage(cs.lastFileContent, 64))
			
			// Update clipboard cache immediately
			cs.lastClip = cs.lastFileContent
		}

		return StateWaiting
	}

	return StateWaiting
}

func main() {
	// Configure logging
	log.SetFlags(log.Ltime | log.Ldate)

	// CLI Arguments
	filePath := flag.String("f", "./sync_clipboard.txt", "Path to the file used for syncing")
	// Note: We use duration string format in Go (e.g., "500ms") rather than float seconds
	intervalStr := flag.String("i", "500ms", "Polling interval (e.g., 500ms, 1s)")
	noNotify := flag.Bool("no-notify", false, "Disable desktop notifications")
	showVersion := flag.Bool("v", false, "Show version")

	flag.Parse()

	if *showVersion {
		fmt.Printf("clipboard-sync %s\n", Version)
		return
	}

	// Resolve absolute path
	absPath, err := filepath.Abs(*filePath)
	if err != nil {
		log.Fatalf("Error resolving path: %v", err)
	}

	// Parse interval
	interval, err := time.ParseDuration(*intervalStr)
	if err != nil {
		log.Fatalf("Invalid interval format (use '500ms', '1s'): %v", err)
	}

	syncer := NewClipboardSync(absPath, !*noNotify)

	log.Printf("Sync started (v%s). Ctrl+C to stop.", Version)

	state := StateWaiting
	for {
		state = syncer.Transition(state)
		time.Sleep(interval)
	}
}
