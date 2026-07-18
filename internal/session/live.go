package session

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/tolle-ai/tollecode/internal/config"
)

var liveMu sync.Mutex

// LiveEvent pairs a decoded agent event with the byte offset of its JSONL line.
type LiveEvent struct {
	Event  map[string]any
	Offset int64
}

// TailResult is returned by TailLiveEvents.
type TailResult struct {
	Events    []LiveEvent
	EndOffset int64 // byte position right after the last line read
}

func livePath(sessionID string) string {
	return filepath.Join(config.Home(), "live", sessionID+".jsonl")
}

func ensureLiveDir() error {
	return os.MkdirAll(filepath.Join(config.Home(), "live"), 0o755)
}

// AppendLiveEvent writes one event as a JSONL line and returns the byte offset
// where that line starts (i.e., the value to set as _off on the event).
func AppendLiveEvent(sessionID string, event map[string]any) (int64, error) {
	if err := ensureLiveDir(); err != nil {
		return 0, err
	}
	liveMu.Lock()
	defer liveMu.Unlock()

	f, err := os.OpenFile(livePath(sessionID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// File size before writing = byte offset of the new line.
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	offset := info.Size()

	line, err := json.Marshal(event)
	if err != nil {
		return offset, err
	}
	line = append(line, '\n')
	_, err = f.Write(line)
	return offset, err
}

// ClearLiveEvents deletes the live JSONL file for sessionID.
func ClearLiveEvents(sessionID string) {
	os.Remove(livePath(sessionID))
}

// TailLiveEvents reads the live file from fromOffset onwards and returns events
// paired with their starting byte offsets. TailResult.EndOffset is the byte
// position immediately after the last line read (use as the next fromOffset).
func TailLiveEvents(sessionID string, fromOffset int64) (TailResult, error) {
	f, err := os.Open(livePath(sessionID))
	if os.IsNotExist(err) {
		return TailResult{EndOffset: fromOffset}, nil
	}
	if err != nil {
		return TailResult{EndOffset: fromOffset}, err
	}
	defer f.Close()

	// Guard: reset stale offset if the live file was cleared for a new turn and
	// the client still holds an offset larger than the current file size.
	if fileSize, serr := f.Seek(0, io.SeekEnd); serr == nil && fromOffset > fileSize {
		fromOffset = 0
	}
	if _, err := f.Seek(fromOffset, io.SeekStart); err != nil {
		return TailResult{EndOffset: fromOffset}, nil
	}

	pos := fromOffset
	result := TailResult{EndOffset: pos}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		lineStart := pos
		raw := scanner.Bytes()
		pos += int64(len(raw)) + 1 // +1 for the \n Scanner consumed

		var ev map[string]any
		if json.Unmarshal(raw, &ev) == nil {
			result.Events = append(result.Events, LiveEvent{Event: ev, Offset: lineStart})
		}
	}
	result.EndOffset = pos
	return result, nil
}
