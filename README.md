# cx

Terminal AI chat client. Fast, keyboard-first, multi-model. Go + Bubbletea.

## Screenshots

<img width="1507" height="898" alt="Screenshot 2026-07-18 at 6 52 11 PM" src="https://github.com/user-attachments/assets/4594e7e1-ed9b-417e-8f1a-bebc726f027d" />
<img width="598" height="154" alt="Screenshot 2026-07-18 at 6 45 56 PM" src="https://github.com/user-attachments/assets/5cc2b40d-5fc0-45ff-a156-009e330653dc" />
<img width="1510" height="926" alt="Screenshot 2026-07-18 at 6 00 15 PM" src="https://github.com/user-attachments/assets/c0f2c342-3f92-4110-aa8e-b38d194fe729" />

## Install

```bash
git clone https://github.com/Ekansh38/cx.git
cd cx
go build -o cx .
export PATH="$PATH:$(pwd)"   # add to ~/.zshrc or ~/.bashrc
```

## Setup

`~/.config/cx/config.toml`:

```toml
model = "anthropic/claude-sonnet-4-5"

[openrouter]
api_key = "sk-or-v1-..."
```

Provider blocks: `[openrouter]`, `[gemini]`, `[openai]`, `[ollama]`, `[groq]`. Env vars override: `OPENROUTER_API_KEY`, `GEMINI_API_KEY`, `OPENAI_API_KEY`, `GROQ_API_KEY`.

## Usage

```bash
cx                # chat
cx doc [file]     # editor + cx side-by-side in tmux
cx vim [file]     # extra doc in nvim with the cx bridge (no split)
cx incognito      # ephemeral chat, deleted on quit (alias: -i)
```

## Keys

| Key | Action |
|-----|--------|
| `enter` | Send |
| `alt+enter` | Newline |
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
| `/retry` (`/r`) · `/edit` · `/stop` | Re-send, edit last, stop stream |
| `/fork` | Pick a past prompt, delete history from it onwards, load into input |
| `/copy [n]` · `/copy prompt [n]` · `/copy all [n]` | Copy recent output |
| `/grep` | Search messages |
| `/model <name>` · `/models` | Switch model, picker |
| `/web [on\|off]` | Toggle agentic web tools |
| `/doc [path]` | Connect a doc + open in editor |
| `/doc edit` · `/doc off` | Reopen, disconnect |
| `/connect doc [path]` · `/disconnect doc` | Connect without opening, disconnect |
| `/sel` · `/sel clear` | Preview, drop editor selection |
| `/undo` | Revert edits from the last user prompt |
| `/img <path> [text]` · `/paste [text]` | Send an image |
| `/memory` · `/remember <fact>` · `/forget <query>` | Memory file ops |
| `/mem [path]` · `/mem list` · `/mem off [path]` | External memory files |
| `/vocab` · `/vocab add <hint>` · `/vocab remove <substr>` | Dictation vocab |
| `/tokens` (or `/stats`) | Token breakdown vs limit |
| `/debug` · `/debug expand` · `/debug collapse` | Payload, verbose modes |

## @file mentions

Type `@` anywhere; `tab` fuzzy-completes over md/txt/image/pdf files under the cwd.

- **Text files** (`.md`, `.txt`): connect as docs. Content sent every turn; model can propose edits (see doc-review flow).
- **Images / PDFs**: attach to that message, read-only.
- **Image URLs** (`@https://...png`): passed through for the provider to fetch.
- **Anything else** (`@ekansh`, `@notes@2024`): plain text passthrough.

```
fix the intro of @notes.md and match the tone of @draft.md
what's wrong in @screenshot.png
```

## Doc chat

Connect md/txt files with `/doc [path]`, `/connect doc [path]`, or an `@` mention. Multi-doc per chat. All re-read from disk on every message so `:w` is instantly visible. Passages: `@L12`, `@L12-30`, `@## Heading`.

Review flow (when the model proposes edits):

- In neovim if the bridge is up, else in cx's chat pane.
- cx line-diffs the proposal, splits it into minimal hunks.
- Proposed lines land as real green buffer lines below the red originals.
- Hunk keys: `y` apply, `n` skip, `N` reject-with-note, `a` apply all, `q` finish, `u` vim undo. `]q` / `[q` jump between hunks. `N` fires an immediate revision request.
- `/undo` in cx reverts everything the last user prompt applied, retries included.

