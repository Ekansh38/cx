package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tmpFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "memory.md")
}

func TestSaveAndRaw(t *testing.T) {
	p := tmpFile(t)

	if err := SaveRaw(p, "## Preferences\n- likes dark mode"); err != nil {
		t.Fatal(err)
	}
	content, err := Raw(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "## Preferences") ||
		!strings.Contains(content, "- likes dark mode") {
		t.Errorf("Raw = %q; want structured content preserved", content)
	}
}

func TestSaveRawLineCap(t *testing.T) {
	p := tmpFile(t)
	long := strings.Repeat("- line\n", 500)
	if err := SaveRaw(p, long); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 200 {
		t.Errorf("file has %d lines; want <= 200", len(lines))
	}
}

func TestRawMissingFile(t *testing.T) {
	content, err := Raw(filepath.Join(t.TempDir(), "nope.md"))
	if err != nil || content != "" {
		t.Errorf("Raw on missing file = %q, %v; want \"\", nil", content, err)
	}
}

func TestTransferBulletOnFork(t *testing.T) {
	src := `## Projects
- building cx: a terminal AI chat

## Recent conversations
- 2026-07-01 · Old Title: discussed the memory redesign
- 2026-06-30 · Other chat: unrelated`

	// Matching bullet: title + date rewritten to the fork.
	out := transferBullet(src, "Old Title", "Forked Discussion", "2026-07-06")
	if strings.Contains(out, "Old Title") {
		t.Errorf("source title should be gone: %q", out)
	}
	if !strings.Contains(out, "2026-07-06 · Forked Discussion: discussed the memory redesign") {
		t.Errorf("bullet not rewritten in place: %q", out)
	}
	// Other bullets survive.
	if !strings.Contains(out, "Other chat") {
		t.Errorf("unrelated bullet dropped: %q", out)
	}
	// Durable sections untouched.
	if !strings.Contains(out, "building cx") {
		t.Errorf("Projects section clobbered: %q", out)
	}
}

func TestTransferBulletOnForkNoMatch(t *testing.T) {
	src := `## Recent conversations
- 2026-07-01 · Other chat: unrelated`
	// No bullet matches the source title — should be a no-op.
	out := transferBullet(src, "Nonexistent", "Fork Title", "2026-07-06")
	if out != src {
		t.Errorf("expected no-op when source bullet absent; got %q", out)
	}
}

func TestTransferBulletOnForkNoSection(t *testing.T) {
	src := `## Projects
- building cx`
	// No Recent conversations section at all — no-op.
	out := transferBullet(src, "Old Title", "Fork", "2026-07-06")
	if out != src {
		t.Errorf("expected no-op without section; got %q", out)
	}
}

func TestStateRoundTrip(t *testing.T) {
	// State helpers target a fixed path under ~/.config/cx, so this test
	// writes there. To keep it hermetic we back up / clear the state file.
	path := stateFilePath()
	orig, _ := os.ReadFile(path)
	t.Cleanup(func() {
		if orig != nil {
			os.WriteFile(path, orig, 0o644)
		} else {
			os.Remove(path)
		}
	})
	os.Remove(path)

	if v := LastCurated(42); v != 0 {
		t.Errorf("LastCurated on empty state = %d; want 0", v)
	}
	MarkCurated(42, 99)
	if v := LastCurated(42); v != 99 {
		t.Errorf("LastCurated after MarkCurated = %d; want 99", v)
	}
	// Other conversations untouched.
	if v := LastCurated(43); v != 0 {
		t.Errorf("LastCurated for unrelated conv = %d; want 0", v)
	}
	MarkCurated(42, 150)
	if v := LastCurated(42); v != 150 {
		t.Errorf("LastCurated after overwrite = %d; want 150", v)
	}
	// Persists across a fresh load.
	state := LoadState()
	if state[42] != 150 {
		t.Errorf("LoadState = %v; want 42->150", state)
	}
}
