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
cx                # chat
cx doc            # fuzzy-pick a doc, then neovim + cx side by side
cx doc notes.md   # same, with the file given directly
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
| `:doc [path]` | Attach a document to discuss/edit (no path = picker) |
| `:doc off` | Close the attached document |
| `:memory` | Show current memory file |
| `:debug` | Show full API payload |
| `:wipe` | Delete all data (asks confirm) |

### Doc chat

Attach a markdown/text file with `:doc` (fuzzy picker over the current directory) or `:doc <path>`. The document renders in a left pane with line numbers; chat lives on the right. The full file is sent to the model every turn, so you can just talk about it — reference passages as `@L12`, `@L12-30`, or `@## Heading`.

- `ctrl+o` focuses the doc pane: `j/k` `u/d` `g/G` scroll, `e` opens the file in `$EDITOR` (reloads on exit), `r` reloads, `esc` back to chat
- When the model proposes changes, each edit shows as a diff: `y` apply, `n` skip, `a` apply all, `esc` cancel — accepted edits are written straight to the file
- The attachment persists with the conversation; `:doc off` closes it

#### Neovim side-by-side

One command sets the whole thing up:

```bash
cx doc            # fuzzy-pick from md/txt files under the current directory
cx doc notes.md   # or name the file directly
```

That opens a tmux session with neovim on the left, cx on the right, and the doc attached (reusing tmux/the doc's previous conversation if they exist). The picker matches fuzzily — `nts` finds `notes.md`. Inside cx, `:doc`, the doc picker, and `e` also auto-open the editor in a split when you're in tmux.

cx re-reads the doc from disk on every message, so every `:w` in neovim is instantly visible to the model. To also send it *what you've highlighted*, add this to your neovim config:

```vim
" visual mode: send the selection to cx
xnoremap <silent> <leader>cs :<C-u>call writefile(
      \ [expand('%:p'), line("'<").'-'.line("'>")] + getline(line("'<"), line("'>")),
      \ expand('~/.local/share/cx/selection.txt'))<CR>
```

Workflow: highlight lines in neovim → `<leader>cs` → switch panes → just type your question. cx attaches the highlighted passage to that message (auto-attaching the file with `:doc` if you hadn't). The status bar shows `sel L12-30` while a selection is waiting; `:sel` previews it, `:sel clear` drops it.

Tip: if cx applies edits while the file is open in neovim, set `:set autoread` so neovim picks them up.

### Memory

cx keeps a structured markdown profile at `~/.config/cx/memory.md`, organized into sections like **Identity**, **Preferences**, **Projects**, **Tools & Workflow**, **Feedback**, **References**, and **Recent conversations** — not a flat list of bullet points. The Recent conversations section is an episodic log (date · title · what was decided) so new sessions know what you've already discussed.

After every response, the configured `memory_model` re-reads the file plus the latest exchange and **rewrites the whole file** — merging new facts into the right sections, generalizing patterns, and pruning stale details. The result is injected into every conversation's system prompt.

`:remember <fact>` and `:forget <query>` also route through the model, so manual edits stay organized in the same structure. `:memory` shows the current file. You can also edit `memory.md` directly if you want.

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
