// Package memory manages the long-term memory file (~/.config/cx/memory.md).
//
// The file is freeform markdown curated by an LLM after each exchange: the
// model receives the current file plus the latest exchange and rewrites the
// whole file — merging, generalizing, and pruning. :remember and :forget
// provide instant line-level manual control between curation passes.
package memory

import (
	"os"
	"strings"
	"sync"
)

// maxLines is a hard safety cap so a runaway model can't bloat the file.
const maxLines = 100

var mu sync.Mutex

// Raw returns the full memory file content ("" if it doesn't exist).
func Raw(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// SaveRaw writes the memory file, enforcing the line cap.
func SaveRaw(path, content string) error {
	mu.Lock()
	defer mu.Unlock()
	return saveLocked(path, content)
}

func saveLocked(path, content string) error {
	content = strings.TrimSpace(content)
	lines := strings.Split(content, "\n")
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		content = strings.Join(lines, "\n")
	}
	return os.WriteFile(path, []byte(content+"\n"), 0o644)
}

// Add appends a fact as a bullet line unless a line already contains it.
// Returns true if added.
func Add(path, fact string) (bool, error) {
	mu.Lock()
	defer mu.Unlock()

	fact = strings.TrimSpace(fact)
	if fact == "" {
		return false, nil
	}

	content, err := Raw(path)
	if err != nil {
		return false, err
	}

	lower := strings.ToLower(fact)
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(strings.ToLower(line), lower) {
			return false, nil
		}
	}

	if content == "" {
		content = "# Memory"
	}
	return true, saveLocked(path, content+"\n- "+fact)
}

// Remove deletes non-header lines matching query (case-insensitive substring).
// Returns the number of lines removed.
func Remove(path, query string) (int, error) {
	mu.Lock()
	defer mu.Unlock()

	content, err := Raw(path)
	if err != nil || content == "" {
		return 0, err
	}

	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return 0, nil
	}

	var kept []string
	removed := 0
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") &&
			strings.TrimSpace(line) != "" &&
			strings.Contains(strings.ToLower(line), q) {
			removed++
			continue
		}
		kept = append(kept, line)
	}

	if removed > 0 {
		return removed, saveLocked(path, strings.Join(kept, "\n"))
	}
	return 0, nil
}
