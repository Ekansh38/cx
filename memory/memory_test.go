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
