// Package memory manages the long-term memory file (~/.config/cx/memory.md).
//
// The file is structured markdown curated by an LLM: after every exchange the
// model reads the current file plus the latest turn and rewrites the whole
// thing — organizing into sections (Identity / Preferences / Projects / Tools
// & Workflow / Feedback / References), merging, generalizing, and pruning.
//
// /remember and /forget also route through the model so edits stay organized.
// This package only provides the raw read/write; the LLM prompting lives in ui.
package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// maxLines is a hard safety cap so a runaway model can't bloat the file.
const maxLines = 200

var mu sync.Mutex

// ── Cross-process curation lock ─────────────────────────────────────────────
//
// Two cx instances running memory curation concurrently would race on the
// memory.md rewrite — last-write-wins, and the loser's turns are dropped
// from that pass. TryLockCuration takes a POSIX advisory lock on a small
// sidecar file; only the process holding the lock is allowed to curate.
// The other instance either skips this round (its threshold will fire
// again) or waits for the next tick.

// TryLockCuration attempts a non-blocking exclusive flock on
// ~/.config/cx/memory.md.lock. Returns the open lock handle on success;
// the caller MUST call UnlockCuration to release. Returns (nil, nil) if
// another process holds the lock — that's not an error, just a signal to
// defer this curation round.
func TryLockCuration() (*os.File, error) {
	path := filepath.Join(configDir(), "memory.md.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, nil // contention, not a real failure
		}
		return nil, err
	}
	return f, nil
}

// UnlockCuration releases the advisory lock and closes the file.
func UnlockCuration(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

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

// ── Curation state (last-curated position per conversation) ──────────────────
//
// memory.state.json maps conversation ID (as a string key) to the message ID
// of the latest assistant turn the memory model has already seen. It prevents
// re-curating the whole chat (and double-counting durable facts) after a
// restart, and lets the batched curation track exactly which turns are "new".

// LoadState reads the {convID: lastCuratedMsgID} map from disk.
func LoadState() map[int64]int64 {
	// Reuse the config dir rather than a brittle relative path.
	path := stateFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return map[int64]int64{}
	}
	var m map[string]int64
	if err := json.Unmarshal(data, &m); err != nil {
		return map[int64]int64{}
	}
	out := make(map[int64]int64, len(m))
	for k, v := range m {
		var id int64
		fmt.Sscanf(k, "%d", &id)
		if id != 0 {
			out[id] = v
		}
	}
	return out
}

// SaveState writes the {convID: lastCuratedMsgID} map to disk.
func SaveState(state map[int64]int64) {
	path := stateFilePath()
	m := make(map[string]int64, len(state))
	for k, v := range state {
		m[fmt.Sprintf("%d", k)] = v
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	mu.Lock()
	defer mu.Unlock()
	os.WriteFile(path, data, 0o644)
}

// MarkCurated records that the memory model has seen up to and including
// lastMsgID for the given conversation.
func MarkCurated(convID, lastMsgID int64) {
	state := LoadState()
	state[convID] = lastMsgID
	SaveState(state)
}

// LastCurated returns the message ID the memory model last saw for convID,
// or 0 if none recorded.
func LastCurated(convID int64) int64 {
	return LoadState()[convID]
}

// ── Fork bullet transfer ─────────────────────────────────────────────────────
//
// When a conversation is forked, the source's "## Recent conversations"
// bullet describes the exact pre-fork discussion that now lives in the fork as
// live history. Rather than tell the model to ignore the mismatch via prompt
// hacks, we transfer the bullet to the fork at fork time: the fork becomes
// the carrier of that discussion, the source earns a fresh bullet on its next
// curation, and the model never sees a misleading log entry.

// TransferBulletOnFork rewrites the source conversation's bullet in
// memory.md so it describes the fork instead: title set to forkTitle, date
// set to today. If no matching bullet is found, it's a no-op (e.g. the
// source had no bullet yet). The transfer is seamless — nothing in the file
// hints at a fork having happened.
func TransferBulletOnFork(srcTitle, forkTitle, today string) error {
	memPath := memoryFilePath()
	content, err := Raw(memPath)
	if err != nil || strings.TrimSpace(content) == "" {
		return err
	}
	updated := transferBullet(content, srcTitle, forkTitle, today)
	if updated == content {
		return nil
	}
	return SaveRaw(memPath, updated)
}

// transferBullet is the pure function tested in isolation: it finds the
// "## Recent conversations" section, locates the bullet whose "· title:"
// matches srcTitle, and rewrites it to forkTitle/today. Returns content
// unchanged if there's no match.
func transferBullet(content, srcTitle, forkTitle, today string) string {
	lines := strings.Split(content, "\n")
	// Find the section header.
	secStart := -1
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "## Recent conversations") {
			secStart = i
			break
		}
	}
	if secStart < 0 {
		return content
	}
	// Section runs until the next ## header or EOF.
	secEnd := len(lines)
	for i := secStart + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			secEnd = i
			break
		}
	}
	for i := secStart + 1; i < secEnd; i++ {
		ln := lines[i]
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "- ") {
			continue
		}
		// Bullet shape: "- {date} · {title}: {rest}"
		rest := strings.TrimPrefix(t, "- ")
		// date is the leading "{YYYY-MM-DD}" (or any non-· token)
		dotIdx := strings.Index(rest, " · ")
		if dotIdx < 0 {
			continue
		}
		titleField := rest[dotIdx+3:]
		colonIdx := strings.Index(titleField, ":")
		if colonIdx < 0 {
			continue
		}
		bulletTitle := strings.TrimSpace(titleField[:colonIdx])
		if bulletTitle != srcTitle {
			continue
		}
		// Found the source bullet: rewrite date and title in place.
		restOfBullet := titleField[colonIdx+1:]
		// Preserve leading indentation/whitespace of the original bullet.
		lead := ln[:strings.Index(ln, "-")]
		lines[i] = lead + "- " + today + " · " + forkTitle + ":" + restOfBullet
		return strings.Join(lines, "\n")
	}
	return content
}

// stateFilePath / memoryFilePath live here so tests can target them via
// config.DataDir() indirectly; they mirror config.MemoryPath() / config.Dir()
// without the import cycle (memory must not import config).
func stateFilePath() string {
	return filepath.Join(configDir(), "memory.state.json")
}
func memoryFilePath() string {
	return filepath.Join(configDir(), "memory.md")
}
func configDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "cx")
}
