package main

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClipboard struct {
	mu       sync.Mutex
	text     string
	readErr  error
	writeErr error
	writes   int
}

func (f *fakeClipboard) read() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return "", f.readErr
	}
	return f.text, nil
}

func (f *fakeClipboard) write(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if f.writeErr != nil {
		return f.writeErr
	}
	f.text = s
	return nil
}

func noopNotify(title, message string, icon any) error { return nil }

// bumpMtime moves file's mtime d into the future so the next poll sees a new version.
func bumpMtime(t *testing.T, file string, d time.Duration) time.Time {
	t.Helper()
	future := time.Now().Add(d)
	if err := os.Chtimes(file, future, future); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

func newTestSync(t *testing.T, file string, clip *fakeClipboard) *ClipboardSync {
	t.Helper()
	s, err := newClipboardSync(file, false, false, ioBackends{
		readClip:  clip.read,
		writeClip: clip.write,
		notify:    noopNotify,
	})
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
	return s
}

func TestCompactMessage(t *testing.T) {
	if got := compactMessage("a\r\nb\rc\nd", 64); got != "a b c d" {
		t.Fatalf("expected CRLF/CR/LF flattened, got %q", got)
	}
	if got := compactMessage("abcdef", 2); got != "ab" {
		t.Fatalf("expected safe small limit without panic, got %q", got)
	}
	if got := compactMessage("abcdef", 0); got != "" {
		t.Fatalf("expected empty for limit 0, got %q", got)
	}
	if got := compactMessage("abcdef", 5); got != "ab..." {
		t.Fatalf("expected truncated string, got %q", got)
	}
}

func TestValidateInterval(t *testing.T) {
	if got := validateInterval(math.NaN()); got != defaultInterval {
		t.Fatalf("NaN should reset to default, got %v", got)
	}
	if got := validateInterval(math.Inf(1)); got != defaultInterval {
		t.Fatalf("+Inf should reset to default, got %v", got)
	}
	if got := validateInterval(-1); got != minInterval {
		t.Fatalf("negative should clamp to minInterval, got %v", got)
	}
	if got := validateInterval(999999); got != maxInterval {
		t.Fatalf("huge interval should clamp to maxInterval, got %v", got)
	}
}

func TestSymlinkPreservedAndVerbatimBytes0600(t *testing.T) {
	dir := t.TempDir()
	realFile := filepath.Join(dir, "real_sync.txt")
	linkFile := filepath.Join(dir, "link_sync.txt")
	if err := os.Symlink(realFile, linkFile); err != nil {
		t.Fatalf("symlink failed: %v", err)
	}

	clip := &fakeClipboard{text: "init"}
	s := newTestSync(t, linkFile, clip)

	clip.text = "  secret-password\r\nline2\rline3\n  "
	state := s.Transition(Waiting)
	if state != WritingClipToFile {
		t.Fatalf("expected WritingClipToFile, got %v", state)
	}
	state = s.Transition(state)
	if state != Waiting {
		t.Fatalf("expected Waiting, got %v", state)
	}

	linfo, err := os.Lstat(linkFile)
	if err != nil || linfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected %s to remain a symlink", linkFile)
	}

	raw, err := os.ReadFile(realFile)
	if err != nil {
		t.Fatalf("read target failed: %v", err)
	}
	if string(raw) != "  secret-password\r\nline2\rline3\n  " {
		t.Fatalf("clipboard bytes were altered: %q", string(raw))
	}
	info, err := os.Stat(realFile)
	if err != nil {
		t.Fatalf("stat target failed: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected 0600 permissions, got %o", perm)
	}
}

func TestTransientCopyErrorRetriesNextCycle(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sync_clipboard.txt")
	if err := os.WriteFile(file, []byte("initial"), 0600); err != nil {
		t.Fatal(err)
	}

	clip := &fakeClipboard{text: "initial"}
	s := newTestSync(t, file, clip)

	if err := os.WriteFile(file, []byte("  from-remote\r\nline2  "), 0600); err != nil {
		t.Fatal(err)
	}
	mtime := bumpMtime(t, file, 2*time.Second)

	clip.writeErr = errors.New("clipboard locked")
	state := s.Transition(Waiting)
	if state != CopyingFileToClip {
		t.Fatalf("expected CopyingFileToClip, got %v", state)
	}
	s.Transition(state)
	if !s.lastCopyFailMtime.Equal(mtime) {
		t.Fatalf("expected failed mtime remembered for log dedup, got %v", s.lastCopyFailMtime)
	}

	clip.writeErr = nil
	state = s.Transition(Waiting)
	if state != CopyingFileToClip {
		t.Fatalf("expected retry CopyingFileToClip, got %v", state)
	}
	s.Transition(state)
	if clip.text != "  from-remote\r\nline2  " {
		t.Fatalf("expected verbatim clipboard after retry, got %q", clip.text)
	}
	if !s.lastCopyFailMtime.IsZero() {
		t.Fatalf("expected recovery to clear failure state, got %v", s.lastCopyFailMtime)
	}
}

func TestEmptyClipboardDoesNotWipeFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sync_clipboard.txt")
	if err := os.WriteFile(file, []byte("keep-me"), 0600); err != nil {
		t.Fatal(err)
	}

	clip := &fakeClipboard{text: "keep-me"}
	s := newTestSync(t, file, clip)

	clip.text = ""
	if state := s.Transition(Waiting); state != Waiting {
		t.Fatalf("expected Waiting (skip empty write), got %v", state)
	}

	raw, _ := os.ReadFile(file)
	if string(raw) != "keep-me" {
		t.Fatalf("file was wiped! got %q", string(raw))
	}
}

func TestInitClipboardFailureDoesNotClobberFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sync_clipboard.txt")
	if err := os.WriteFile(file, []byte("remote-content"), 0600); err != nil {
		t.Fatal(err)
	}

	clip := &fakeClipboard{text: "old-local-clip", readErr: errors.New("not ready")}
	s := newTestSync(t, file, clip)

	clip.readErr = nil
	if state := s.Transition(Waiting); state != Waiting {
		t.Fatalf("expected Waiting on deferred init, got %v", state)
	}

	raw, _ := os.ReadFile(file)
	if string(raw) != "remote-content" {
		t.Fatalf("remote file was clobbered! got %q", string(raw))
	}
}

func TestOversizedFileSkippedWithoutOOM(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sync_clipboard.txt")
	if err := os.WriteFile(file, []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}

	clip := &fakeClipboard{text: "ok"}
	s := newTestSync(t, file, clip)

	big := strings.Repeat("A", maxContentBytes+10)
	if err := os.WriteFile(file, []byte(big), 0600); err != nil {
		t.Fatal(err)
	}
	bumpMtime(t, file, 2*time.Second)

	if state := s.Transition(Waiting); state != Waiting {
		t.Fatalf("expected oversized file skipped, got %v", state)
	}
	if len(s.lastFileContent) > maxContentBytes {
		t.Fatalf("oversized content should not be retained in memory")
	}
}

func TestConflictCheckHandlesGlobMetacharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dir[1]")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "clip[0].txt")
	conflict := filepath.Join(dir, "clip[0].txt.conflict1")
	if err := os.WriteFile(file, []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conflict, []byte("dup"), 0600); err != nil {
		t.Fatal(err)
	}

	clip := &fakeClipboard{text: "ok"}
	s := newTestSync(t, file, clip)

	s.checkConflicts()
	if len(s.lastConflicts) != 1 || s.lastConflicts[0] != "clip[0].txt.conflict1" {
		t.Fatalf("expected conflict detected despite glob chars, got %v", s.lastConflicts)
	}
}

func TestInvalidUTF8FileSkippedUntilMtimeChanges(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sync_clipboard.txt")
	if err := os.WriteFile(file, []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}

	clip := &fakeClipboard{text: "ok"}
	s := newTestSync(t, file, clip)

	if err := os.WriteFile(file, []byte{'a', 0xff, 0xfe, 'b'}, 0600); err != nil {
		t.Fatal(err)
	}
	bad := bumpMtime(t, file, 2*time.Second)

	if state := s.Transition(Waiting); state != Waiting {
		t.Fatalf("expected invalid UTF-8 file skipped, got %v", state)
	}
	if clip.writes != 0 || clip.text != "ok" {
		t.Fatalf("clipboard must not receive invalid UTF-8, got %q", clip.text)
	}
	if !s.lastMtime.Equal(bad) {
		t.Fatalf("expected invalid file version consumed, got %v", s.lastMtime)
	}

	// A later valid write (new mtime) is picked up again.
	if err := os.WriteFile(file, []byte("fixed"), 0600); err != nil {
		t.Fatal(err)
	}
	bumpMtime(t, file, 4*time.Second)
	state := s.Transition(Waiting)
	if state != CopyingFileToClip {
		t.Fatalf("expected valid file copied after recovery, got %v", state)
	}
	s.Transition(state)
	if clip.text != "fixed" {
		t.Fatalf("expected clipboard updated, got %q", clip.text)
	}
}

func TestInvalidUTF8ClipboardSkipped(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sync_clipboard.txt")
	if err := os.WriteFile(file, []byte("keep-me"), 0600); err != nil {
		t.Fatal(err)
	}

	clip := &fakeClipboard{text: "keep-me"}
	s := newTestSync(t, file, clip)

	clip.text = "bad\xffbytes"
	for i := 0; i < 2; i++ {
		if state := s.Transition(Waiting); state != Waiting {
			t.Fatalf("expected invalid clipboard skipped, got %v", state)
		}
	}
	if s.lastClip != clip.text {
		t.Fatalf("expected invalid clip retained so repeats short-circuit")
	}
	raw, _ := os.ReadFile(file)
	if string(raw) != "keep-me" {
		t.Fatalf("file overwritten with invalid UTF-8: %q", string(raw))
	}

	clip.text = "good"
	if state := s.Transition(Waiting); state != WritingClipToFile {
		t.Fatalf("expected valid clipboard written, got %v", state)
	}
}
