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

func TestAddAndRaw(t *testing.T) {
	p := tmpFile(t)

	added, err := Add(p, "likes dark mode")
	if err != nil || !added {
		t.Fatalf("Add = %v, %v; want true, nil", added, err)
	}

	content, err := Raw(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "- likes dark mode") {
		t.Errorf("Raw = %q; want it to contain the fact", content)
	}
}

func TestAddDedup(t *testing.T) {
	p := tmpFile(t)
	Add(p, "uses Go for everything")

	added, _ := Add(p, "uses go") // substring of existing line, case-insensitive
	if added {
		t.Error("Add should dedup against existing line content")
	}
}

func TestRemove(t *testing.T) {
	p := tmpFile(t)
	Add(p, "likes dark mode")
	Add(p, "prefers tabs over spaces")

	n, err := Remove(p, "dark mode")
	if err != nil || n != 1 {
		t.Fatalf("Remove = %d, %v; want 1, nil", n, err)
	}

	content, _ := Raw(p)
	if strings.Contains(content, "dark mode") {
		t.Error("removed fact still present")
	}
	if !strings.Contains(content, "tabs over spaces") {
		t.Error("unrelated fact was removed")
	}
}

func TestRemoveKeepsHeaders(t *testing.T) {
	p := tmpFile(t)
	SaveRaw(p, "# Memory about the user\n- user fact")

	n, _ := Remove(p, "user")
	if n != 1 {
		t.Fatalf("Remove = %d; want 1 (header must not count)", n)
	}
	content, _ := Raw(p)
	if !strings.Contains(content, "# Memory about the user") {
		t.Error("header was removed")
	}
}

func TestSaveRawLineCap(t *testing.T) {
	p := tmpFile(t)
	long := strings.Repeat("- line\n", 200)
	if err := SaveRaw(p, long); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 100 {
		t.Errorf("file has %d lines; want <= 100", len(lines))
	}
}

func TestRawMissingFile(t *testing.T) {
	content, err := Raw(filepath.Join(t.TempDir(), "nope.md"))
	if err != nil || content != "" {
		t.Errorf("Raw on missing file = %q, %v; want \"\", nil", content, err)
	}
}
