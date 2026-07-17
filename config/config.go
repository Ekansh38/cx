package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Model             string     `toml:"model"`
	MemoryModel       string     `toml:"memory_model"`
	DictationModel    string     `toml:"dictation_model"`     // LLM used to clean up voice transcripts (defaults to MemoryModel)
	MaxContextTokens  int        `toml:"max_context_tokens"`
	MaxTokens         int        `toml:"max_tokens"`          // max response tokens per request
	MemoryInterval    int        `toml:"memory_interval"`     // fire memory curation after N new turns
	MemoryIdleSeconds int        `toml:"memory_idle_seconds"` // ...or after this many idle seconds, whichever first
	Gemini            ProviderCf `toml:"gemini"`
	OpenAI            ProviderCf `toml:"openai"`
	Ollama            ProviderCf `toml:"ollama"`
	OpenRouter        ProviderCf `toml:"openrouter"`
	Groq              ProviderCf `toml:"groq"` // used only for whisper-based dictation STT
}

type ProviderCf struct {
	APIKey  string `toml:"api_key"`
	BaseURL string `toml:"base_url"`
}

func Load() (*Config, error) {
	cfg := &Config{
		Model:             "", // no default — uses most recent conversation's model
		MemoryModel:       "google/gemini-2.5-flash",
		MaxContextTokens:  128000,
		MaxTokens:         16384,
		MemoryInterval:    6,
		MemoryIdleSeconds: 120,
		Ollama:            ProviderCf{BaseURL: "http://localhost:11434/v1"},
	}

	path := filepath.Join(Dir(), "config.toml")
	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			return nil, err
		}
	}

	// Env vars always win over file
	if k := os.Getenv("GEMINI_API_KEY"); k != "" {
		cfg.Gemini.APIKey = k
	}
	if k := os.Getenv("OPENAI_API_KEY"); k != "" {
		cfg.OpenAI.APIKey = k
	}
	if k := os.Getenv("OPENROUTER_API_KEY"); k != "" {
		cfg.OpenRouter.APIKey = k
	}
	if k := os.Getenv("GROQ_API_KEY"); k != "" {
		cfg.Groq.APIKey = k
	}

	if cfg.Ollama.BaseURL == "" {
		cfg.Ollama.BaseURL = "http://localhost:11434/v1"
	}

	return cfg, nil
}

// Dir returns ~/.config/cx, creating it if needed.
func Dir() string {
	home, _ := os.UserHomeDir()
	d := filepath.Join(home, ".config", "cx")
	os.MkdirAll(d, 0o755)
	return d
}

// DataDir returns ~/.local/share/cx, creating it if needed.
func DataDir() string {
	home, _ := os.UserHomeDir()
	d := filepath.Join(home, ".local", "share", "cx")
	os.MkdirAll(d, 0o755)
	return d
}

// MemoryPath returns the path to the memory file.
func MemoryPath() string {
	return filepath.Join(Dir(), "memory.md")
}

// LastModel returns the model the user most recently selected ("" if none).
func LastModel() string {
	data, err := os.ReadFile(filepath.Join(DataDir(), "last-model.txt"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// SaveLastModel remembers an explicit model selection across sessions.
func SaveLastModel(model string) {
	os.WriteFile(filepath.Join(DataDir(), "last-model.txt"), []byte(model+"\n"), 0o644)
}

// LoadMemory reads memory.md if it exists, returning its content.
func LoadMemory() string {
	data, err := os.ReadFile(MemoryPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// ExternalMemoryPath returns the path to the external memory list file.
// One absolute path per line; each listed file is auto-attached to every chat
// as a persistent memory doc (contents included, edits propose-able).
func ExternalMemoryPath() string {
	return filepath.Join(Dir(), "external-memory.txt")
}

// LoadExternalMemoryPaths returns the absolute paths listed in
// external-memory.txt. Entries that no longer resolve to a file are silently
// dropped (so a deleted vault file doesn't error every turn).
func LoadExternalMemoryPaths() []string {
	data, err := os.ReadFile(ExternalMemoryPath())
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, ln := range strings.Split(string(data), "\n") {
		p := strings.TrimSpace(ln)
		if p == "" || seen[p] {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// saveExternalMemoryPaths writes the deduped, order-preserved list.
func saveExternalMemoryPaths(paths []string) error {
	if len(paths) == 0 {
		return os.WriteFile(ExternalMemoryPath(), []byte{}, 0o644)
	}
	return os.WriteFile(ExternalMemoryPath(), []byte(strings.Join(paths, "\n")+"\n"), 0o644)
}

// ResolveExternalMemoryPath expands ~ and turns a relative path into an
// absolute one — same shape as user-typed paths elsewhere.
func ResolveExternalMemoryPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "~") {
		home, _ := os.UserHomeDir()
		p = home + p[1:]
	}
	return filepath.Abs(p)
}

// AddExternalMemoryPath resolves and adds path if the file exists and isn't
// already listed. Returns the absolute path that was recorded.
func AddExternalMemoryPath(path string) (string, error) {
	abs, err := ResolveExternalMemoryPath(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", err
	}
	paths := LoadExternalMemoryPaths()
	for _, p := range paths {
		if p == abs {
			return abs, nil
		}
	}
	paths = append(paths, abs)
	return abs, saveExternalMemoryPaths(paths)
}

// RemoveExternalMemoryPath drops path from the list (accepts either the
// absolute stored form or a user-typed variant).
func RemoveExternalMemoryPath(path string) error {
	abs, _ := ResolveExternalMemoryPath(path)
	paths := LoadExternalMemoryPaths()
	kept := paths[:0]
	for _, p := range paths {
		if p == abs || p == path {
			continue
		}
		kept = append(kept, p)
	}
	return saveExternalMemoryPaths(kept)
}

// DictationVocabPath returns the path to the dictation vocab file. The file
// carries proper-noun corrections and other custom vocabulary hints for the
// LLM cleanup pass — one line per hint, e.g. `Ekansh (not Ekaansh)`.
func DictationVocabPath() string {
	return filepath.Join(Dir(), "dictation-vocab.txt")
}

// LoadDictationVocab returns the vocab hints, seeding the file with sensible
// defaults on first read.
const defaultDictationVocab = `# Custom vocabulary for voice dictation. One hint per line.
# The LLM cleanup pass applies these when it recognizes the intended word.
# Lines starting with # are ignored.

Ekansh (not Ekaansh, Akansh, Akansha, Ikansh)
Geno (not Gino, Jeno, Jinno, Zeno)
`

func LoadDictationVocab() string {
	path := DictationVocabPath()
	data, err := os.ReadFile(path)
	if err != nil {
		os.WriteFile(path, []byte(defaultDictationVocab), 0o644)
		return defaultDictationVocab
	}
	return string(data)
}

// SaveDictationVocab overwrites the vocab file with the given text.
func SaveDictationVocab(text string) error {
	return os.WriteFile(DictationVocabPath(), []byte(text), 0o644)
}

// AppendDictationVocab appends line as a new entry, seeding defaults first
// if the file didn't exist. Trims whitespace and duplicate empty lines.
func AppendDictationVocab(line string) error {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	current := LoadDictationVocab() // seeds defaults on first call
	// Guarantee we're on a fresh line — the appended entry shouldn't glue
	// onto whatever was there before.
	if !strings.HasSuffix(current, "\n") {
		current += "\n"
	}
	return SaveDictationVocab(current + line + "\n")
}
