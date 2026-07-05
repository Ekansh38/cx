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
| `tab` | Autocomplete `/command` or `@file` |

### Commands

| Command | Action |
|---------|--------|
| `/help` | Show all keybinds + commands |
| `/quit` (or `:q`) | Quit |
| `/new` | New conversation |
| `/list` | Conversation picker |
| `/edit` | Edit your last message |
| `/stop` | Stop streaming response |
| `/delete` | Delete current conversation (confirms) |
| `/grep` | Search all messages |
| `/copy` | Copy last response to clipboard |
| `/copy prompt` | Copy your last message to clipboard |
| `/retry` / `/r` | Re-send last message |
| `/img <path> [text]` | Send an image |
| `/rename <title>` | Rename conversation |
| `/model <name>` | Switch model |
| `/models` | Model picker (fetches from OpenRouter) |
| `/remember <fact>` | Save to memory |
| `/forget <query>` | Remove from memory |
| `/paste [text]` | Paste image from clipboard |
| `/doc [path]` | Connect a doc and open it in your editor (no path = fuzzy picker) |
| `/doc edit` | Reopen a connected doc in your editor |
| `/doc off` | Disconnect (same as `/disconnect doc`) |
| `/connect doc [path]` | Connect a doc without opening the editor (no path = last doc) |
| `/disconnect doc` | Disconnect a doc (picker with ALL when several) |
| `/memory` | Show current memory file |
| `/debug` | Show full API payload |
| `/wipe` | Delete all data (asks confirm) |

### @file mentions

Mention a file anywhere in a message with `@` — tab fuzzy-completes against md/txt/image files under the current directory, with candidates shown live as you type. On send, text files **connect as docs** and images **attach to the message**. `@` that doesn't resolve to a file (emails, handles) passes through as plain text.

```
fix the intro of @notes.md and make it match the tone of @draft.md
what's wrong in @screenshot.png
```

### Doc chat

Connect markdown/text files with `/doc` (fuzzy picker over the current directory), `/doc <path>`, or `/connect doc [path]` — with no path, `/connect doc` reuses the last doc you connected (cx remembers it across sessions). A chat can have **multiple docs connected**; all of them are sent to the model every turn (re-read from disk, so every save is picked up). Documents live in *your editor* — inside tmux, `/doc` opens them beside cx automatically; for extra docs just open them in neovim yourself, or use `/doc edit` to open a connected one with the cx bridge wired up.

Reference passages as `@L12`, `@L12-30`, or `@## Heading`. Disconnect with `/disconnect doc` — instant with one doc, a fuzzy picker (including an ALL option) with several.

- When the model proposes changes, the review happens **in neovim**. cx line-diffs whatever the model sends and splits it into minimal hunks — even if the model rewrites the whole document, you review a handful of small diffs, not one giant one. The proposed text is inserted as **real buffer lines** (green) below the lines it replaces (red) — scroll it, search it, even tweak it before deciding. Every hunk fills the quickfix list, so `]q` / `[q` jump between edits. With the cursor on a hunk: `y` apply, `n` skip, `N` reject with a note, `a` apply all, `u` undo your last decision (brings the diff back), `q` quit. The file saves when the review ends; applied hunks sit in the normal undo tree, so plain `u` after the review undoes them like any other edit. Pressing `N` fires the revision request **immediately** — the model starts reworking that edit while you keep reviewing the rest, and its new proposal queues up behind the current review.
- cx spawns neovim with an RPC socket (`--listen`), which also powers hot reload: when cx writes the file, your buffer refreshes instantly.
- Without the neovim bridge (different `$EDITOR`, no socket), the y/n review falls back to cx's chat pane.
- Connections persist with the conversation; reopening a chat reconnects its docs

#### Neovim side-by-side

One command sets the whole thing up:

```bash
cx doc            # fuzzy-pick from md/txt files under the current directory
cx doc notes.md   # or name the file directly
```

That opens a tmux session with neovim on the left, cx on the right, and the doc connected (reusing tmux/the doc's previous conversation if they exist). The picker matches fuzzily — `nts` finds `notes.md`. Inside cx, `/doc` and `/doc edit` also auto-open the editor in a split when you're in tmux.

cx re-reads the doc from disk on every message, so every `:w` in neovim is instantly visible to the model. To also send it *what you've highlighted*, add this to your neovim config:

```vim
" visual mode: send the selection to cx
xnoremap <silent> <leader>cs :<C-u>call writefile(
      \ [expand('%:p'), line("'<").'-'.line("'>")] + getline(line("'<"), line("'>")),
      \ expand('~/.local/share/cx/selection.txt'))<CR>
```

Workflow: highlight lines in neovim → `<leader>cs` → switch panes → just type your question. cx attaches the highlighted passage to that message (auto-attaching the file with `/doc` if you hadn't). The status bar shows `sel L12-30` while a selection is waiting; `/sel` previews it, `/sel clear` drops it.

Tip: if cx applies edits while the file is open in neovim, set `:set autoread` so neovim picks them up.

### Memory

cx keeps a structured markdown profile at `~/.config/cx/memory.md`, organized into sections like **Identity**, **Preferences**, **Projects**, **Tools & Workflow**, **Feedback**, **References**, and **Recent conversations** — not a flat list of bullet points. The Recent conversations section is an episodic log (date · title · what was decided) so new sessions know what you've already discussed.

After every response, the configured `memory_model` re-reads the file plus the latest exchange and **rewrites the whole file** — merging new facts into the right sections, generalizing patterns, and pruning stale details. The result is injected into every conversation's system prompt.

`/remember <fact>` and `/forget <query>` also route through the model, so manual edits stay organized in the same structure. `/memory` shows the current file. You can also edit `memory.md` directly if you want.

### Context Compaction

When conversations get long, cx automatically summarizes older messages to stay within the context window. Recent messages stay verbatim.

Configure in `config.toml`:

```toml
memory_model = "google/gemini-2.5-flash"  # model for memory curation + compaction
max_context_tokens = 128000
max_tokens = 16384  # max output tokens per response
```

### Models

cx works with any OpenAI-compatible API:

- **OpenRouter** (recommended): Models like `anthropic/claude-sonnet-4-5`, `openai/gpt-4o`, `google/gemini-2.5-flash`
- **Gemini**: `gemini-2.0-flash`, `gemini-1.5-pro`
- **OpenAI**: `gpt-4o`, `gpt-4o-mini`
- **Ollama**: Any local model (`llama3.2`, `qwen2.5:32b`, etc.)

Switch mid-conversation with `/model <name>` or use the picker with `ctrl+t`.

### Data

- Config: `~/.config/cx/config.toml`
- Memory: `~/.config/cx/memory.md`
- Database: `~/.local/share/cx/cx.db` (SQLite)
