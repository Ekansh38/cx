# cx

Terminal AI chat client. Fast, keyboard-first, multi-model. Built with Go + Bubbletea.

## Screenshots

<img width="1507" height="898" alt="Screenshot 2026-07-18 at 6 52 11 PM" src="https://github.com/user-attachments/assets/4594e7e1-ed9b-417e-8f1a-bebc726f027d" />
<img width="598" height="154" alt="Screenshot 2026-07-18 at 6 45 56 PM" src="https://github.com/user-attachments/assets/5cc2b40d-5fc0-45ff-a156-009e330653dc" />
<img width="1510" height="926" alt="Screenshot 2026-07-18 at 6 00 15 PM" src="https://github.com/user-attachments/assets/c0f2c342-3f92-4110-aa8e-b38d194fe729" />


## Install

```bash
git clone https://github.com/Ekansh38/cx.git
cd cx
go build -o cx .
export PATH="$PATH:$(pwd)"   # add to ~/.zshrc or ~/.bashrc
```

## Setup

Create `~/.config/cx/config.toml`:

```toml
model = "anthropic/claude-sonnet-4-5"

[openrouter]
api_key = "sk-or-v1-..."
```

Direct-provider keys work too (`[gemini]`, `[openai]`, `[ollama]`, `[groq]`). Env vars override the file: `OPENROUTER_API_KEY`, `GEMINI_API_KEY`, `OPENAI_API_KEY`, `GROQ_API_KEY`.

## Usage

```bash
cx                # chat
cx doc [file]     # editor + cx side-by-side in tmux
cx vim [file]     # extra doc in nvim with the cx bridge (no split)
cx incognito      # ephemeral chat — no memory, deleted on quit (alias: -i)
```

## Keys

| Key | Action |
|-----|--------|
| `enter` | Send |
| `alt+enter` | Newline (multiline input) |
| `esc` | Dismiss error banner |
| `ctrl+c` | Cancel stream / quit |
| `ctrl+l` | Conversation picker |
| `ctrl+n` | New conversation |
| `ctrl+g` | Search all messages |
| `ctrl+t` | Model picker |
| `ctrl+e` | Open `$EDITOR` for long input |
| `ctrl+u` / `ctrl+d` | Scroll half page |
| `ctrl+r` | Voice dictation (toggle) |
| `up` / `down` | Walk prompt (scroll chat when empty) |
| `tab` | Autocomplete `/command` or `@file` |

## Commands

| Command | Action |
|---------|--------|
| `/help` | This help |
| `/new` · `/list` · `/rename <t>` · `/delete` · `/wipe` | Manage conversations |
| `/retry` (`/r`) · `/edit` · `/stop` | Re-send · edit last · stop stream |
| `/fork` | Pick a past prompt, delete history from it onwards, load into input |
| `/copy [n]` · `/copy prompt [n]` · `/copy all [n]` | Copy recent output |
| `/grep` | Search messages |
| `/model <name>` · `/models` | Switch model / picker |
| `/web [on\|off]` | Toggle agentic web tools |
| `/doc [path]` | Connect a doc + open in editor |
| `/doc edit` · `/doc off` | Reopen · disconnect |
| `/connect doc [path]` · `/disconnect doc` | Connect without opening · disconnect |
| `/sel` · `/sel clear` | Preview / drop editor selection |
| `/undo` | Revert edits from the last user prompt |
| `/img <path> [text]` · `/paste [text]` | Send an image |
| `/memory` · `/remember <fact>` · `/forget <query>` | Memory file ops |
| `/mem [path]` · `/mem list` · `/mem off [path]` | External memory files |
| `/vocab` · `/vocab add <hint>` · `/vocab remove <substr>` | Dictation vocab |
| `/tokens` (or `/stats`) | Token breakdown (system / docs / transcript / total vs limit) + conversation shape |
| `/debug` · `/debug expand` · `/debug collapse` | Payload / verbose modes |

## @file mentions

Type `@` anywhere in a message; `tab` fuzzy-completes over md/txt/image/pdf files under the cwd, with live candidates as you type.

