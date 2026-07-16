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
cx doc            # fuzzy-pick a doc, open it in neovim beside cx
cx doc notes.md   # same, with the file given directly
                  # (opening does NOT connect: /connect doc attaches it)
cx vim            # fuzzy-pick among the current chat's connected docs (or
                  # files in the cwd) and open one in neovim with the cx
                  # bridge — no tmux split. handy when several docs are
                  # connected to one chat
cx vim notes.md   # same, with the file given directly
```

### Keybindings

| Key | Action |
|-----|--------|
| `enter` | Send message |
| `alt+enter` | Newline (multiline input; the box grows as you type, and big pastes collapse to `[paste #N, X lines]`) |
| `esc` | Dismiss a stuck error banner (never wipes the input) |
| `up/down` | Move through your prompt (scrolls chat when empty) |
| `ctrl+c` | Cancel stream / quit |
| `ctrl+l` | Conversation picker |
| `ctrl+n` | New conversation |
| `ctrl+g` | Search all messages |
| `ctrl+t` | Model switcher |
| `ctrl+e` | Open `$EDITOR` for long input |
| `ctrl+u/d` | Scroll half page |
| `ctrl+r` | Voice dictation (toggle) — records mic, transcribes via Groq Whisper, cleans up disfluencies + custom vocabulary, inserts into input |
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
| `/delete` | Delete the current conversation |
| `/grep` | Search all messages |
| `/copy [n]` | Copy the last n responses (default 1) |
| `/copy prompt [n]` | Copy your last n messages |
| `/copy all [n]` | Copy the last n prompt/response pairs |
| `/retry` / `/r` | Re-send last message |
| `/fork` | Fuzzy-pick a past prompt, DELETE everything from it onwards in this chat, and load it back into the input for redo (rewrites history in place — no new conversation) |
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
| `/undo` | Revert the edits applied by the last review |
| `/web [on|off]` | Toggle live web search (OpenRouter models) |
| `/connect doc [path]` | Connect a doc without opening the editor (no path = last doc) |
| `/disconnect doc` | Disconnect a doc (picker with ALL when several) |
| `/memory` | Show current memory file |
| `/debug` | Show full API payload |
| `/debug expand` | Verbose mode: full notes, memory + context events |
| `/debug collapse` | Back to the clean default |
| `/wipe` | Delete all data (asks confirm) |

### @file mentions

Mention a file anywhere in a message with `@` — tab fuzzy-completes against md/txt/image/pdf files under the current directory, with candidates shown live as you type. On send, text files **connect as docs**, images and **PDFs attach to the message** (sent as native document input — models read them directly), and image URLs (`@https://site.com/pic.png`) pass straight through for the provider to fetch. `@` that doesn't resolve to a file (emails, handles) passes through as plain text.

```
fix the intro of @notes.md and make it match the tone of @draft.md
what's wrong in @screenshot.png
```

### Doc chat

Connect markdown/text files with `/doc` (fuzzy picker over the current directory), `/doc <path>`, or `/connect doc [path]` — with no path, `/connect doc` reuses the last doc you connected (cx remembers it across sessions). A chat can have **multiple docs connected**; all of them are sent to the model every turn (re-read from disk, so every save is picked up). Documents live in *your editor* — inside tmux, `/doc` opens them beside cx automatically; for extra docs just open them in neovim yourself, or use `/doc edit` to open a connected one with the cx bridge wired up.

Reference passages as `@L12`, `@L12-30`, or `@## Heading`. Disconnect with `/disconnect doc` — instant with one doc, a fuzzy picker (including an ALL option) with several.

- When the model proposes changes, the review happens **in neovim**. cx line-diffs whatever the model sends and splits it into minimal hunks — even if the model rewrites the whole document, you review a handful of small diffs, not one giant one. The proposed text is inserted as **real buffer lines** (green) below the lines it replaces (red) — scroll it, search it, even tweak it before deciding. Every hunk fills the quickfix list, so `]q` / `[q` jump between edits. With the cursor on a hunk: `y` apply, `n` skip, `N` reject with a note, `a` apply all, `q` finish. Undo is just **vim's `u`** — decisions are ordinary buffer edits, and undoing one brings the full diff (highlights, footer, quickfix entry) right back. After a review is finished, `/undo` in cx reverts everything it applied. The file saves when the review ends; applied hunks sit in the normal undo tree, so plain `u` after the review undoes them like any other edit. Pressing `N` fires the revision request **immediately** — the model starts reworking that edit while you keep reviewing the rest, and its new proposal queues up behind the current review.
- cx spawns neovim with an RPC socket (`--listen`), which also powers hot reload: when cx writes the file, your buffer refreshes instantly.
- Without the neovim bridge (different `$EDITOR`, no socket), the y/n review falls back to cx's chat pane.
- Connections persist with the conversation; reopening a chat reconnects its docs

#### Neovim side-by-side

One command sets the whole thing up:

```bash
cx doc            # fuzzy-pick from md/txt files under the current directory
cx doc notes.md   # or name the file directly
```

That opens a tmux session with neovim on the left and cx on the right. The doc is remembered but NOT connected to the conversation: type `/connect doc` (no path needed) when you want it in context. The picker matches fuzzily — `nts` finds `notes.md`. Inside cx, `/doc` and `/doc edit` also auto-open the editor in a split when you're in tmux.

cx re-reads the doc from disk on every message, so every `:w` in neovim is instantly visible to the model. To also send it *what you've highlighted*, add this to your neovim config:

```vim
" visual mode: send the selection to cx
xnoremap <silent> <leader>cs :<C-u>call writefile(
      \ [expand('%:p'), line("'<").'-'.line("'>")] + getline(line("'<"), line("'>")),
      \ expand('~/.local/share/cx/selection.txt'))<CR>
```

Workflow: highlight lines in neovim → `<leader>cs` → switch panes → just type your question. cx attaches the highlighted passage to that message (auto-attaching the file with `/doc` if you hadn't). The status bar shows `sel L12-30` while a selection is waiting; `/sel` previews it, `/sel clear` drops it.

Tip: if cx applies edits while the file is open in neovim, set `:set autoread` so neovim picks them up.

### Web search

### Edit-management tools

While reviewing edits in neovim, the model can also **discard** its own pending edits or **apply them all** through tool calls. If you redirect mid-review ("wait, let's discuss this instead"), the model will typically call `discard_pending_edits`, wipe its proposals from your editor, and answer in prose. Say "yes, apply everything" and it'll call `apply_all_pending_edits`. You can still `y/n/N` yourself — the tools are a hands-free alternative.

### Web search

Web search is agentic and on by default: the model gets `web_search` and `fetch_url` tools, decides when to use them, crafts its own queries, and can run several rounds — you see `searching the web: "best cafes singapore 2026"` inline as it works, and the searches stay in the transcript for follow-up context. Search execution reuses your OpenRouter key (a cheap grounded model does the lookup); page reading goes through the free r.jina.ai reader. `/web off` removes the tools (status bar shows `no-web`).

### Memory

cx keeps a structured markdown profile at `~/.config/cx/memory.md`, organized into sections like **Identity**, **Preferences**, **Projects**, **Tools & Workflow**, **Feedback**, **References**, and **Recent conversations** — not a flat list of bullet points. The Recent conversations section is an episodic log (date · title · what was decided) so new sessions know what you've already discussed.

**Always live.** cx re-reads `memory.md` from disk before *every* message you send. So if you `/remember` something in one chat, then switch back to a chat that was already open, the next message picks up the new memory without a restart.

**Confident updates.** After each response, the configured `memory_model` reads the file plus the latest exchange and **rewrites the whole file** — merging new facts into the right sections, generalizing patterns, and pruning stale details. When you contradict something (age changed, graduated, moved cities, dropped a project, changed a preference), the memory model **overwrites** the old bullet instead of stacking a contradiction next to it. If a fact goes clearly out of date, it gets deleted.

`/remember <fact>` and `/forget <query>` also route through the model, so manual edits stay organized in the same structure. `/memory` shows the current file. You can also edit `memory.md` directly — the next message picks up your edits.

### External memory files

`/mem [path]` attaches an arbitrary file as read-only external memory: its contents get pasted into the system prompt of *every* conversation, so cx knows about your life-notes, curriculum, vault entries, whatever. The model is explicitly told not to propose edits — you maintain those files yourself.

- `/mem` — fuzzy picker over `.md`/`.txt` files under the cwd
- `/mem <path>` — attach directly (tilde and relatives resolve)
- `/mem list` — show currently attached
- `/mem off [path]` — detach (picker if no path given)

The list lives at `~/.config/cx/external-memory.txt` (one absolute path per line). Files that no longer resolve are silently skipped.

### Voice dictation

Press `ctrl+r` to start recording, `ctrl+r` again to stop. cx captures the default mic with `ffmpeg`, sends the WAV to **Groq's `whisper-large-v3-turbo`** for transcription (sub-second latency), and pipes the raw transcript through a fast LLM cleanup pass (fixes disfluencies, applies your custom vocabulary, punctuates) before dropping the text into your input.

Custom vocabulary — proper nouns, easily-misheard words — lives at `~/.config/cx/dictation-vocab.txt`, one line per hint:

```
Ekansh (not Ekaansh, Akansh)
Geno (not Gino, Jeno)
```

The file is created with sensible defaults on first run.

Requires:
- `ffmpeg` on `$PATH` (`brew install ffmpeg`)
- Groq API key (`groq.api_key` in `config.toml` or `GROQ_API_KEY` env). Get one at [console.groq.com](https://console.groq.com) — the free tier covers casual use.

Cleanup uses `dictation_model` in `config.toml` if set, else `memory_model`, else `google/gemini-2.5-flash`.

Recordings are hard-capped at 5 minutes. Recordings under 4KB (accidentally-tapped ctrl+r twice) are dropped silently.

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
- System prompt: `~/.config/cx/system-prompt.md` (edit freely; delete to reset to the default)
- Memory: `~/.config/cx/memory.md`
- Editor drafts: `~/.local/share/cx/draft.md` (persists after ctrl+e sessions, never deleted)
- Database: `~/.local/share/cx/cx.db` (SQLite)
