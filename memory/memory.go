package memory

import (
	"os"
	"strings"
	"sync"
)

const maxFacts = 50

var mu sync.Mutex

// Load reads memory.md and returns the list of facts.
func Load(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var facts []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") {
			facts = append(facts, strings.TrimPrefix(line, "- "))
		}
	}
	return facts, nil
}

// Save writes facts to memory.md with a header.
func Save(path string, facts []string) error {
	var sb strings.Builder
	sb.WriteString("# Memory\n\n")
	for _, f := range facts {
		sb.WriteString("- ")
		sb.WriteString(f)
		sb.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

// Add appends a fact if it's not a near-duplicate. Returns true if added.
func Add(path string, fact string) (bool, error) {
	mu.Lock()
	defer mu.Unlock()

	facts, err := Load(path)
	if err != nil {
		return false, err
	}

	fact = strings.TrimSpace(fact)
	if fact == "" {
		return false, nil
	}

	// Dedup: skip if new fact is substring of existing or vice versa
	lower := strings.ToLower(fact)
	for _, existing := range facts {
		el := strings.ToLower(existing)
		if strings.Contains(el, lower) || strings.Contains(lower, el) {
			return false, nil
		}
	}

	facts = append(facts, fact)

	// Evict oldest if over cap
	if len(facts) > maxFacts {
		facts = facts[len(facts)-maxFacts:]
	}

	return true, Save(path, facts)
}

// Remove removes facts matching query (case-insensitive substring).
// Returns the number of facts removed.
func Remove(path string, query string) (int, error) {
	mu.Lock()
	defer mu.Unlock()

	facts, err := Load(path)
	if err != nil {
		return 0, err
	}

	q := strings.ToLower(query)
	var kept []string
	removed := 0
	for _, f := range facts {
		if strings.Contains(strings.ToLower(f), q) {
			removed++
		} else {
			kept = append(kept, f)
		}
	}

	if removed > 0 {
		return removed, Save(path, kept)
	}
	return 0, nil
}

// FormatForPrompt returns memory facts formatted for system prompt injection.
func FormatForPrompt(facts []string) string {
	if len(facts) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, f := range facts {
		sb.WriteString("- ")
		sb.WriteString(f)
		sb.WriteByte('\n')
	}
	return sb.String()
}
