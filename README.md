# cx

Terminal AI chat client. Fast, keyboard-first, multi-model. Built with Go + Bubbletea.

## Install

```bash
git clone https://github.com/Ekansh38/cx.git
cd cx
go build -o cx .

# Add to PATH (add to ~/.zshrc or ~/.bashrc)
export PATH="$PATH:$(pwd)"
```

## Setup

Create `~/.config/cx/config.toml`:

```toml
model = "anthropic/claude-sonnet-4-5"

[openrouter]
api_key = "sk-or-v1-..."
```

Or use direct provider keys:

```toml
model = "gemini-2.0-flash"

[gemini]
api_key = "..."
```

Env vars override config: `OPENROUTER_API_KEY`, `GEMINI_API_KEY`, `OPENAI_API_KEY`.

## Usage

```bash
cx
```

### Keybindings

| Key | Action |
|-----|--------|
| `enter` | Send message |
| `alt+enter` | Newline (multiline input) |
| `esc` | Clear input line |
| `ctrl+c` | Cancel stream / quit |
| `ctrl+l` | Conversation picker |
| `ctrl+n` | New conversation |
| `ctrl+g` | Search all messages |
| `ctrl+t` | Model switcher |
| `ctrl+e` | Open `$EDITOR` for long input |
| `ctrl+u/d` | Scroll half page |
| `tab` | Autocomplete `:command` |

### Commands

| Command | Action |
|---------|--------|
| `:help` | Show all keybinds + commands |
| `:q` | Quit |
| `:new` | New conversation |
| `:list` | Conversation picker |
| `:edit` | Edit your last message |
| `:stop` | Stop streaming response |
| `:delete` | Delete current conversation (confirms) |
| `:grep` | Search all messages |
| `:copy` | Copy last response to clipboard |
| `:retry` / `:r` | Re-send last message |
| `:img <path> [text]` | Send an image |
| `:rename <title>` | Rename conversation |
| `:model <name>` | Switch model |
| `:models` | Model picker (fetches from OpenRouter) |
| `:remember <fact>` | Save to memory |
| `:forget <query>` | Remove from memory |
| `:paste [text]` | Paste image from clipboard |
| `:memory` | Show current memory file |
| `:debug` | Show full API payload |
| `:wipe` | Delete all data (asks confirm) |

### Memory

cx auto-learns about you. After each response, a background model curates `~/.config/cx/memory.md` — merging, generalizing, and pruning facts into organized markdown sections. Memory is injected into every conversation's system prompt.

Manual control with `:remember` and `:forget`. View with `:memory`. Edit `memory.md` directly if you want.

### Context Compaction

When conversations get long, cx automatically summarizes older messages to stay within the context window. Recent messages stay verbatim.

Configure in `config.toml`:

```toml
memory_model = "google/gemini-2.0-flash-001"  # model for memory curation + compaction
max_context_tokens = 128000
max_tokens = 16384  # max output tokens per response
```

### Models

cx works with any OpenAI-compatible API:

- **OpenRouter** (recommended): Models like `anthropic/claude-sonnet-4-5`, `openai/gpt-4o`, `google/gemini-2.0-flash-001`
- **Gemini**: `gemini-2.0-flash`, `gemini-1.5-pro`
- **OpenAI**: `gpt-4o`, `gpt-4o-mini`
- **Ollama**: Any local model (`llama3.2`, `qwen2.5:32b`, etc.)

Switch mid-conversation with `:model <name>` or use the picker with `ctrl+t`.

### Data

- Config: `~/.config/cx/config.toml`
- Memory: `~/.config/cx/memory.md`
- Database: `~/.local/share/cx/cx.db` (SQLite)
