package stdio

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

var emitMu sync.Mutex

// EmitSink receives every event passed to Emit. The default sink writes one
// JSON line to stdout (the Tauri IPC transport). Web mode installs a sink that
// broadcasts to connected /ws/cmd clients instead. Set via SetEmitSink.
type EmitSink func(event map[string]any)

// emitSink is the active event transport. nil means "use the default stdout
// writer". Guarded by emitMu together with the actual write so a swap can never
// interleave with an in-flight emit.
var emitSink EmitSink

// SetEmitSink replaces the event transport. Pass nil to restore stdout output.
// Safe to call while other goroutines are emitting.
func SetEmitSink(sink EmitSink) {
	emitMu.Lock()
	defer emitMu.Unlock()
	emitSink = sink
}

// Emit delivers a single event to the active sink (stdout by default).
// Thread-safe: multiple goroutines (agent tasks) may call this concurrently.
func Emit(event map[string]any) {
	emitMu.Lock()
	sink := emitSink
	emitMu.Unlock()

	if sink != nil {
		sink(event)
		return
	}

	line, err := json.Marshal(event)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[sidecar] emit marshal error: %v\n", err)
		return
	}
	emitMu.Lock()
	defer emitMu.Unlock()
	os.Stdout.Write(line)
	os.Stdout.Write([]byte{'\n'})
}

// EmitType is a shorthand for Emit when you only need a type field.
func EmitType(typ string) {
	Emit(map[string]any{"type": typ})
}
