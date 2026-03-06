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

const version = "0.2.1"

type State int

const (
	StateWaiting State = iota
	StateWritingClipToFile
	StateCopyingFileToClip
)

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
	filePath           string
	enableNotifications bool
	lastClip           string
	lastMtime          time.Time
	lastFileContent    string
}

func NewClipboardSync(filePath string, enableNotifications bool) (*ClipboardSync, error) {
	// Ensure file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		f, err := os.Create(filePath)
		if err != nil {
			return nil, fmt.Errorf("could not create file %s: %w", filePath, err)
		}
		f.Close()
	}

	cs := &ClipboardSync{
		filePath:           filePath,
		enableNotifications: enableNotifications,
	}

	cs.lastClip = cs.safePaste()
	cs.lastMtime = cs.getMtime()
	cs.lastFileContent = cs.safeRead()

	log.Printf("Sync initialized on: %s", filePath)
	return cs, nil
}

func (cs *ClipboardSync) showNotification(title, message string) {
	if !cs.enableNotifications {
		return
	}
	if err := beeep.Notify(title, message, ""); err != nil {
		log.Printf("WARNING: Notification failed: %v", err)
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
		errMsg := err.Error()
		if strings.Contains(errMsg, "completed successfully") {
			log.Printf("DEBUG: Clipboard locked by another app: %v", err)
		} else {
			log.Printf("WARNING: Clipboard access failed: %v", err)
		}
	}
	return cs.lastClip
}

func (cs *ClipboardSync) safeRead() string {
	data, err := os.ReadFile(cs.filePath)
	if err != nil {
		return ""
	}
	return string(data)
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

		// 3. Check for conflict files
		dir := filepath.Dir(cs.filePath)
		base := filepath.Base(cs.filePath)
		matches, err := filepath.Glob(filepath.Join(dir, base+"*"))
		if err == nil {
			var conflicts []string
			for _, m := range matches {
				if m != cs.filePath {
					info, err := os.Stat(m)
					if err == nil && !info.IsDir() {
						conflicts = append(conflicts, filepath.Base(m))
					}
				}
			}
			if len(conflicts) > 0 {
				log.Printf("WARNING: Potential conflict files detected: %v", conflicts)
			}
		}

		// 4. Check File Change
		currentMtime := cs.getMtime()
		if currentMtime != cs.lastMtime {
			cs.lastMtime = currentMtime

			info, err := os.Stat(cs.filePath)
			if err == nil && info.Size() == 0 {
				log.Printf("File %s is empty, waiting...", cs.filePath)
				return StateWaiting
			}

			currentFileContent := cs.safeRead()
			if currentFileContent != cs.lastFileContent {
				cs.lastFileContent = currentFileContent
				return StateCopyingFileToClip
			}
			log.Printf("The file %s content has not changed.", cs.filePath)
		}

		return StateWaiting

	case StateWritingClipToFile:
		log.Printf("Clipboard -> File (%d chars)", len(cs.lastClip))
		err := os.WriteFile(cs.filePath, []byte(cs.lastClip), 0644)
		if err != nil {
			log.Printf("ERROR: Write failed: %v", err)
		} else {
			cs.lastFileContent = cs.lastClip
			cs.lastMtime = cs.getMtime()
		}
		return StateWaiting

	case StateCopyingFileToClip:
		log.Printf("File -> Clipboard (%d chars)", len(cs.lastFileContent))
		if err := clipboard.WriteAll(cs.lastFileContent); err != nil {
			log.Printf("ERROR: Copy failed: %v", err)
		} else {
			cs.showNotification("Synced to Clipboard", compactMessage(cs.lastFileContent, 64))
			cs.lastClip = cs.lastFileContent
		}
		return StateWaiting
	}

	return StateWaiting
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime)

	filePath := flag.String("file-path", "./sync_clipboard.txt", "Path to the file used for syncing")
	flag.StringVar(filePath, "f", "./sync_clipboard.txt", "Path to the file used for syncing (shorthand)")

	interval := flag.Float64("interval", 0.5, "Polling interval in seconds")
	flag.Float64Var(interval, "i", 0.5, "Polling interval in seconds (shorthand)")

	noNotify := flag.Bool("no-notify", false, "Disable desktop notifications")

	printVersion := flag.Bool("version", false, "Print version and exit")
	flag.BoolVar(printVersion, "v", false, "Print version and exit (shorthand)")

	flag.Parse()

	if *printVersion {
		fmt.Printf("clipsyncer %s\n", version)
		os.Exit(0)
	}

	absPath, err := filepath.Abs(*filePath)
	if err != nil {
		log.Fatalf("CRITICAL: Could not resolve path: %v", err)
	}

	log.Printf("Sync (v%s) starting with interval %.2f seconds, file syncing: %s. Press Ctrl+C to stop.",
		version, *interval, absPath)

	syncer, err := NewClipboardSync(absPath, !*noNotify)
	if err != nil {
		log.Fatalf("CRITICAL: %v", err)
	}

	state := StateWaiting
	sleepDuration := time.Duration(*interval * float64(time.Second))

	for {
		state = syncer.Transition(state)
		time.Sleep(sleepDuration)
	}
}