- **Text files** (`.md`, `.txt`) → **connect as docs**. Content is sent every turn and the model **can propose edits** to them via the doc-review flow (see below).
- **Images / PDFs** → **attach to that message** (read-only; can't be edited).
- **Image URLs** (`@https://…png`) → passed through for the provider to fetch.
- **Anything else** (`@ekansh`, `@notes@2024`) → plain text passthrough.

```
fix the intro of @notes.md and match the tone of @draft.md
what's wrong in @screenshot.png
```

## Doc chat

Connect md/txt files with `/doc [path]`, `/connect doc [path]`, or an `@` mention. A chat can hold multiple docs; all are re-read from disk on every message so `:w` in your editor is instantly visible. Reference passages inline: `@L12`, `@L12-30`, `@## Heading`.

**Review flow** (when the model proposes edits):

- Reviewed **in neovim** if the bridge is up, else in cx's chat pane
- cx line-diffs the proposal and splits it into minimal hunks — a typo fix stays a one-liner even if the model rewrites the whole doc
- Proposed lines land as **real green buffer lines** below the **red** originals. Scroll them, search them, tweak them
- On a hunk: `y` apply · `n` skip · `N` reject-with-note · `a` apply all · `q` finish · `u` vim undo. `]q`/`[q` jump between hunks. `N` fires an immediate revision request
- `/undo` in cx reverts everything the last user prompt applied — including retry cascades

**Neovim selections**: add this to your nvim config to send highlighted lines with your next message:

```vim
xnoremap <silent> <leader>cs :<C-u>call writefile(
      \ [expand('%:p'), line("'<").'-'.line("'>")] + getline(line("'<"), line("'>")),
      \ expand('~/.local/share/cx/selection.txt'))<CR>
```

Highlight → `<leader>cs` → switch panes → type your question. Status bar shows `sel L12-30` while a selection is queued. `/sel` previews, `/sel clear` drops.

**Edit-management tools**: the model can also call `discard_pending_edits`, `apply_all_pending_edits`, `apply_pending_edit <n>`, `reject_pending_edit <n>` — a hands-free alternative to `y/n/N/a`.

## Web search

`web_search` and `fetch_url` tools are on by default. The model picks its own queries, runs multiple rounds, and inline-shows `searching the web: "..."` as it works. Search execution reuses your OpenRouter key (cheap grounded model); page reads go through `r.jina.ai`. `/web off` removes the tools.

## Memory

Structured markdown profile at `~/.config/cx/memory.md` — `## Identity · Preferences · Projects · Tools & Workflow · Feedback · References · Recent conversations`.

- **Always live.** Re-read from disk before every message. Edit in another cx or another chat and the next message picks it up.
- **Auto-curated.** After each response the `memory_model` rewrites the whole file — merges facts, prunes stale ones. Overwrites contradictions confidently (age, grade, city, project change → old bullet replaced).
- **Safety cap**: 200 lines. Suspicious shrinks (drops to <1/3 the size) are rejected.
- `/remember` and `/forget` route through the same model. `/memory` shows the file. Direct edits work too.

### External memory files (read-only)

`/mem [path]` attaches any file as read-only context, pasted into the system prompt of every chat. The model gets the path + full contents but is told not to propose edits — you maintain those files yourself.

List lives at `~/.config/cx/external-memory.txt`. Missing files are silently skipped.

## Voice dictation

`ctrl+r` toggles recording. Groq's `whisper-large-v3-turbo` transcribes (sub-second), a fast LLM cleans it up with your custom vocabulary, cleaned text lands in your input.

Custom vocabulary lives at `~/.config/cx/dictation-vocab.txt`:

```
Ekansh (not Ekaansh, Akansh)
Geno (not Gino, Jeno)
```

Manage inline: `/vocab`, `/vocab add <hint>`, `/vocab remove <substr>`. Changes apply on the next `ctrl+r`.

Requires `ffmpeg` (`brew install ffmpeg`) and a `groq.api_key` in `config.toml` (or `GROQ_API_KEY` env). Cleanup uses `dictation_model` if set, else `memory_model`, else `google/gemini-2.5-flash`. 5-min recording cap; <4KB recordings are dropped.

## Multi-instance behavior

Two cx windows on the same conversation don't auto-sync. Reload manually with `/list` + switch, or restart cx. Memory-file curation uses a cross-process `flock` so concurrent rewrites can't clobber each other, so background curation is still safe.

## Incognito

```bash
cx incognito   # or: cx -i
```

Ephemeral chat: **no memory injection, no external memory, no auto-title, no curation**. `/remember` and `/forget` are disabled. Status bar shows `🕶 INCOGNITO`. Conversation is deleted on quit; if cx crashes, the leftover row is titled `(incognito)` for easy cleanup.

## Compaction

Long chats get auto-summarized so older messages stay within context. Configure in `config.toml`:

```toml
memory_model = "google/gemini-2.5-flash"
max_context_tokens = 128000
max_tokens = 16384
```

## Models

Any OpenAI-compatible API works.

- **OpenRouter** (recommended): `anthropic/claude-sonnet-4-5`, `openai/gpt-4o`, `google/gemini-2.5-flash`, …
- **Gemini**: `gemini-2.0-flash`, `gemini-1.5-pro`
- **OpenAI**: `gpt-4o`, `gpt-4o-mini`
- **Ollama**: `llama3.2`, `qwen2.5:32b`, any local model
- **Groq**: whisper for dictation

Switch mid-chat with `/model <name>` or `ctrl+t`.

## Files

- Config: `~/.config/cx/config.toml`
- System prompt: `~/.config/cx/system-prompt.md` (edit / delete to reset)
- Memory: `~/.config/cx/memory.md`
- External memory list: `~/.config/cx/external-memory.txt`
- Dictation vocab: `~/.config/cx/dictation-vocab.txt`
- Editor drafts: `~/.local/share/cx/draft.md`
- Database: `~/.local/share/cx/cx.db` (SQLite, WAL)
