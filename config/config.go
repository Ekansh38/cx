package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Model            string     `toml:"model"`
	MemoryModel      string     `toml:"memory_model"`
	MaxContextTokens int        `toml:"max_context_tokens"`
	Gemini           ProviderCf `toml:"gemini"`
	OpenAI           ProviderCf `toml:"openai"`
	Ollama           ProviderCf `toml:"ollama"`
	OpenRouter       ProviderCf `toml:"openrouter"`
}

type ProviderCf struct {
	APIKey  string `toml:"api_key"`
	BaseURL string `toml:"base_url"`
}

func Load() (*Config, error) {
	cfg := &Config{
		Model:            "llama3.2",
		MemoryModel:      "google/gemini-2.0-flash-001",
		MaxContextTokens: 128000,
		Ollama:           ProviderCf{BaseURL: "http://localhost:11434/v1"},
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

// LoadMemory reads memory.md if it exists, returning its content.
func LoadMemory() string {
	data, err := os.ReadFile(MemoryPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
