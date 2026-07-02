// Package memory manages the long-term memory file (~/.config/cx/memory.md).
//
// The file is structured markdown curated by an LLM: after every exchange the
// model reads the current file plus the latest turn and rewrites the whole
// thing — organizing into sections (Identity / Preferences / Projects / Tools
// & Workflow / Feedback / References), merging, generalizing, and pruning.
//
// :remember and :forget also route through the model so edits stay organized.
// This package only provides the raw read/write; the LLM prompting lives in ui.
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
	content = strings.TrimSpace(content)
	lines := strings.Split(content, "\n")
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		content = strings.Join(lines, "\n")
	}
	return os.WriteFile(path, []byte(content+"\n"), 0o644)
}