Neovim visual-mode selection binding:

```vim
xnoremap <silent> <leader>cs :<C-u>call writefile(
      \ [expand('%:p'), line("'<").'-'.line("'>")] + getline(line("'<"), line("'>")),
      \ expand('~/.local/share/cx/selection.txt'))<CR>
```

Highlight, press `<leader>cs`, switch panes, type your question. Status bar shows `sel L12-30` while queued. `/sel` previews, `/sel clear` drops.

Edit-management tools the model can call: `discard_pending_edits`, `apply_all_pending_edits`, `apply_pending_edit <n>`, `reject_pending_edit <n>`.

## Web search

`web_search` and `fetch_url` tools on by default. Multi-round. Inline `searching the web: "..."` as it works. Uses your OpenRouter key for search; page reads via `r.jina.ai`. `/web off` disables.

## Memory

Structured markdown profile at `~/.config/cx/memory.md`: `## Identity · Preferences · Projects · Tools & Workflow · Feedback · References · Recent conversations`.

- Re-read from disk before every message.
- After each response the `memory_model` rewrites the file, merges facts, prunes stale ones, overwrites contradictions.
- 200-line cap. Suspicious shrinks (<1/3 the size) rejected.
- `/remember` and `/forget` route through the same model. `/memory` shows the file. Direct edits work.

### External memory files (read-only)

`/mem [path]` attaches any file as read-only context, pasted into every chat's system prompt. Model gets path + full contents, told not to propose edits.

List: `~/.config/cx/external-memory.txt`. Missing files silently skipped.

## Voice dictation

`ctrl+r` toggles recording. Groq `whisper-large-v3-turbo` transcribes, a fast LLM cleans it up with your custom vocab, cleaned text lands in your input.

Vocab: `~/.config/cx/dictation-vocab.txt`:

```
Ekansh (not Ekaansh, Akansh)
Geno (not Gino, Jeno)
```

Inline: `/vocab`, `/vocab add <hint>`, `/vocab remove <substr>`. Picked up on next `ctrl+r`.

Requires `ffmpeg` (`brew install ffmpeg`) and `groq.api_key` in `config.toml` (or `GROQ_API_KEY` env). Cleanup model: `dictation_model`, else `memory_model`, else `google/gemini-2.5-flash`. 5-min cap. <4KB recordings dropped.

## Multi-instance

Two cx windows on the same conversation don't auto-sync. `/list` + switch to reload. Memory curation uses a cross-process `flock` so concurrent rewrites can't clobber each other.

## Incognito

```bash
cx incognito   # or: cx -i
```

No memory injection, no external memory, no auto-title, no curation. `/remember` and `/forget` disabled. Status bar shows `INCOGNITO`. Conversation deleted on quit; crashed leftovers titled `(incognito)`.

## Compaction

Long chats auto-summarize older messages. `config.toml`:

```toml
memory_model = "google/gemini-2.5-flash"
max_context_tokens = 128000
max_tokens = 16384
```

## Models

Any OpenAI-compatible API.

- OpenRouter: `anthropic/claude-sonnet-4-5`, `openai/gpt-4o`, `google/gemini-2.5-flash`
- Gemini: `gemini-2.0-flash`, `gemini-1.5-pro`
- OpenAI: `gpt-4o`, `gpt-4o-mini`
- Ollama: `llama3.2`, `qwen2.5:32b`, any local
- Groq: whisper for dictation

Switch mid-chat with `/model <name>` or `ctrl+t`.

## Files

- Config: `~/.config/cx/config.toml`
- System prompt: `~/.config/cx/system-prompt.md` (edit or delete to reset)
- Memory: `~/.config/cx/memory.md`
- External memory list: `~/.config/cx/external-memory.txt`
- Dictation vocab: `~/.config/cx/dictation-vocab.txt`
- Editor drafts: `~/.local/share/cx/draft.md`
- Database: `~/.local/share/cx/cx.db` (SQLite, WAL)
