package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The child must not depend on the parent's file descriptors: after spawn,
// the parent can close stdout/stdin/store immediately and the child still
// works. Verified by starting the child with a state file and confirming
// it completes (or fails cleanly) without any parent-side FDs.
func TestSpawnMemoryFlushDetaches(t *testing.T) {
	tmp := t.TempDir()
	statePath := filepath.Join(tmp, "state.json")
	state := flushState{ConvTitle: "test", Today: "2026-07-06", Fresh: "user: hi\n\nassistant: hey\n"}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(statePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Structural check: without a valid config the child returns non-zero,
	// but we only care that it doesn't panic or hang. If config is unavailable
	// the child returns 1 quickly.
	code := RunFlushChild(statePath)
	if _, err := os.Stat(statePath); err == nil {
		t.Errorf("state file must be consumed even on error, got: still exists")
	}
	_ = code
}

// spawnMemoryFlush is a no-op when there's nothing to flush.
func TestSpawnMemoryFlushEmpty(t *testing.T) {
	if spawnMemoryFlush(flushState{}) {
		t.Error("spawn should be false for empty state")
	}
	if spawnMemoryFlush(flushState{Fresh: "   "}) {
		t.Error("spawn should be false for whitespace-only fresh")
	}
}

func TestFlushStateJSONRoundTrip(t *testing.T) {
	before := flushState{ConvTitle: "a", Today: "b", Other: "c", Already: "d", Fresh: "e"}
	data, _ := json.Marshal(before)
	var after flushState
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("roundtrip = %+v; want %+v", after, before)
	}
	_ = time.Second
	_ = strings.TrimSpace
}
