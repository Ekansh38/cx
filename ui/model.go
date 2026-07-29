package ui

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"cx/config"
	"cx/llm"
	"cx/memory"
	"cx/store"
)

// ── states ───────────────────────────────────────────────────────────────────

type appState int

const (
	stateChat          appState = iota
	statePicker                 // conversation picker overlay
	stateSearch                 // inline message search
	stateModelPicker            // model switcher
	stateDocPicker              // filesystem document picker for /doc
	stateDocListPicker          // connected-docs picker (/disconnect doc / /doc edit)
	stateForkPicker             // past-prompt picker for /fork
	stateMemAddPicker           // filesystem picker for /mem
	stateMemRemovePicker        // current external-memory list picker for /mem off
	stateCopyPicker             // fuzzy search over messages for /copy
)

// ── tea.Msg types ─────────────────────────────────────────────────────────────

type tokenMsg struct {
	gen  int
	text string
}
type streamEndMsg struct {
	gen     int
	content string
}
type streamErrMsg struct {
	gen int
	err string
}
type titleUpdatedMsg struct {
	convID int64
	title  string
}
type editorDoneMsg string
type streamTickMsg struct{}
type modelsLoadedMsg []llm.ModelInfo
type modelsErrorMsg string
type memoryUpdatedMsg struct {
	content string // new memory file (blank = NO_CHANGES / no update)
	note    string // display-only confirmation; blank for silent auto-curation
	convID  int64  // conversation the curation ran against (0 = legacy)
}
type memoryErrorMsg string
type memoryIdleTickMsg struct{ gen int } // fires when the user pauses; trips a curation if memPending non-empty
type compactionDoneMsg struct {
	gen     int
	convID  int64
	summary string
}

// pasteRef is a collapsed paste: the input shows a short placeholder, the
// full text is substituted back in when the message is sent.
type pasteRef struct {
	placeholder string
	text        string
}

// expandPastes substitutes collapsed paste placeholders with their full text.
func expandPastes(input string, pastes []pasteRef) string {
	for _, p := range pastes {
		input = strings.Replace(input, p.placeholder, p.text, 1)
	}
	return input
}

// ── search result ─────────────────────────────────────────────────────────────

type searchResult struct {
	conv *store.Conversation
	msg  *store.Message
}

// ── available /commands ───────────────────────────────────────────────────────

var commands = []string{
	"/clear", "/connect doc", "/copy", "/copy response ", "/copy prompt ", "/copy all ", "/debug", "/debug expand", "/debug collapse",
	"/delete", "/disconnect doc", "/doc", "/editor", "/fork",
	"/edit", "/forget ", "/grep", "/help", "/img ",
	"/list", "/mem", "/mem list", "/mem off ", "/memory", "/model ", "/models", "/new",
	"/paste", "/quit", "/r", "/remember ",
	"/rename ", "/retry", "/sel", "/stop", "/tokens", "/undo",
	"/vocab", "/vocab add ", "/vocab remove ",
	"/web", "/wipe",
}

// ── Model ─────────────────────────────────────────────────────────────────────

type Model struct {
	// core
	cfg      *config.Config
	store    *store.Store
	conv     *store.Conversation
	messages []*store.Message
	provider llm.Provider
	model    string

	// layout
	width  int
	height int
	ready  bool

	// chat view
	viewport      viewport.Model
	input         textarea.Model
	pendingImages []string // image data URLs / remote URLs for the next message
	pendingFiles  []string // pdf data URLs for the next message
	atBottom      bool
	autoTitled    bool   // prevent re-triggering auto-title
	errMsg        string // shown on separator row, cleared on next input

	// streaming
	streaming    bool
	streamGen    int // increments on every start/stop: stale async msgs are dropped
	streamCh     <-chan string
	cancelStream context.CancelFunc
	streamBuf    *strings.Builder

	// picker state
	state        appState
	pickerConvs  []*store.Conversation
	pickerFilter string
	pickerCursor int

	// search state
	searchInput  string
	searchAll    []searchResult
	searchCursor int

	// model picker state
	modelList    []llm.ModelInfo
	modelFilter  string
	modelCursor  int
	modelLoading bool

	// doc chat state (docs render in the user's editor, not in cx)
	docs        []*attachedDoc
	docReview   bool
	docEdits    []docEdit
	docEditIdx  int
	extGroups   []editGroup   // per-file edit batches queued for in-editor review
	extNotes    []string      // per-file review summaries accumulated across groups
	extRetry    bool          // a rejection carried a note: auto-retry when done
	lastApplied []docEdit     // applied edits of the last finished review, for /undo
	pendingSel  *docSelection // editor highlight riding along the next message
	curating    bool          // a memory-curation request is in flight
	memPending  []int64       // IDs of assistant turns awaiting memory curation
	lastCurated int64         // newest assistant msg ID the memory model has seen (per conv)
	memIdleGen  int           // streamGen of the armed idle tick (0 = none armed)
	pastes      []pasteRef    // collapsed multi-line pastes awaiting send

	// doc picker state (/doc with no path)
	docFiles       []string
	docFilter      string
	docCursor      int
	docPickerQuits bool // launched via `cx doc`: esc quits instead of returning to chat

	// connected-docs list picker state (/disconnect doc / /doc edit)
	docListMode   string // "open" or "disconnect"
	docListItems  []string
	docListFilter string
	docListCursor int

	// fork picker state (/fork)
	forkItems  []*store.Message // user prompts, newest first
	forkFilter string
	forkCursor int

	// external memory picker state (/mem [off])
	memFiles  []string // add: cwd candidates; remove: currently attached paths
	memFilter string
	memCursor int
	memMode   string // "add" or "remove"

	// copy picker state (/copy with no args opens fuzzy search)
	copyItems   []searchResult // messages from current conv (or all if toggled)
	copyFilter  string
	copyCursor  int
	copyAllConvs bool // if true, search across all convs

	// system
	systemPrompt string
	verbose      bool // /debug expand: show full notes, memory + context events
	webSearch    bool // web tools offered to the model (default on; /web toggles)

	// voice dictation state (ctrl+r)
	dictating      bool
	dictationBusy  bool // between stop and text-in-box: transcription/cleanup in flight
	dictationFrame int  // monotonic tick counter for the banner animation

	// incognito mode (cx incognito): no memory injection, no external
	// memory, no memory curation, no /remember. Conversation is deleted
	// on quit (main.go). Set once at construction via MarkIncognito.
	incognito bool

	// contextTokens cache. Recomputing on every viewport render was the
	// dominant cost during streaming. Refreshed via primeContextTokens
	// from mutation sites (streamEnd, handleInput, switchConversation).
	tokenCacheSig string
	tokenCacheVal int

	// markdown rendering (cached — glamour is expensive)
	mdRenderer *glamour.TermRenderer
	mdWidth    int
	mdCache    map[*store.Message]string
}

// ── constructor ───────────────────────────────────────────────────────────────

func New(cfg *config.Config, st *store.Store, conv *store.Conversation, msgs []*store.Message, prov llm.Provider, modelName, sysPrompt string) Model {
	ta := textarea.New()
	ta.Prompt = ""
	ta.Placeholder = "message..."
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = 0 // no line cap: the widget TRUNCATES input beyond MaxHeight
	ta.SetHeight(1)
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.BlurredStyle.Base = lipgloss.NewStyle()
	// Enter = submit (we handle it), Alt+Enter / Shift+Enter = newline
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "shift+enter"))
	ta.Focus()

	m := Model{
		cfg:          cfg,
		store:        st,
		conv:         conv,
		messages:     msgs,
		provider:     prov,
		model:        modelName,
		input:        ta,
		systemPrompt: sysPrompt,
		atBottom:     true,
		state:        stateChat,
		streamBuf:    &strings.Builder{},
		mdCache:      make(map[*store.Message]string),
		webSearch:    true,
	}
	// Restore connected docs from a previous session
	m.loadConvDocs()
	// Restore the memory-curation cursor so we don't re-curate the whole
	// chat on startup, and rebuild the pending-turn list from the store.
	m.loadMemoryCursor()
	return m
}

func (m Model) Init() tea.Cmd {
	return textarea.Blink
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if !m.ready {
			m.viewport = viewport.New(msg.Width, m.viewportHeight())
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = m.viewportHeight()
		}
		m.input.SetWidth(msg.Width - 6)
		m.refreshContent() // re-wrap on every resize
		if m.atBottom {
			m.viewport.GotoBottom()
		}
		return m, nil

	case docReloadMsg:
		m.reloadDocs()
		return m, nil

	case dictationStartedMsg:
		if msg.err != nil {
			playSound(soundError)
			m.errMsg = "dictation: " + msg.err.Error()
			m.dictating = false
			m.syncInputHeight()
			return m, nil
		}
		m.dictating = true
		m.dictationFrame = 0
		m.errMsg = ""
		m.syncInputHeight() // banner rows come out of the viewport
		return m, dictationTick()

	case dictationTickMsg:
		if !m.dictating && !m.dictationBusy {
			return m, nil
		}
		m.dictationFrame++
		if m.dictating {
			// Read whatever ffmpeg has written since last tick and turn it
			// into one waveform bucket. Silent ticks decay the previous
			// level so the bar visibly drops instead of freezing.
			sampleWaveform(currentDictation)
		}
		// Chain the tick as long as either state is active — animation
		// keeps running through the transcribing phase too.
		return m, dictationTick()

	case dictationDoneMsg:
		m.dictationBusy = false
		if msg.err != nil {
			playSound(soundError)
			m.errMsg = "dictation: " + msg.err.Error()
			m.syncInputHeight() // banner gone, viewport reclaims rows
			return m, nil
		}
		playSound(soundTextReady)
		m.syncInputHeight()
		// Splice the transcribed text at the cursor position in the input
		// box, adding a leading space when the caret is already after text.
		cur := m.input.Value()
		insert := msg.text
		if cur != "" && !strings.HasSuffix(cur, " ") && !strings.HasSuffix(cur, "\n") {
			insert = " " + insert
		}
		m.input.SetValue(cur + insert)
		m.input.CursorEnd()
		m.syncInputHeight()
		return m, nil

	case extReviewTickMsg:
		if consumeReviewDiscard() {
			abortExternalReview() // nvim may still hold the review; wipe it
			m.extGroups = nil
			m.extNotes = nil
			m.extRetry = false
			m.lastApplied = nil
			m.injectSystemLine("discarded pending edits (via cx)")
			return m, nil
		}
		if consumeReviewUndo() {
			// Same logic as /undo — revert the last-applied review.
			if len(m.lastApplied) == 0 {
				m.injectSystemLine("nothing to undo")
			} else {
				m.reloadDocs()
				reverted, failed := 0, 0
				for i := len(m.lastApplied) - 1; i >= 0; i-- {
					e := m.lastApplied[i]
					doc := m.findDoc(e.file)
					if doc == nil {
						failed++
						continue
					}
					newContent, ok := applyEditTo(doc.content, e.replace, e.search)
					if !ok {
						failed++
						continue
					}
					if err := os.WriteFile(doc.path, []byte(newContent), 0o644); err != nil {
						failed++
						continue
					}
					doc.content = newContent
					reverted++
				}
				m.lastApplied = nil
				note := fmt.Sprintf("reverted %d applied edit(s) (via model tool)", reverted)
				if failed > 0 {
					note += fmt.Sprintf("; %d couldn't be reverted (text changed)", failed)
				}
				m.injectSystemLine(note)
				pokeChecktime() // tell nvim to reload the file from disk
			}
			return m, nil
		}
		if len(m.extGroups) == 0 {
			return m, nil
		}
		// N fired mid-review: send the revision request right away, while the
		// user keeps reviewing the remaining hunks.
		if !m.streaming && m.provider != nil {
			raw := consumeRejectNow()
			var events []rejectEvent
			for _, ev := range raw {
				if isKnownEditSearch(m.extGroups, ev.Search) {
					events = append(events, ev)
				}
			}
			if len(events) > 0 {
				var sb strings.Builder
				for _, ev := range events {
					fmt.Fprintf(&sb, "I rejected the proposed edit changing %q to %q. Reason: %q. ",
						clip(ev.Search, 300), clip(ev.Replace, 300), ev.Reason)
					if ev.SelectionText != "" {
						fmt.Fprintf(&sb, "I was highlighting lines %d-%d in %s: %q. ",
							ev.SelectionFrom, ev.SelectionTo,
							filepath.Base(ev.SelectionFile),
							clip(ev.SelectionText, 800))
					}
				}
				sb.WriteString("Revise it now and propose an updated <edit> block. The other proposed edits are still under review; do not resend them.")
				note := sb.String()
				if saved, err := m.store.AddMessage(m.conv.ID, "note", note); err == nil {
					m.messages = append(m.messages, saved)
				} else {
					m.messages = append(m.messages, &store.Message{Role: "note", Content: note})
				}
				m.refreshContent()
				if m.atBottom {
					m.viewport.GotoBottom()
				}
				m2, cmd := m.startStream()
				return m2, tea.Batch(cmd, extReviewTick(msg.n+1))
			}
		}
		results, done := readExternalReviewResults()
		if !done {
			if msg.n > 3600 { // ~30 min: assume the review was abandoned
				m.extGroups = nil
				m.extNotes = nil
				m.extRetry = false
				m.lastApplied = nil
				abortExternalReview()
				return m, nil
			}
			return m, extReviewTick(msg.n + 1)
		}
		m.reloadDocs() // neovim wrote the file

		// Retry once all groups are reviewed when a rejection carried a note
		// (unless it already fired via reject-now) or an edit failed to
		// locate, which means the model anchored on stale text.
		for _, r := range results {
			if !r.Applied && !r.Reported && r.Reason != "" {
				m.extRetry = true
			}
		}
		g := m.extGroups[0]
		m.extGroups = m.extGroups[1:]
		for i, r := range results {
			if r.Applied && i < len(g.edits) {
				m.lastApplied = append(m.lastApplied, g.edits[i])
			}
		}
		m.extNotes = append(m.extNotes, summarizeReview(results, filepath.Base(g.file)))

		// More files to review: hand the next group to neovim
		if len(m.extGroups) > 0 {
			if startExternalReview(m.extGroups[0].file, m.extGroups[0].edits) {
				return m, extReviewTick(0)
			}
			m.extNotes = append(m.extNotes, fmt.Sprintf("review of %s could not start", filepath.Base(m.extGroups[0].file)))
			m.extGroups = nil
		}

		// Sweep any reject events that raced the final results
		for _, ev := range consumeRejectNow() {
			if !isKnownEditSearch(m.extGroups, ev.Search) {
				continue // stale event from a previous session
			}
			m.extNotes = append(m.extNotes, fmt.Sprintf("edit rejected: %q", ev.Reason))
			m.extRetry = true
		}
		note := strings.Join(m.extNotes, "; ")
		retry := m.extRetry
		m.extNotes = nil
		m.extRetry = false
		if retry {
			note += ". Revise the rejected or unlocated edits: re-read the current document content above, anchor on text that exists NOW, and propose updated <edit> blocks."
		}
		if saved, err := m.store.AddMessage(m.conv.ID, "note", note); err == nil {
			m.messages = append(m.messages, saved)
		} else {
			m.messages = append(m.messages, &store.Message{Role: "note", Content: note})
		}
		m.refreshContent()
		if m.atBottom {
			m.viewport.GotoBottom()
		}
		if retry && !m.streaming && m.provider != nil {
			m2, cmd := m.startStream()
			// The retry stream will queue any new edits into extGroups when
			// it ends — keep the tick chain alive so those get picked up
			// even if the queue branch's own tick races us
			return m2, tea.Batch(cmd, extReviewTick(0))
		}
		return m, nil

	case streamTickMsg:
		if m.streaming {
			m.refreshContent()
			if m.atBottom {
				m.viewport.GotoBottom()
			}
			return m, streamTick()
		}
		return m, nil

	case tokenMsg:
		if msg.gen != m.streamGen || !m.streaming {
			return m, nil // token from a cancelled/superseded stream
		}
		// Just accumulate — the 150ms streamTick refreshes the view.
		m.streamBuf.WriteString(msg.text)
		return m, listenToken(m.streamCh, msg.gen)

	case streamErrMsg:
		if msg.gen != m.streamGen {
			return m, nil // error from a cancelled/superseded stream
		}
		m.stopStream()
		m.streamBuf.Reset()
		m.errMsg = msg.err
		return m, nil

	case streamEndMsg:
		// Cancelled or superseded streams (ctrl+c, /stop, conversation switch,
		// a newer stream) must not save their content.
		if msg.gen != m.streamGen || !m.streaming {
			return m, nil
		}
		content := msg.content
		m.streaming = false
		m.cancelStream = nil
		m.streamCh = nil

		if content != "" {
			if saved, err := m.store.AddMessage(m.conv.ID, "assistant", content); err == nil {
				m.messages = append(m.messages, saved)
			} else {
				m.messages = append(m.messages, &store.Message{Role: "assistant", Content: content})
			}
		}
		m.streamBuf.Reset()
		m.refreshContent()
		m.viewport.GotoBottom()
		m.atBottom = true
		m.primeContextTokens() // status bar reflects the fresh total

		// Drain any undo request queued by the undo_last_review tool during
		// this stream. The extReviewTick chain only runs while a review is
		// active; if the review finished before the model's response, the
		// tick is dead and the signal would never be consumed.
		if consumeReviewUndo() {
			m.reloadDocs()
			reverted, failed := 0, 0
			for i := len(m.lastApplied) - 1; i >= 0; i-- {
				e := m.lastApplied[i]
				doc := m.findDoc(e.file)
				if doc == nil {
					failed++
					continue
				}
				newContent, ok := applyEditTo(doc.content, e.replace, e.search)
				if !ok {
					failed++
					continue
				}
				if err := os.WriteFile(doc.path, []byte(newContent), 0o644); err != nil {
					failed++
					continue
				}
				doc.content = newContent
				reverted++
			}
			m.lastApplied = nil
			note := fmt.Sprintf("reverted %d applied edit(s)", reverted)
			if failed > 0 {
				note += fmt.Sprintf(" (%d couldn't — text changed)", failed)
			}
			m.injectSystemLine(note)
			pokeChecktime()
		}

		// Doc mode: hand proposed edits to neovim for review (per file, in
		// sequence), or fall back to the in-cx y/n flow without the bridge.
		var reviewCmd tea.Cmd
		if len(m.docs) > 0 && content != "" {
			syncDocs(m.docPaths())
			m.reloadDocs() // match edits against the latest buffer content
			edits := parseDocEdits(content)
			edits = resolveEditFiles(edits, m.docs)
			edits = deChainEdits(edits, m.docs)
			edits = normalizeEdits(edits, m.docs)
			var noops int
			edits, noops = explodeEdits(edits, m.docs)
			// Loud diagnostic when the response looked like it contained an
			// edit block but nothing parsed / resolved. Common cause: model
			// emitted an unclosed <edit> tag, malformed SEARCH/REPLACE, or
			// an edit that couldn't be attached to any connected doc. Silent
			// dropping used to look like "the edit never happened" and the
			// user had to re-ask.
			if len(edits) == 0 && noops == 0 && strings.Contains(content, "<edit") {
				m.injectSystemLine("model proposed an edit but I couldn't parse it. ask again, or /debug to see the raw response")
			}
			if len(edits) == 0 && noops > 0 {
				m.injectSystemLine(fmt.Sprintf("%d proposed edit(s) matched the document already, nothing to apply", noops))
			}
			if len(edits) > 0 && consumeReviewDiscard() {
				abortExternalReview()
				m.extGroups = nil
				m.extNotes = nil
				m.extRetry = false
				m.lastApplied = nil
				m.injectSystemLine("discarded pending edits (via cx)")
				edits = nil // don't start a review below
			}
			if len(edits) > 0 && len(m.extGroups) > 0 {
				// A review is already on screen: INJECT the new edits into
				// it as additional pending hunks. No aborting the current
				// review, no queueing behind unfinished decisions, no user
				// waiting to finish before the fix appears.
				groups := groupEditsByFile(edits)
				injected := 0
				for _, g := range groups {
					if appendExternalReview(g.file, g.edits) {
						m.extGroups[len(m.extGroups)-1].edits = append(m.extGroups[len(m.extGroups)-1].edits, g.edits...)
						injected += len(g.edits)
					}
				}
				if injected == 0 {
					// Append failed (bridge down mid-session): fall back to
					// the old queue path so the retry isn't lost entirely.
					m.extGroups = append(m.extGroups, groups...)
				}
				reviewCmd = extReviewTick(0)
			} else if len(edits) > 0 {
				groups := groupEditsByFile(edits)
				if len(groups) > 0 && startExternalReview(groups[0].file, groups[0].edits) {
					m.extGroups = groups
					m.extNotes = nil
					m.extRetry = false
					// lastApplied is NOT reset here: retry cascades within a
					// single user prompt (N-with-note → revised proposal →
					// review again) accumulate into one /undo scope. Reset
					// happens on the next user prompt (see handleInput).
					word := "edits"
					if len(edits) == 1 {
						word = "edit"
					}
					line := fmt.Sprintf("proposed %d %s in neovim", len(edits), word)
					if noops > 0 {
						line += fmt.Sprintf(" (%d proposed no change, dropped)", noops)
					}
					m.injectSystemLine(line)
					reviewCmd = extReviewTick(0)
				} else {
					m.injectSystemLine("neovim bridge unavailable, review here")
					m.docEdits = edits
					m.docEditIdx = 0
					m.docReview = true
					m.showDocEdit()
				}
			}
		}

		var cmds []tea.Cmd
		if reviewCmd != nil {
			cmds = append(cmds, reviewCmd)
		}
		if !m.autoTitled && !m.incognito && m.conv.Title == "Untitled" && len(m.messages) >= 2 {
			m.autoTitled = true
			cmds = append(cmds, m.autoTitleCmd())
		}
		// Memory curation is batched + hybrid-triggered: we accumulate finished
		// assistant turns in memPending and fire only when N have piled up OR
		// the user goes idle for a while, whichever comes first. The armed
		// idle tick carries the current streamGen so a tick from a previous
		// conversation is ignored after a switch.
		if last := lastAssistantID(m.messages); last != 0 && (len(m.memPending) == 0 || m.memPending[len(m.memPending)-1] != last) {
			m.memPending = append(m.memPending, last)
		}
		interval := m.cfg.MemoryInterval
		if interval <= 0 {
			interval = 6
		}
		if !m.curating && len(m.memPending) >= interval {
			if cmd := m.curateMemoryCmd(); cmd != nil {
				m.curating = true
				m.memPending = nil
				m.memIdleGen = 0
				cmds = append(cmds, cmd)
			}
		} else if !m.curating && len(m.memPending) > 0 {
			idle := m.cfg.MemoryIdleSeconds
			if idle <= 0 {
				idle = 120
			}
			gen := m.streamGen + 1 // a tick landing after a streamGen bump is stale
			m.memIdleGen = gen
			cmds = append(cmds, tea.Tick(time.Duration(idle)*time.Second, func(time.Time) tea.Msg {
				return memoryIdleTickMsg{gen: gen}
			}))
		}
		if len(cmds) > 0 {
			return m, tea.Batch(cmds...)
		}
		return m, nil

	case titleUpdatedMsg:
		// Only apply if we're still on the conversation that was titled
		if msg.convID == m.conv.ID {
			m.conv.Title = msg.title
		}
		return m, nil

	case editorDoneMsg:
		// Mirror the editor exactly into the input — including cleared-to-blank.
		// Never sends; the user presses Enter.
		content := strings.TrimSpace(string(msg))
		m.input.SetValue(content)
		if m.input.Value() != content {
			// the widget dropped part of the text (it truncates in several
			// edge cases): NEVER lose a draft, collapse it like a paste
			lines := strings.Count(content, "\n") + 1
			ph := fmt.Sprintf("[editor draft #%d, %d lines]", len(m.pastes)+1, lines)
			m.pastes = append(m.pastes, pasteRef{placeholder: ph, text: content})
			m.input.SetValue(ph)
		}
		m.input.CursorEnd()
		m.syncInputHeight()
		return m, nil

	case modelsLoadedMsg:
		m.modelList = []llm.ModelInfo(msg)
		m.modelLoading = false
		return m, nil

	case modelsErrorMsg:
		m.errMsg = string(msg)
		m.state = stateChat
		m.modelLoading = false
		return m, nil

	case memoryIdleTickMsg:
		// The user paused long enough to consider the conversation "at rest".
		// Fire a curation now if there are pending turns and no curation is
		// already in flight; ignore ticks armed for a prior streamGen (the
		// conversation has since moved on).
		if msg.gen != m.memIdleGen || m.curating || len(m.memPending) == 0 {
			return m, nil
		}
		if cmd := m.curateMemoryCmd(); cmd != nil {
			m.curating = true
			m.memPending = nil
			m.memIdleGen = 0
			return m, cmd
		}
		return m, nil

	case memoryUpdatedMsg:
		m.curating = false
		if msg.content != "" {
			existing, _ := memory.Raw(config.MemoryPath())
			oldLines := strings.Count(existing, "\n") + 1
			newLines := strings.Count(msg.content, "\n") + 1
			// Auto-curation returning a drastically smaller file is almost
			// always a truncated or garbage reply, not real pruning: one bad
			// response must not wipe the memory. Explicit /remember//forget
			// (note set) may shrink freely.
			if msg.note == "" && oldLines >= 20 && newLines*3 < oldLines {
				if m.verbose {
					m.injectSystemLine(fmt.Sprintf("memory update rejected: suspicious shrink (%d lines -> %d)", oldLines, newLines))
				}
			} else {
				// Cross-process advisory lock so two cx instances rewriting
				// memory.md concurrently can't clobber each other. Lock
				// contention just defers this write — the next threshold
				// will retry, and the other process's turns are already
				// covered by the write we're waiting on.
				lk, _ := memory.TryLockCuration()
				if lk == nil && !m.verbose {
					// Someone else is curating right now; skip silently.
					// verbose users still see the "no changes" note below.
				} else if err := memory.SaveRaw(config.MemoryPath(), msg.content); err != nil {
					memory.UnlockCuration(lk)
					m.injectSystemLine("memory write failed: " + err.Error())
				} else {
					memory.UnlockCuration(lk)
					m.reloadSystemPrompt()
				}
			}
		}
		if msg.note != "" {
			m.injectSystemLine(msg.note)
		} else if m.verbose {
			if msg.content != "" {
				m.injectSystemLine(fmt.Sprintf("memory updated (%d lines)", strings.Count(msg.content, "\n")+1))
			} else {
				m.injectSystemLine("memory: no changes")
			}
		}
		return m, nil

	case memoryErrorMsg:
		m.curating = false
		if string(msg) != "" {
			m.injectSystemLine("memory error: " + string(msg))
		}
		return m, nil

	case compactionDoneMsg:
		// Cancelled, superseded, or for a conversation we've left: drop it.
		if msg.gen != m.streamGen || msg.convID != m.conv.ID {
			return m, nil
		}
		// Drop transient display-only notes (system lines, diff previews)
		var persisted []*store.Message
		for _, pm := range m.messages {
			if pm.ID != 0 {
				persisted = append(persisted, pm)
			}
		}
		recentCount := min(6, len(persisted))
		recent := persisted[len(persisted)-recentCount:]

		// Persist the summary just before the recent messages so it survives
		// restarts and lands in the right chronological position.
		createdAt := time.Now().Unix()
		if len(recent) > 0 {
			createdAt = recent[0].CreatedAt - 1
		}
		summaryMsg, err := m.store.AddMessageAt(m.conv.ID, "summary", msg.summary, createdAt)
		if err != nil {
			summaryMsg = &store.Message{Role: "summary", Content: msg.summary}
		}
		m.messages = append([]*store.Message{summaryMsg}, recent...)
		m.refreshContent()
		m.viewport.GotoBottom()
		m.atBottom = true
		return m.startStream()

	case tea.KeyMsg:
		switch m.state {
		case statePicker:
			return m.updatePicker(msg)
		case stateSearch:
			return m.updateSearch(msg)
		case stateModelPicker:
			return m.updateModelPicker(msg)
		case stateDocPicker:
			return m.updateDocPicker(msg)
		case stateDocListPicker:
			return m.updateDocListPicker(msg)
		case stateForkPicker:
			return m.updateForkPicker(msg)
		case stateMemAddPicker, stateMemRemovePicker:
			return m.updateMemPicker(msg)
		case stateCopyPicker:
			return m.updateCopyPicker(msg)
		default:
			return m.updateChat(msg)
		}
	}

	return m, nil
}

// ── Chat key handling ─────────────────────────────────────────────────────────

func (m Model) updateChat(msg tea.KeyMsg) (Model, tea.Cmd) {
	// Pending doc edits: y/n/a/esc review takes over the keyboard
	if m.docReview {
		return m.updateDocReview(msg)
	}

	// Clear error on any meaningful key
	if msg.Type == tea.KeyRunes || msg.Type == tea.KeyBackspace || msg.Type == tea.KeyDelete {
		m.errMsg = ""
	}

	// Large pastes collapse to a placeholder (expanded again on send) so the
	// input stays readable instead of flooding with pasted text
	if msg.Type == tea.KeyRunes && msg.Paste {
		text := string(msg.Runes)
		lines := strings.Count(text, "\n") + 1
		if lines >= 3 || len(text) > 200 {
			ph := fmt.Sprintf("[paste #%d, %d lines]", len(m.pastes)+1, lines)
			m.pastes = append(m.pastes, pasteRef{placeholder: ph, text: text})
			m.input.InsertString(ph)
			m.syncInputHeight()
			return m, nil
		}
	}

	switch msg.Type {

	case tea.KeyCtrlC:
		if m.streaming {
			// Keep whatever was streamed so far
			partial := m.streamBuf.String()
			m.stopStream()
			m.streamBuf.Reset()
			if partial != "" {
				if saved, err := m.store.AddMessage(m.conv.ID, "assistant", partial+" [cancelled]"); err == nil {
					m.messages = append(m.messages, saved)
				} else {
					m.messages = append(m.messages, &store.Message{Role: "assistant", Content: partial + " [cancelled]"})
				}
			}
			m.refreshContent()
			m.viewport.GotoBottom()
			return m, nil
		}
		return m, tea.Quit

	case tea.KeyEnter:
		if msg.Alt {
			// alt+enter inserts a newline: the textarea's own binding handles
			// it, but only if this case doesn't swallow the key first
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			m.syncInputHeight()
			return m, cmd
		}
		input := strings.TrimSpace(m.input.Value())
		if input == "" {
			return m, nil
		}
		if m.streaming && !strings.HasPrefix(input, "/") {
			return m, nil
		}
		input = strings.TrimSpace(expandPastes(input, m.pastes))
		m.pastes = nil
		m.input.Reset()
		m.syncInputHeight()
		m.errMsg = ""
		return m.handleInput(input)

	case tea.KeyEsc:
		// Never wipe the prompt on esc: too easy to trigger by accident
		// and lose a long draft. Only dismiss a stuck error banner.
		m.errMsg = ""
		return m, nil

	case tea.KeyTab:
		val := m.input.Value()
		if head, tok, ok := lastMentionToken(val); ok {
			if cands := mentionCandidates(tok); len(cands) > 0 {
				m.input.SetValue(head + "@" + cands[0] + " ")
				m.input.CursorEnd()
			}
			return m, nil
		}
		matches := completionsFor(val)
		if len(matches) == 1 {
			m.input.SetValue(matches[0])
			m.input.CursorEnd()
		}
		return m, nil

	case tea.KeyCtrlL:
		return m.enterPicker()

	case tea.KeyCtrlN:
		return m.newConversation()

	case tea.KeyCtrlE:
		return m.openEditor()

	case tea.KeyCtrlX:
		// Expand the paste placeholder the cursor is sitting on (or the
		// nearest one in the input). Collapses it back on a second press.
		return m.togglePasteUnderCursor()

	case tea.KeyCtrlG:
		return m.enterSearch()

	case tea.KeyCtrlT:
		return m.enterModelPicker()

	case tea.KeyCtrlU:
		m.viewport.HalfViewUp()
		m.atBottom = m.viewport.AtBottom()
		return m, nil

	case tea.KeyCtrlD:
		m.viewport.HalfViewDown()
		m.atBottom = m.viewport.AtBottom()
		return m, nil

	case tea.KeyCtrlR:
		// Toggle voice dictation: press to start, press again to stop.
		// If a transcription is already in flight, ignore.
		if m.dictationBusy {
			return m, nil
		}
		if m.dictating {
			m.dictating = false
			m.dictationBusy = true
			// Tick keeps chaining because dictationBusy is true — the
			// spinner animation stays alive through transcription.
			return m, tea.Batch(stopDictationCmd(m.cfg), dictationTick())
		}
		return m, startDictationCmd(m.cfg)

	case tea.KeyUp, tea.KeyDown:
		// With text in the prompt, arrows move the cursor through it (like
		// any editor); with an empty prompt they scroll the conversation
		if m.input.Value() != "" {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			m.syncInputHeight()
			return m, cmd
		}
		if msg.Type == tea.KeyUp {
			m.viewport.LineUp(1)
		} else {
			m.viewport.LineDown(1)
		}
		m.atBottom = m.viewport.AtBottom()
		return m, nil

	case tea.KeyPgUp:
		m.viewport.HalfViewUp()
		m.atBottom = m.viewport.AtBottom()
		return m, nil

	case tea.KeyPgDown:
		m.viewport.HalfViewDown()
		m.atBottom = m.viewport.AtBottom()
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	// Dynamically resize textarea + viewport based on line count
	m.syncInputHeight()
	return m, cmd
}

// ── Input handling ────────────────────────────────────────────────────────────

func (m Model) handleInput(input string) (Model, tea.Cmd) {
	// vim muscle memory
	if input == ":q" {
		m.stopStream()
		// Flush pending memory curation in the background so quit feels
		// instant while still writing the final memory update. main.go waits
		// briefly for this to settle before the process exits.
		if cmd := m.flushMemoryAndSave(); cmd != nil {
			return m, tea.Batch(tea.Quit, cmd)
		}
		return m, tea.Quit
	}

	if strings.HasPrefix(input, "/") {
		if isKnownCommand(input) {
			return m.handleCommand(input)
		}
		// Not a command: an absolute path (or /file) falls through as a
		// normal message; a lone /typo gets an error instead of being sent.
		verb := strings.SplitN(input, " ", 2)[0]
		if !strings.Contains(verb[1:], "/") {
			if _, err := os.Stat(verb); err != nil {
				m.errMsg = "unknown command: " + verb + " (tab to complete, /help for list)"
				return m, nil
			}
		}
	}

	// Auto-detect image paths from drag-drop. detectImagePathsInInput handles
	// the messy real-world cases: escape-space (`\ `), unescaped spaces mid-
	// filename, multi-line drops (one path per line), and multi-path
	// concatenation. Any resolvable image paths are peeled out; the leftover
	// text (if any) becomes the message body.
	if imgs, rest := m.detectImagePathsInInput(input); len(imgs) > 0 {
		primary := imgs[0]
		for _, extra := range imgs[1:] {
			if dataURL, err := encodeAttachment(extra); err == nil {
				m.pendingImages = append(m.pendingImages, dataURL)
				m.injectSystemLine("attached " + filepath.Base(extra))
			}
		}
		return m.handleCommand(`/img "` + primary + `" ` + rest)
	}

	if m.provider == nil {
		m.errMsg = "no provider (set GEMINI_API_KEY, OPENAI_API_KEY, or configure ollama)"
		return m, nil
	}

	// Pick up an editor highlight, if one was sent over (see README: neovim bridge).
	// Skip selections whose file no longer exists (stale from a crashed nvim
	// or a prior test run) so they don't hijack the message and auto-connect
	// a bogus doc.
	display := input
	if sel := readSelection(); sel != nil {
		if _, err := os.Stat(sel.file); err != nil {
			consumeSelection() // stale reference: throw it away silently
		} else {
			if len(m.docs) == 0 {
				m, _ = m.connectDoc(sel.file) // auto-connect the highlighted file
			}
			if m.findDoc(sel.file) != nil {
				m.pendingSel = sel
				consumeSelection()
				display += fmt.Sprintf("\n[highlighted L%d-%d in %s]", sel.start, sel.end, filepath.Base(sel.file))
			}
		}
	}
	// Resolve @file mentions: text files connect as docs, images and PDFs
	// attach (the first local one persists on the message via image_path;
	// extras and remote URLs ride along for this request only).
	mentionImage := ""
	for _, tok := range strings.Fields(input) {
		if !strings.HasPrefix(tok, "@") || len(tok) < 2 {
			continue
		}
		path := tok[1:]
		// Linked image: pass the URL straight through; providers fetch it
		if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
			if _, ok := imageExts[strings.ToLower(filepath.Ext(path))]; ok {
				m.pendingImages = append(m.pendingImages, path)
				m.injectSystemLine("attached " + path)
			}
			continue
		}
		if strings.HasPrefix(path, "~") {
			if home, err := os.UserHomeDir(); err == nil {
				path = home + path[1:]
			}
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			continue
		}
		if fi, err := os.Stat(abs); err != nil || fi.IsDir() {
			continue // not a file: leave the token as plain text
		}
		ext := strings.ToLower(filepath.Ext(abs))
		_, isImage := imageExts[ext]
		if isImage || ext == ".pdf" {
			if _, err := encodeAttachment(abs); err != nil {
				m.errMsg = "attach: " + err.Error()
				continue
			}
			if mentionImage == "" {
				mentionImage = abs // persisted on the message
			} else if dataURL, err := encodeAttachment(abs); err == nil {
				if isImage {
					m.pendingImages = append(m.pendingImages, dataURL)
				} else {
					m.pendingFiles = append(m.pendingFiles, dataURL)
				}
			}
			m.injectSystemLine("attached " + filepath.Base(abs))
			continue
		}
		if m.findDoc(abs) == nil {
			m, _ = m.connectDoc(abs)
		}
	}

	// Pull unsaved buffer changes to disk, then re-read: the editor buffer
	// is the source of truth for what the model sees
	syncDocs(m.docPaths())
	m.reloadDocs()

	// Refresh the system prompt from memory.md on every message so a chat
	// left running in the background picks up memory edits made in another
	// cx session (or via /remember from a different chat) without a restart.
	m.reloadSystemPrompt()

	if saved, err := m.store.AddMessageWithImage(m.conv.ID, "user", display, mentionImage); err == nil {
		m.messages = append(m.messages, saved)
	} else {
		m.messages = append(m.messages, &store.Message{Role: "user", Content: display, ImagePath: mentionImage})
	}
	m.refreshContent()
	m.viewport.GotoBottom()
	m.atBottom = true

	// A new prompt starts a fresh /undo scope. Retry cascades within one
	// response (N-with-note → revised proposal → apply again) all fold into
	// the same scope, so /undo reverses the whole chain — not just the last
	// review round.
	m.lastApplied = nil

	// Refresh the cached token count for the status bar's next render, so
	// it can skip the O(n) walk during streaming.
	m.primeContextTokens()

	// Check if compaction needed before sending
	if m.contextTokens() > m.cfg.MaxContextTokens*3/4 {
		return m.startCompaction()
	}
	return m.startStream()
}

func (m Model) handleCommand(input string) (Model, tea.Cmd) {
	input = strings.TrimSpace(input)
	parts := strings.SplitN(input, " ", 2)
	verb := parts[0]

	switch input {
	case "/wipe confirm":
		if err := m.store.WipeAll(); err != nil {
			m.errMsg = "wipe failed: " + err.Error()
			return m, nil
		}
		conv, err := m.store.CreateConversation(m.model)
		if err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		m.stopStream()
		m.conv = conv
		m.messages = nil
		m.streamBuf.Reset()
		m.autoTitled = false
		m.state = stateChat
		m.errMsg = ""
		m.loadConvDocs()
		m.refreshContent()
		m.viewport.GotoBottom()
		m.atBottom = true
		return m, nil
	}

	switch verb {
	case "/stop":
		if !m.streaming {
			m.errMsg = "not currently streaming"
			return m, nil
		}
		partial := m.streamBuf.String()
		m.stopStream()
		m.streamBuf.Reset()
		if partial != "" {
			if saved, err := m.store.AddMessage(m.conv.ID, "assistant", partial+" [stopped]"); err == nil {
				m.messages = append(m.messages, saved)
			} else {
				m.messages = append(m.messages, &store.Message{Role: "assistant", Content: partial + " [stopped]"})
			}
		}
		m.refreshContent()
		m.viewport.GotoBottom()
		return m, nil

	case "/quit":
		m.stopStream()
		if cmd := m.flushMemoryAndSave(); cmd != nil {
			return m, tea.Batch(tea.Quit, cmd)
		}
		return m, tea.Quit

	case "/new":
		return m.newConversation()

	case "/list":
		return m.enterPicker()

	case "/grep":
		return m.enterSearch()

	case "/clear":
		// Remove display-only system annotations (ID=0); keep all persisted messages
		var kept []*store.Message
		for _, msg := range m.messages {
			if msg.ID != 0 {
				kept = append(kept, msg)
			}
		}
		m.messages = kept
		m.refreshContent()
		return m, nil

	case "/debug":
		if len(parts) > 1 {
			switch strings.TrimSpace(parts[1]) {
			case "expand":
				m.verbose = true
				m.mdCache = make(map[*store.Message]string)
				m.refreshContent()
				m.injectSystemLine("verbose mode on: full notes, memory and context events (/debug collapse to turn off)")
				return m, nil
			case "collapse":
				m.verbose = false
				m.mdCache = make(map[*store.Message]string)
				m.refreshContent()
				m.injectSystemLine("clean mode on")
				return m, nil
			}
		}
		msgs := m.buildLLMMessages()
		var sb strings.Builder
		fmt.Fprintf(&sb, "── debug: %s ──\n", m.model)
		fmt.Fprintf(&sb, "── %d messages in payload ──\n\n", len(msgs))
		for i, msg := range msgs {
			imgNote := ""
			if len(msg.Images) > 0 {
				imgNote = fmt.Sprintf("  [+%d image(s) attached]", len(msg.Images))
			}
			fmt.Fprintf(&sb, "[%d] %s:%s\n%s\n\n", i, msg.Role, imgNote, msg.Content)
		}
		m.injectSystemLine(sb.String())
		return m, nil

	case "/models":
		return m.enterModelPicker()

	case "/rename":
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			m.errMsg = "usage: /rename <title>"
			return m, nil
		}
		title := strings.TrimSpace(parts[1])
		m.store.UpdateTitle(m.conv.ID, title)
		m.conv.Title = title
		m.autoTitled = true // don't auto-overwrite a manual rename
		return m, nil

	case "/model":
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			m.injectSystemLine("current model: " + m.model)
			return m, nil
		}
		newModel := strings.TrimSpace(parts[1])
		prov, err := llm.ForModel(newModel, m.cfg)
		if err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		m.model = newModel
		m.provider = prov
		m.store.UpdateModel(m.conv.ID, newModel)
		config.SaveLastModel(newModel)
		m.injectSystemLine("switched to " + newModel)
		return m, nil

	case "/web":
		if !strings.Contains(m.model, "/") {
			m.errMsg = "web search needs an OpenRouter model (e.g. anthropic/claude-sonnet-4-5)"
			return m, nil
		}
		arg := ""
		if len(parts) > 1 {
			arg = strings.TrimSpace(parts[1])
		}
		switch arg {
		case "on":
			m.webSearch = true
		case "off":
			m.webSearch = false
		default:
			m.webSearch = !m.webSearch
		}
		if m.webSearch {
			m.injectSystemLine("web tools on: the model can search and read pages when it needs to")
		} else {
			m.injectSystemLine("web tools off")
		}
		return m, nil

	case "/undo":
		if len(m.lastApplied) == 0 {
			m.errMsg = "no applied review edits to undo"
			return m, nil
		}
		m.reloadDocs()
		reverted, failed := 0, 0
		for i := len(m.lastApplied) - 1; i >= 0; i-- {
			e := m.lastApplied[i]
			doc := m.findDoc(e.file)
			if doc == nil {
				failed++
				continue
			}
			newContent, ok := applyEditTo(doc.content, e.replace, e.search)
			if !ok {
				failed++
				continue
			}
			if err := os.WriteFile(doc.path, []byte(newContent), 0o644); err != nil {
				failed++
				continue
			}
			doc.content = newContent
			reverted++
		}
		pokeChecktime()
		m.lastApplied = nil
		note := fmt.Sprintf("I reverted %d previously applied edit(s).", reverted)
		if failed > 0 {
			note += fmt.Sprintf(" %d could not be reverted (the text changed since).", failed)
		}
		if saved, err := m.store.AddMessage(m.conv.ID, "note", note); err == nil {
			m.messages = append(m.messages, saved)
		} else {
			m.messages = append(m.messages, &store.Message{Role: "note", Content: note})
		}
		m.refreshContent()
		m.viewport.GotoBottom()
		m.atBottom = true
		return m, nil

	case "/fork":
		return m.enterForkPicker()

	case "/editor":
		return m.openEditor()

	case "/help":
		m.injectSystemLine(helpText)
		return m, nil

	case "/copy":
		// /copy with no args opens the fuzzy copy picker.
		// /copy [response|prompt|all] [n] keeps the old shortcut.
		if len(parts) == 1 {
			return m.enterCopyPicker()
		}
		what, n := "response", 1
		if len(parts) > 1 {
			args := strings.Fields(parts[1])
			if len(args) > 0 {
				switch args[0] {
				case "response", "prompt", "all":
					what = args[0]
				default:
					m.errMsg = "usage: /copy [response|prompt|all] [n]"
					return m, nil
				}
			}
			if len(args) > 1 {
				if v, err := strconv.Atoi(args[1]); err == nil && v > 0 {
					n = v
				} else {
					m.errMsg = "usage: /copy [response|prompt|all] [n]"
					return m, nil
				}
			}
		}
		text := m.collectCopy(what, n)
		if text == "" {
			m.errMsg = "nothing to copy"
			return m, nil
		}
		if err := copyToClipboard(text); err != nil {
			m.errMsg = "copy failed: " + err.Error()
			return m, nil
		}
		m.errMsg = ""
		m.injectSystemLine(fmt.Sprintf("copied %s (%d) to clipboard", what, n))
		return m, nil

	case "/delete":
		if err := m.store.DeleteConversation(m.conv.ID); err != nil {
			m.errMsg = "delete failed: " + err.Error()
			return m, nil
		}
		// Switch to most recent remaining, or create new
		convs, _ := m.store.ListConversations()
		if len(convs) == 0 {
			return m.newConversation()
		}
		return m.switchConversation(convs[0].ID)

	case "/edit":
		if m.streaming {
			return m, nil
		}
		// Find last user message
		var lastUserIdx int = -1
		for i := len(m.messages) - 1; i >= 0; i-- {
			if m.messages[i].Role == "user" {
				lastUserIdx = i
				break
			}
		}
		if lastUserIdx < 0 {
			m.errMsg = "no message to edit"
			return m, nil
		}
		lastMsg := m.messages[lastUserIdx]
		// Remove last user message + everything after from DB
		if lastMsg.ID > 0 {
			m.store.DeleteMessagesFrom(m.conv.ID, lastMsg.ID)
		}
		// Remove from in-memory list
		m.messages = m.messages[:lastUserIdx]
		// Put the old text into the input field for editing
		content := lastMsg.Content
		// Strip image / highlight placeholder lines
		var lines []string
		for line := range strings.SplitSeq(content, "\n") {
			if !strings.HasPrefix(line, "[image: ") && !strings.HasPrefix(line, "[highlighted ") {
				lines = append(lines, line)
			}
		}
		m.input.SetValue(strings.TrimSpace(strings.Join(lines, "\n")))
		m.input.CursorEnd()
		m.syncInputHeight()
		m.refreshContent()
		m.viewport.GotoBottom()
		m.atBottom = true
		return m, nil

	case "/retry", "/r":
		if m.streaming {
			return m, nil
		}
		// Find last user message
		var lastUserIdx int = -1
		for i := len(m.messages) - 1; i >= 0; i-- {
			if m.messages[i].Role == "user" {
				lastUserIdx = i
				break
			}
		}
		if lastUserIdx < 0 {
			m.errMsg = "no message to retry"
			return m, nil
		}
		// Remove the superseded reply (and anything after) from the DB too,
		// or it comes back on reload and doubles up in the payload
		for _, old := range m.messages[lastUserIdx+1:] {
			if old.ID > 0 {
				m.store.DeleteMessagesFrom(m.conv.ID, old.ID)
				break
			}
		}
		m.messages = m.messages[:lastUserIdx+1]
		m.refreshContent()
		m.viewport.GotoBottom()
		m.atBottom = true
		return m.startStream()

	case "/img":
		if m.streaming {
			m.errMsg = "wait for the response to finish (or /stop)"
			return m, nil
		}
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			m.errMsg = "usage: /img /path/to/image.png [optional message]"
			return m, nil
		}
		// Split into path and optional message (handles quoted / escaped spaces)
		imgPath, text := splitPathToken(parts[1])
		if text == "" {
			text = "What's in this image?"
		}
		// Resolve and validate path
		if strings.HasPrefix(imgPath, "~") {
			home, _ := os.UserHomeDir()
			imgPath = home + imgPath[1:]
		}
		absPath, err := filepath.Abs(imgPath)
		if err != nil {
			m.errMsg = "invalid path: " + err.Error()
			return m, nil
		}
		if _, err := encodeImageToDataURL(absPath); err != nil {
			m.errMsg = "image: " + err.Error()
			return m, nil
		}
		// Save with the image path; no placeholder line in the message
		// body. The image is delivered via ImagePath (see buildLLMMessages);
		// echoing the filename inside the transcript was noise the user
		// already knows because they just dragged the file.
		if saved, err := m.store.AddMessageWithImage(m.conv.ID, "user", text, absPath); err == nil {
			saved.ImagePath = absPath
			m.messages = append(m.messages, saved)
		} else {
			m.messages = append(m.messages, &store.Message{Role: "user", Content: text, ImagePath: absPath})
		}
		// The saved ImagePath is attached by buildLLMMessages; adding the
		// data URL to pendingImages too would send the image twice.
		m.refreshContent()
		m.viewport.GotoBottom()
		m.atBottom = true
		if m.contextTokens() > m.cfg.MaxContextTokens*3/4 {
			return m.startCompaction()
		}
		return m.startStream()

	case "/paste":
		if m.streaming {
			m.errMsg = "wait for the response to finish (or /stop)"
			return m, nil
		}
		path, err := pasteClipboardImage()
		if err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		text := ""
		if len(parts) > 1 {
			text = strings.TrimSpace(parts[1])
		}
		return m.handleCommand(`/img "` + path + `" ` + text)

	case "/doc":
		if m.streaming {
			m.errMsg = "wait for the response to finish (or /stop)"
			return m, nil
		}
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			return m.enterDocPicker()
		}
		arg := strings.TrimSpace(parts[1])
		switch arg {
		case "off", "close":
			return m.disconnectDocFlow()
		case "edit":
			switch len(m.docs) {
			case 0:
				m.errMsg = "no documents connected"
				return m, nil
			case 1:
				return m.openDocEditor(m.docs[0].path)
			default:
				return m.enterDocListPicker("open")
			}
		}
		path, _ := splitPathToken(arg)
		m2, cmd := m.connectDoc(path)
		if len(m2.docs) > 0 {
			m2.autoEditorSplit(m2.docs[len(m2.docs)-1].path)
		}
		return m2, cmd

	case "/connect":
		if m.streaming {
			m.errMsg = "wait for the response to finish (or /stop)"
			return m, nil
		}
		arg := ""
		if len(parts) > 1 {
			arg = strings.TrimSpace(parts[1])
		}
		if arg != "doc" && !strings.HasPrefix(arg, "doc ") {
			m.errMsg = "usage: /connect doc [path]"
			return m, nil
		}
		path := strings.TrimSpace(strings.TrimPrefix(arg, "doc"))
		if path == "" {
			path = lastDocPath()
			if path == "" {
				m.errMsg = "no remembered doc yet (connect one with a path first)"
				return m, nil
			}
		} else {
			path, _ = splitPathToken(path)
		}
		return m.connectDoc(path)

	case "/disconnect":
		if len(parts) > 1 && strings.TrimSpace(parts[1]) != "doc" {
			m.errMsg = "usage: /disconnect doc"
			return m, nil
		}
		return m.disconnectDocFlow()

	case "/sel":
		if len(parts) > 1 && strings.TrimSpace(parts[1]) == "clear" {
			consumeSelection()
			m.injectSystemLine("selection cleared")
			return m, nil
		}
		sel := readSelection()
		if sel == nil {
			m.injectSystemLine("no editor selection. highlight in neovim and press <leader>cs (see README)")
			return m, nil
		}
		preview := sel.text
		if len(preview) > 500 {
			preview = preview[:500] + "…"
		}
		note := "(attached to your next message · /sel clear to drop)"
		if len(m.docs) > 0 && m.findDoc(sel.file) == nil {
			note = "(from a file that isn't a connected doc, will NOT be sent)"
		}
		m.injectSystemLine(fmt.Sprintf("── selection: L%d-%d of %s ──\n%s\n%s",
			sel.start, sel.end, filepath.Base(sel.file), preview, note))
		return m, nil

	case "/memory":
		content, err := memory.Raw(config.MemoryPath())
		if err != nil {
			m.errMsg = "memory error: " + err.Error()
			return m, nil
		}
		if content == "" {
			m.injectSystemLine("memory is empty. chat or use /remember <fact>")
			return m, nil
		}
		m.injectSystemLine("── memory file ──\n" + content)
		return m, nil

	case "/mem":
		return m.handleMemCommand(parts)

	case "/vocab":
		return m.handleVocabCommand(parts)

	case "/tokens", "/stats":
		m.injectSystemLine(m.tokenStatsView())
		return m, nil

	case "/remember":
		if m.incognito {
			m.errMsg = "/remember is disabled in incognito mode"
			return m, nil
		}
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			m.errMsg = "usage: /remember <fact>"
			return m, nil
		}
		fact := strings.TrimSpace(parts[1])
		m.injectSystemLine("updating memory...")
		return m, m.editMemoryCmd("Remember this: "+fact, "memory updated, remembered: "+fact)

	case "/forget":
		if m.incognito {
			m.errMsg = "/forget is disabled in incognito mode"
			return m, nil
		}
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			m.errMsg = "usage: /forget <query>"
			return m, nil
		}
		query := strings.TrimSpace(parts[1])
		m.injectSystemLine("updating memory...")
		return m, m.editMemoryCmd("Forget/remove anything related to: "+query, "memory updated, forgot: "+query)

	case "/wipe":
		m.injectSystemLine("this will delete ALL conversations and messages.\ntype  /wipe confirm  to proceed, or anything else to cancel.")
		return m, nil

	default:
		m.errMsg = "unknown command: " + verb + "  (tab to complete, /help for list)"
		return m, nil
	}
}

// stripPlaceholders removes display-only [image:]/[highlighted] lines.
func stripPlaceholders(content string) string {
	var lines []string
	for line := range strings.SplitSeq(content, "\n") {
		if !strings.HasPrefix(line, "[image: ") && !strings.HasPrefix(line, "[highlighted ") {
			lines = append(lines, line)
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// collectCopy gathers the last n responses, prompts, or prompt/response
// pairs, oldest first. Returns fewer when the conversation is shorter.
func (m Model) collectCopy(what string, n int) string {
	var out []string
	take := func(role string, format func(string) string) {
		count := 0
		for i := len(m.messages) - 1; i >= 0 && count < n; i-- {
			if m.messages[i].Role != role {
				continue
			}
			text := m.messages[i].Content
			if role == "user" {
				text = stripPlaceholders(text)
			}
			if role == "assistant" {
				text = stripEditBlocks(text)
			}
			out = append([]string{format(text)}, out...)
			count++
		}
	}
	switch what {
	case "response":
		take("assistant", func(t string) string { return t })
	case "prompt":
		take("user", func(t string) string { return t })
	case "all":
		// walk backwards collecting assistant+user pairs
		pairs := 0
		var chunk []string
		for i := len(m.messages) - 1; i >= 0 && pairs < n; i-- {
			switch m.messages[i].Role {
			case "assistant":
				chunk = append([]string{"cx: " + stripEditBlocks(m.messages[i].Content)}, chunk...)
			case "user":
				chunk = append([]string{"you: " + stripPlaceholders(m.messages[i].Content)}, chunk...)
				out = append(chunk, out...)
				chunk = nil
				pairs++
			}
		}
		if len(chunk) > 0 && pairs < n {
			out = append(chunk, out...)
		}
	}
	return strings.Join(out, "\n\n")
}

// injectSystemLine adds a display-only note into the viewport (not persisted).
func (m *Model) injectSystemLine(text string) {
	// ID=0 marks display-only messages
	m.messages = append(m.messages, &store.Message{Role: "system", Content: text})
	m.refreshContent()
	m.viewport.GotoBottom()
	m.atBottom = true
}

const helpText = `KEYS
  enter                  send
  alt+enter              newline
  esc                    dismiss error banner
  ctrl+c                 cancel stream / quit
  ctrl+l                 conversation picker
  ctrl+n                 new conversation
  ctrl+g                 search all messages
  ctrl+t                 model picker
  ctrl+e                 open $EDITOR for long input (pastes auto-expanded)
  ctrl+x                 expand / collapse the paste placeholder under cursor
  ctrl+u / ctrl+d        scroll half page
  ctrl+r                 toggle voice dictation
  up / down              walk prompt (scroll chat when empty)
  tab                    autocomplete /command or @file

CHAT
  /new                   new conversation
  /list                  conversation picker
  /rename <title>        rename current conversation
  /delete                delete current conversation
  /wipe                  delete ALL conversations (asks confirm)
  /retry  (or /r)        re-send last message
  /edit                  edit your last message
  /stop                  stop current response
  /fork                  pick a past prompt, delete from it onwards, load into input
  /copy [n]              copy last n responses
  /copy prompt [n]       copy last n prompts
  /copy all [n]          copy last n prompt/response pairs
  /grep                  search messages
  /clear                 clear injected system notes

MODEL & WEB
  /model <name>          switch model
  /models                model picker (OpenRouter)
  /web [on|off]          toggle agentic web tools

DOCS
  /doc [path]            connect a doc + open in editor
  /doc edit              reopen a connected doc in editor
  /doc off               disconnect
  /connect doc [path]    connect a doc without opening the editor
  /disconnect doc        disconnect a doc
  /sel                   preview editor selection
  /sel clear             drop editor selection
  /undo                  revert edits from the last user prompt
  @file                  mention: .md/.txt connect, images/PDFs attach

IMAGES
  /img <path> [text]     send an image
  /paste [text]          send image from clipboard

MEMORY
  /memory                show memory.md
  /remember <fact>       add via memory model
  /forget <query>        drop via memory model
  /mem [path]            attach external memory file (read-only) — picker if no path
  /mem list              list attached
  /mem off [path]        detach — picker if no path

DICTATION VOCAB
  /vocab                 show current entries
  /vocab add <hint>      append a line
  /vocab remove <substr> drop matching lines

DEBUG
  /tokens  (or /stats)   token breakdown + conversation shape
  /debug                 show full API payload
  /debug expand          verbose mode
  /debug collapse        back to default

SHELL
  cx                     chat
  cx doc [file]          open editor + cx side-by-side (tmux)
  cx vim [file]          open doc in nvim with cx bridge (no split)
  cx incognito           (or cx -i) ephemeral chat, no memory, deleted on quit`

// ── Streaming ─────────────────────────────────────────────────────────────────

// stopStream cancels any in-flight stream or compaction and bumps the
// generation so late async messages from it are dropped. Safe to call when
// nothing is running.
func (m *Model) stopStream() {
	if m.cancelStream != nil {
		m.cancelStream()
	}
	m.streaming = false
	m.streamCh = nil
	m.cancelStream = nil
	m.streamGen++
}

func (m Model) startStream() (Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan string, 128)

	m.streaming = true
	m.streamGen++
	gen := m.streamGen
	m.cancelStream = cancel
	m.streamCh = ch
	m.streamBuf.Reset()

	msgs := m.buildLLMMessages()
	m.pendingImages = nil // consumed
	m.pendingFiles = nil  // consumed
	m.pendingSel = nil    // consumed
	var tools []llm.Tool
	if m.webSearch && strings.Contains(m.model, "/") {
		tools = webTools()
	}
	return m, tea.Batch(
		runAgent(ctx, m.provider, m.model, msgs, tools, m.cfg, ch, gen),
		listenToken(ch, gen),
		streamTick(),
	)
}

// runAgent streams the response, executing any tool calls (web search, page
// fetch) the model requests and looping until it answers with plain content.
// Tool activity is streamed inline as italic status lines, so it is visible
// live and preserved in the saved message for follow-up context.
func runAgent(ctx context.Context, prov llm.Provider, model string, msgs []llm.Message, tools []llm.Tool, cfg *config.Config, ch chan<- string, gen int) tea.Cmd {
	return func() tea.Msg {
		emit := func(t string) {
			select {
			case ch <- t:
			case <-ctx.Done():
			}
		}
		defer close(ch)
		var full strings.Builder
		cappedOut := true
		for round := 0; round < maxToolRounds; round++ {
			content, calls, err := prov.StreamTools(ctx, model, msgs, tools, emit)
			if err != nil && ctx.Err() == nil {
				return streamErrMsg{gen: gen, err: err.Error()}
			}
			full.WriteString(content)
			if len(calls) == 0 || ctx.Err() != nil {
				cappedOut = false
				break
			}
			msgs = append(msgs, llm.Message{Role: "assistant", Content: content, ToolCalls: calls})
			// Run this round's tool calls IN PARALLEL. Wall-clock latency
			// drops from sum(individual) to max(individual). Results are
			// preserved in the original call order so tool_call_id lookups
			// stay deterministic. Each finished call emits its italic
			// status line the moment it completes, so the user sees live
			// progress instead of a frozen "searching..." block.
			type toolResult struct {
				status, result string
			}
			results := make([]toolResult, len(calls))
			var wg sync.WaitGroup
			var emitMu sync.Mutex
			for i := range calls {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					status, result := execWebTool(ctx, cfg, calls[i])
					results[i] = toolResult{status: status, result: result}
					line := "\n\n*" + status + "*\n\n"
					emitMu.Lock()
					emit(line)
					full.WriteString(line)
					emitMu.Unlock()
				}(i)
			}
			wg.Wait()
			for i, r := range results {
				msgs = append(msgs, llm.Message{Role: "tool", ToolCallID: calls[i].ID, Content: r.result})
			}
		}
		// If we exited by hitting the tool-round cap, the last round's tool
		// results never got a follow-up call — the model spent all its
		// rounds tool-calling and never synthesized an answer, leaving the
		// user staring at a wall of "searching..." lines and no reply. Do
		// one final tool-less call so it MUST answer from what it has.
		if cappedOut && ctx.Err() == nil {
			nudge := "\n\n*tool budget reached; writing the answer now*\n\n"
			emit(nudge)
			full.WriteString(nudge)
			content, _, err := prov.StreamTools(ctx, model, msgs, nil, emit)
			if err != nil && ctx.Err() == nil {
				return streamErrMsg{gen: gen, err: err.Error()}
			}
			full.WriteString(content)
		}
		return streamEndMsg{gen: gen, content: strings.TrimSpace(full.String())}
	}
}

func streamTick() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg {
		return streamTickMsg{}
	})
}

func listenToken(ch <-chan string, gen int) tea.Cmd {
	return func() tea.Msg {
		t, ok := <-ch
		if !ok {
			return nil
		}
		return tokenMsg{gen: gen, text: t}
	}
}

func (m Model) buildLLMMessages() []llm.Message {
	out := []llm.Message{{Role: "system", Content: m.systemPrompt}}
	// Attach the live documents (read fresh so external edits are picked up)
	if len(m.docs) > 0 {
		docs := make([]*attachedDoc, 0, len(m.docs))
		for _, d := range m.docs {
			if data, err := os.ReadFile(d.path); err == nil {
				docs = append(docs, &attachedDoc{path: d.path, content: string(data)})
			}
		}
		if len(docs) > 0 {
			out = append(out, llm.Message{Role: "system", Content: docsContextMsg(docs)})
		}
	}
	// Editor highlight riding along this turn
	if m.pendingSel != nil {
		out = append(out, llm.Message{Role: "system", Content: selectionContextMsg(m.pendingSel)})
	}
	// Everything before the last compaction summary is already covered by it —
	// start there so reloaded conversations stay compacted.
	start := 0
	for i, msg := range m.messages {
		if msg.Role == "summary" {
			start = i
		}
	}
	for i, msg := range m.messages {
		if i < start {
			continue
		}
		if msg.Role == "summary" {
			out = append(out, llm.Message{Role: "system", Content: msg.Content})
			continue
		}
		if msg.Role == "note" {
			// Review feedback is the user's words; sending it as a user turn
			// also keeps the payload valid for models that reject trailing
			// system messages (which silently broke auto-retry).
			out = append(out, llm.Message{Role: "user", Content: msg.Content})
			continue
		}
		if msg.Role != "user" && msg.Role != "assistant" {
			continue // system notes, diff previews — display only
		}
		lm := llm.Message{Role: msg.Role, Content: msg.Content}
		// Attach this message's persisted image or PDF
		if msg.ImagePath != "" {
			if dataURL, err := encodeAttachment(msg.ImagePath); err == nil {
				if strings.ToLower(filepath.Ext(msg.ImagePath)) == ".pdf" {
					lm.Files = append(lm.Files, dataURL)
				} else {
					lm.Images = append(lm.Images, dataURL)
				}
			}
		}
		// Attach session-only pendings to the last user message
		if i == len(m.messages)-1 && msg.Role == "user" {
			lm.Images = append(lm.Images, m.pendingImages...)
			lm.Files = append(lm.Files, m.pendingFiles...)
		}
		out = append(out, lm)
	}
	return out
}

// tokenStatsView renders a breakdown of what's going to the model on the
// next send, plus conversation-shape counts. Same numbers the compaction
// threshold uses, so this doubles as a "how close am I to compaction" check.
func (m Model) tokenStatsView() string {
	sys := llm.EstimateTokens(m.systemPrompt)
	var docTok int
	for _, d := range m.docs {
		docTok += llm.EstimateTokens(d.content)
	}
	// Split transcript into "since last summary" (what actually goes on the
	// wire) vs "before the last summary" (covered by the summary already,
	// not sent again).
	start := 0
	for i, msg := range m.messages {
		if msg.Role == "summary" {
			start = i
		}
	}
	var trTok, uCount, aCount, sCount, noteCount int
	for i, msg := range m.messages {
		switch msg.Role {
		case "user":
			uCount++
		case "assistant":
			aCount++
		case "summary":
			sCount++
		case "note":
			noteCount++
		}
		if i < start {
			continue
		}
		if msg.Role != "user" && msg.Role != "assistant" && msg.Role != "summary" && msg.Role != "note" {
			continue
		}
		trTok += llm.EstimateTokens(msg.Content) + 4
	}
	total := sys + docTok + trTok
	limit := m.cfg.MaxContextTokens
	if limit <= 0 {
		limit = 128000
	}
	pct := 0
	if limit > 0 {
		pct = total * 100 / limit
	}
	compactAt := limit * 3 / 4

	fmtn := func(n int) string {
		if n >= 1000 {
			return fmt.Sprintf("%.1fk", float64(n)/1000)
		}
		return fmt.Sprintf("%d", n)
	}

	var sb strings.Builder
	sb.WriteString("── tokens & stats ──\n")
	fmt.Fprintf(&sb, "  system prompt      %7s tok\n", fmtn(sys))
	if len(m.docs) > 0 {
		fmt.Fprintf(&sb, "  connected docs     %7s tok  (%d file(s))\n", fmtn(docTok), len(m.docs))
	}
	fmt.Fprintf(&sb, "  transcript         %7s tok  (since last summary)\n", fmtn(trTok))
	fmt.Fprintf(&sb, "  ─────────────      ───────────\n")
	fmt.Fprintf(&sb, "  total (next send)  %7s tok  (%d%% of %s limit)\n", fmtn(total), pct, fmtn(limit))
	fmt.Fprintf(&sb, "  compact threshold  %7s tok  (75%% of limit)\n", fmtn(compactAt))
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "  messages           %d user · %d assistant · %d summary · %d notes\n",
		uCount, aCount, sCount, noteCount)
	fmt.Fprintf(&sb, "  model              %s\n", m.model)
	if m.pendingSel != nil {
		fmt.Fprintf(&sb, "  selection queued   L%d-%d in %s\n", m.pendingSel.start, m.pendingSel.end, filepath.Base(m.pendingSel.file))
	}
	if len(m.pendingImages) > 0 || len(m.pendingFiles) > 0 {
		fmt.Fprintf(&sb, "  pending attach     %d image(s), %d file(s) — excluded from token count\n",
			len(m.pendingImages), len(m.pendingFiles))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// contextTokens estimates the text token count of the next API payload.
// Images are deliberately excluded — base64 data URLs would wildly inflate
// the estimate and trigger spurious compaction.
//
// This is called from the status bar, which re-renders on every keystroke
// and tick. Walking every message + doc + system prompt each frame added
// up to visible input lag. Cache is invalidated whenever any of the
// counted inputs change (see invalidateTokens callers).
func (m Model) contextTokens() int {
	sig := m.tokenCacheSignature()
	if m.tokenCacheSig == sig && m.tokenCacheVal > 0 {
		return m.tokenCacheVal
	}
	start := 0
	for i, msg := range m.messages {
		if msg.Role == "summary" {
			start = i
		}
	}
	n := llm.EstimateTokens(m.systemPrompt)
	for _, d := range m.docs {
		n += llm.EstimateTokens(d.content)
	}
	for i, msg := range m.messages {
		if i < start || (msg.Role != "user" && msg.Role != "assistant" && msg.Role != "summary" && msg.Role != "note") {
			continue
		}
		n += llm.EstimateTokens(msg.Content) + 4
	}
	// Mutating value receiver — the cache field lives on Model-by-value, so
	// the write survives via bubbletea's tea.Model contract only when the
	// caller returns the updated model. Since contextTokens has a value
	// receiver, we can't mutate m here. Instead the cache is populated
	// via primeContextTokens (pointer receiver) called from Update paths.
	return n
}

// tokenCacheSignature returns a cheap fingerprint of the inputs contextTokens
// walks. When it changes, the cache is stale.
func (m Model) tokenCacheSignature() string {
	// Length of system prompt + message count + last message ID + doc count
	// + total doc content length. Not cryptographic; just needs to move when
	// any input changes.
	var docLen int
	for _, d := range m.docs {
		docLen += len(d.content)
	}
	var lastID int64
	if n := len(m.messages); n > 0 {
		lastID = m.messages[n-1].ID
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d",
		len(m.systemPrompt), len(m.messages), lastID, len(m.docs), docLen)
}

// primeContextTokens forces a fresh compute and caches the result. Call this
// from mutation sites (streamEnd, handleInput, switchConversation) so the
// hot render path can read a cached value.
func (m *Model) primeContextTokens() {
	sig := m.tokenCacheSignature()
	if m.tokenCacheSig == sig && m.tokenCacheVal > 0 {
		return
	}
	start := 0
	for i, msg := range m.messages {
		if msg.Role == "summary" {
			start = i
		}
	}
	n := llm.EstimateTokens(m.systemPrompt)
	for _, d := range m.docs {
		n += llm.EstimateTokens(d.content)
	}
	for i, msg := range m.messages {
		if i < start || (msg.Role != "user" && msg.Role != "assistant" && msg.Role != "summary" && msg.Role != "note") {
			continue
		}
		n += llm.EstimateTokens(msg.Content) + 4
	}
	m.tokenCacheSig = sig
	m.tokenCacheVal = n
}

// reloadSystemPrompt rebuilds the system prompt from memory.md on disk.
// In incognito mode we keep the base personality prompt only — no memory,
// no external memory — so the model never sees anything about the user.
func (m *Model) reloadSystemPrompt() {
	if m.incognito {
		m.systemPrompt = LoadBasePromptOnly()
		return
	}
	m.systemPrompt = BuildSystemPrompt(config.LoadMemory())
}


// ── Auto-title ────────────────────────────────────────────────────────────────

func (m Model) autoTitleCmd() tea.Cmd {
	// Cheap text-only transcript — no images, no system prompt, capped length.
	var sb strings.Builder
	for _, msg := range m.messages {
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		content := msg.Content
		if len(content) > 500 {
			content = content[:500]
		}
		sb.WriteString(msg.Role + ": " + content + "\n\n")
		if sb.Len() > 3000 {
			break
		}
	}
	transcript := sb.String()

	cfg, st, convID := m.cfg, m.store, m.conv.ID
	mainProv, mainModel := m.provider, m.model

	return func() tea.Msg {
		// Prefer the cheap memory model; fall back to the main model
		model := cfg.MemoryModel
		prov, err := llm.ForModel(model, cfg)
		if err != nil || model == "" {
			prov, model = mainProv, mainModel
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		title, err := prov.Complete(ctx, model, []llm.Message{{
			Role:    "user",
			Content: "Give this conversation a short title (4 words max, no quotes, no punctuation). Reply with only the title.\n\n" + transcript,
		}})
		if err != nil {
			return nil
		}
		title = strings.TrimSpace(title)
		if title == "" {
			return nil
		}
		if i := strings.Index(title, "\n"); i >= 0 {
			title = title[:i]
		}
		if len(title) > 60 {
			title = title[:60]
		}
		st.UpdateTitle(convID, title)
		return titleUpdatedMsg{convID: convID, title: title}
	}
}

// ── Memory curation & explicit edits ─────────────────────────────────────────

const curationPrompt = `You are the long-term memory manager for a personal AI assistant. Your job is to maintain a single markdown file that captures durable knowledge about the user: who they are, their preferences, ongoing projects, tools, workflows, and goals.

Organize the memory into meaningful ## markdown sections. Adapt sections to the content, but the common shape is:

  ## Identity              who they are, background, role, expertise
  ## Preferences           taste, style, communication, how they like to work
  ## Projects              ongoing initiatives, current focus, goals
  ## Tools & Workflow      languages, editors, libraries, environment
  ## Feedback              how they want you to behave (do X, avoid Y) and WHY
  ## References            pointers to external systems (dashboards, repos, docs)
  ## Recent conversations  episodic log — what was discussed and decided, per conversation

Rules:
- Rich, substantive bullets — not one-word tags. Merge related facts into single bullets.
- Consolidate duplicates; generalize when patterns emerge (three similar facts become one insight)
- Drop stale, trivial, or conversation-specific details (except in Recent conversations).
- OVERWRITE stale facts confidently when the user contradicts or updates them. Age, grade,
  role, city, current project, tastes, and traits all change. If a new turn implies an old
  bullet is outdated (e.g. "I graduated" invalidates "grade 12"; "moved to Berlin" invalidates
  a prior city; a corrected preference supersedes the earlier one), REPLACE the old bullet
  rather than stack a contradiction next to it. When something is clearly no longer true,
  delete it — do not hedge with "was X, now Y" unless the history itself matters.
- Every ## section that survives must have at least one bullet.
- Keep the total file under 200 lines. Quality over quantity.
- Never invent facts or extrapolate beyond what was actually said.

The "## Recent conversations" section is an episodic log so future sessions know what was already discussed:
- Exactly one bullet per conversation: "- {date} · {title}: {one sentence: what was discussed, decided, or concluded}"
- The current conversation is "%s" (today: %s). If it already has a bullet, UPDATE that bullet to reflect the conversation so far; otherwise add one at the top of the section. If the title is "Untitled", write a short descriptive title yourself.
- Newest first. Keep at most 15 bullets; drop the oldest beyond that.
- Before dropping an old bullet, promote anything still durable into the appropriate section above.

You are given the current memory file, the full transcript of the current conversation (split into what you've ALREADY curated and what is NEW since then), and short recaps of the user's other recent conversations for cross-conversation consolidation. The new turns are where you should focus — most exchanges teach you nothing durable, and the expected outcome is NO_CHANGES. Only rewrite when you've genuinely learned something worth remembering long-term: a new durable fact, a corrected understanding, a decision that shifts a project, or a meaningful update to the current conversation's bullet. When you do rewrite, you may also consolidate across conversations (merge duplicates, generalize a pattern visible in the recaps) — but never invent.

If nothing should change, reply with exactly: NO_CHANGES
Otherwise reply with ONLY the complete new memory file content — no code fences, no commentary.

Current memory file:
<memory>
%s
</memory>

Other recent conversations (for cross-conversation consolidation; the current conversation is the one titled "%s"):
%s

Current conversation transcript:
<already_curated>
%s
</already_curated>
<new_messages>
%s
</new_messages>`

// otherConvTokenCap caps the cross-conversation recaps so they never dominate
// the prompt — the focus is the current conversation's new turns.
const otherConvTokenCap = 1500

// lastAssistantID returns the message ID of the most recent assistant turn,
// or 0 if there is none. Used to track which turns the memory model has seen.
func lastAssistantID(msgs []*store.Message) int64 {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && msgs[i].ID != 0 {
			return msgs[i].ID
		}
	}
	return 0
}

// buildMemoryTranscript renders the current conversation as a text transcript
// split into two labeled spans: turns the memory model has already seen
// (already_curated) and turns added since (new_messages). lastCuratedID is the
// message ID of the newest turn the model last processed; turns strictly
// after it are "new". Memory/system messages are dropped (the memory file is
// passed separately above, so including it here would be circular). Returns
// the two spans in order.
func (m Model) buildMemoryTranscript(lastCuratedID int64) (already, fresh string) {
	start := 0
	for i, msg := range m.messages {
		if msg.Role == "summary" {
			start = i
		}
	}
	var a, f strings.Builder
	writeTurn := func(w *strings.Builder, role, content string) {
		// Trim runaway messages so a huge paste can't blow the prompt.
		if len(content) > 6000 {
			content = content[:6000] + " …[truncated]"
		}
		fmt.Fprintf(w, "%s: %s\n\n", role, content)
	}
	for i, msg := range m.messages {
		if i < start {
			continue
		}
		if msg.Role == "summary" {
			fmt.Fprintf(&a, "(earlier context) %s\n\n", msg.Content)
			continue
		}
		if msg.Role == "note" {
			// Review feedback is the user's words; treat as user turns.
			if msg.ID != 0 && msg.ID <= lastCuratedID {
				writeTurn(&a, "user", msg.Content)
			} else {
				writeTurn(&f, "user", msg.Content)
			}
			continue
		}
		if msg.Role != "user" && msg.Role != "assistant" {
			continue // system notes, diff previews — display only
		}
		if msg.ID != 0 && msg.ID <= lastCuratedID {
			writeTurn(&a, msg.Role, msg.Content)
		} else {
			writeTurn(&f, msg.Role, msg.Content)
		}
	}
	return a.String(), f.String()
}

// buildOtherConvRecaps renders the most recent other conversations as a compact
// block, using each conversation's latest compaction summary when present and
// falling back to its first user message otherwise. Each line is one
// conversation: "- {title}: {summary-or-first-prompt}". Capped to keep the
// prompt focused on the current chat.
func (m Model) buildOtherConvRecaps() string {
	recaps, err := m.store.RecentSummaries(m.conv.ID, 8)
	if err != nil || len(recaps) == 0 {
		return "(none)"
	}
	var sb strings.Builder
	tokens := 0
	for _, r := range recaps {
		// The store helper packed title + content separated by a NUL byte.
		parts := strings.SplitN(r.Content, "\u0000", 2)
		title := parts[0]
		content := ""
		if len(parts) > 1 {
			content = parts[1]
		}
		if content == "" {
			continue
		}
		// Trim each recap to keep the block bounded.
		if len(content) > 300 {
			content = content[:300] + "…"
		}
		line := fmt.Sprintf("- %s: %s", title, content)
		est := len(line) / 4 // rough token estimate
		if tokens+est > otherConvTokenCap {
			break
		}
		tokens += est
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	if sb.Len() == 0 {
		return "(none)"
	}
	return sb.String()
}

func (m Model) curateMemoryCmd() tea.Cmd {
	// Incognito: never touch memory.md. The whole point of the mode.
	if m.incognito {
		return nil
	}
	// Collect the IDs of all assistant turns still pending curation. The
	// caller appends to m.memPending after each streamEnd; here we snapshot
	// it and clear it, so a concurrent re-entry doesn't double-count.
	if len(m.memPending) == 0 {
		return nil
	}
	pending := m.memPending
	convID := m.conv.ID
	convTitle := m.conv.Title

	// Snapshot the transcript + state under the lock-free values we already
	// hold. The LLM call reads memory.md fresh from disk.
	already, fresh := m.buildMemoryTranscript(m.lastCurated)
	if strings.TrimSpace(fresh) == "" {
		return nil
	}
	other := m.buildOtherConvRecaps()
	memPath := config.MemoryPath()
	cfg := m.cfg
	today := time.Now().Format("2006-01-02")
	mainProv, mainModel := m.provider, m.model

	// Track the newest assistant ID we're about to curate, so on success we
	// advance lastCurated even if the conversation has moved on by the time
	// the call returns.
	var newest int64
	for _, id := range pending {
		if id > newest {
			newest = id
		}
	}
	// newest may be 0 (e.g. synthetic, unpersisted turn): fall back to the
	// last assistant message ID currently in view.
	if newest == 0 {
		for i := len(m.messages) - 1; i >= 0; i-- {
			if m.messages[i].Role == "assistant" && m.messages[i].ID != 0 {
				newest = m.messages[i].ID
				break
			}
		}
	}

	return func() tea.Msg {
		memModel := cfg.MemoryModel
		if memModel == "" {
			memModel = "google/gemini-2.5-flash"
		}
		prov, err := llm.ForModel(memModel, cfg)
		if err != nil {
			// No OpenRouter key etc: fall back to the main provider rather
			// than erroring after every exchange
			prov, memModel = mainProv, mainModel
		}
		if prov == nil {
			return memoryErrorMsg("no provider for memory curation")
		}

		existing, _ := memory.Raw(memPath)
		if existing == "" {
			existing = "(empty — this is a new memory file)"
		}

		prompt := []llm.Message{
			{Role: "user", Content: fmt.Sprintf(curationPrompt, convTitle, today, existing, convTitle, other, already, fresh)},
		}

		// Bigger batch than the old per-turn call: give the model room to
		// reason over the whole transcript.
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		result, err := prov.Complete(ctx, memModel, prompt)
		if err != nil {
			return memoryErrorMsg(err.Error())
		}

		result = strings.TrimSpace(result)
		// Strip code fences if the model wrapped its output anyway
		result = strings.TrimPrefix(result, "```markdown")
		result = strings.TrimPrefix(result, "```md")
		result = strings.TrimPrefix(result, "```")
		result = strings.TrimSuffix(result, "```")
		result = strings.TrimSpace(result)

		if result == "" || result == "NO_CHANGES" || strings.HasPrefix(result, "NO_CHANGES") {
			// Even on NO_CHANGES, advance the cursor so we don't re-send
			// the same turns next time — the model has seen them.
			if newest > 0 {
				memory.MarkCurated(convID, newest)
			}
			return memoryUpdatedMsg{content: ""}
		}
		if newest > 0 {
			memory.MarkCurated(convID, newest)
		}
		return memoryUpdatedMsg{content: result, convID: convID}
	}
}

// editMemoryPrompt is used by /remember and /forget — an explicit instruction
// from the user that the memory model applies to the file.
const editMemoryPrompt = `You are the long-term memory manager for a personal AI assistant. The user has issued an EXPLICIT instruction to update their memory file.

Organize the memory into meaningful ## markdown sections. Adapt sections to the content, but the common shape is:

  ## Identity          who they are, background, role, expertise
  ## Preferences       taste, style, communication, how they like to work
  ## Projects          ongoing initiatives, current focus, goals
  ## Tools & Workflow  languages, editors, libraries, environment
  ## Feedback          how they want you to behave (do X, avoid Y) and WHY
  ## References        pointers to external systems (dashboards, repos, docs)

Rules:
- Follow the user's instruction precisely — this is their explicit ask.
- When adding, place the new fact in the most appropriate section (create one if needed).
- When forgetting/removing, drop the matching content but keep surrounding context intact.
- Rich, substantive bullets — not one-word tags. Merge related facts.
- Every ## section that survives must have at least one bullet.
- Keep the total file under 200 lines.

Reply with ONLY the complete new memory file content — no code fences, no commentary.
If the instruction has literally no effect (e.g. forget something that isn't there), reply with exactly: NO_CHANGES

## Current memory file
<memory>
%s
</memory>

## User instruction
%s`

func (m Model) editMemoryCmd(instruction, successNote string) tea.Cmd {
	memPath := config.MemoryPath()
	cfg := m.cfg
	mainProv, mainModel := m.provider, m.model

	return func() tea.Msg {
		memModel := cfg.MemoryModel
		if memModel == "" {
			memModel = "google/gemini-2.5-flash"
		}
		prov, err := llm.ForModel(memModel, cfg)
		if err != nil {
			prov, memModel = mainProv, mainModel
		}
		if prov == nil {
			return memoryErrorMsg("no provider for memory edits")
		}

		existing, _ := memory.Raw(memPath)
		if existing == "" {
			existing = "(empty — this is a new memory file)"
		}

		prompt := []llm.Message{
			{Role: "user", Content: fmt.Sprintf(editMemoryPrompt, existing, instruction)},
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		result, err := prov.Complete(ctx, memModel, prompt)
		if err != nil {
			return memoryErrorMsg(err.Error())
		}

		result = strings.TrimSpace(result)
		result = strings.TrimPrefix(result, "```markdown")
		result = strings.TrimPrefix(result, "```md")
		result = strings.TrimPrefix(result, "```")
		result = strings.TrimSuffix(result, "```")
		result = strings.TrimSpace(result)

		if result == "" || strings.HasPrefix(result, "NO_CHANGES") {
			return memoryUpdatedMsg{content: "", note: "memory unchanged (no matching content or already reflected)"}
		}
		return memoryUpdatedMsg{content: result, note: successNote}
	}
}

// loadMemoryCursor restores the memory-curation cursor for the current
// conversation and rebuilds memPending as every assistant message ID after
// lastCurated. Called on startup and conversation switch so curation resumes
// exactly where it left off (never re-curating the whole chat, never losing
// pending turns).
func (m *Model) loadMemoryCursor() {
	m.lastCurated = memory.LastCurated(m.conv.ID)
	m.memPending = nil
	for _, msg := range m.messages {
		if msg.Role == "assistant" && msg.ID != 0 && msg.ID > m.lastCurated {
			m.memPending = append(m.memPending, msg.ID)
		}
	}
	m.memIdleGen = 0
}

// maybeFlushMemory returns a curation command if there are pending turns to
// curate for the CURRENT conversation. It snapshots the pending state, clears
// it, and returns nil when there's nothing to do. Used as a best-effort flush
// before leaving a conversation (switch, /new, /fork, compaction, quit).
func (m *Model) maybeFlushMemory() tea.Cmd {
	if len(m.memPending) == 0 || m.curating {
		return nil
	}
	if cmd := m.curateMemoryCmd(); cmd != nil {
		m.memPending = nil
		m.memIdleGen = 0
		return cmd
	}
	return nil
}

// flushMemoryAndSave: precompute the curation-prompt state and spawn a
// detached child (cx _curate) that runs the LLM call and writes memory.md.
// Returns nil so quit is instant.
func (m *Model) flushMemoryAndSave() tea.Cmd {
	if len(m.memPending) == 0 {
		return nil
	}
	already, fresh := m.buildMemoryTranscript(m.lastCurated)
	if strings.TrimSpace(fresh) == "" {
		return nil
	}
	state := flushState{
		ConvTitle: m.conv.Title,
		Today:     time.Now().Format("2006-01-02"),
		Other:     m.buildOtherConvRecaps(),
		Already:   already,
		Fresh:     fresh,
	}
	if spawnMemoryFlush(state) {
		m.memPending = nil
	}
	return nil
}

// WaitForMemoryFlush is a no-op: curation on quit runs in a detached child
// process now, so main.go doesn't need to wait for anything before exiting.
// Kept for compatibility with the previous main.go call.
func WaitForMemoryFlush(_ time.Duration) {}

// ── Context compaction ──// ── Context compaction ───────────────────────────────────────────────────────

const compactionPrompt = `Summarize the following conversation concisely. Preserve key facts, decisions, code snippets, and context needed to continue the conversation. Be thorough but brief.

%s`

func (m Model) startCompaction() (Model, tea.Cmd) {
	m.streaming = true // show streaming indicator
	m.injectSystemLine("compacting context...")

	// Flush pending memory curation first so it sees the real transcript
	// (not a compacted one) and doesn't race with compaction's message
	// mutation.
	flush := m.maybeFlushMemory()

	recentCount := min(6, len(m.messages))
	cutoff := len(m.messages) - recentCount

	// Start after the last prior summary — that history is already compacted.
	start := 0
	for i, msg := range m.messages[:cutoff] {
		if msg.Role == "summary" {
			start = i
		}
	}

	// Build the old messages text for summarization
	var sb strings.Builder
	for _, msg := range m.messages[start:cutoff] {
		if msg.Role != "user" && msg.Role != "assistant" && msg.Role != "summary" && msg.Role != "note" {
			continue
		}
		if msg.Role == "summary" || msg.Role == "note" {
			sb.WriteString("(earlier context) ")
		} else {
			sb.WriteString(msg.Role)
			sb.WriteString(": ")
		}
		sb.WriteString(msg.Content)
		sb.WriteString("\n\n")
	}
	oldText := sb.String()

	// Use the MAIN model for compaction (quality matters)
	prov := m.provider
	model := m.model
	convID := m.conv.ID
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	m.cancelStream = cancel // ctrl+c / /stop / switching aborts compaction too
	m.streamGen++
	gen := m.streamGen

	cmds := []tea.Cmd{
		func() tea.Msg {
			defer cancel()
			prompt := []llm.Message{
				{Role: "user", Content: fmt.Sprintf(compactionPrompt, oldText)},
			}
			result, err := prov.Complete(ctx, model, prompt)
			if err != nil {
				return streamErrMsg{gen: gen, err: "compaction failed: " + err.Error()}
			}
			return compactionDoneMsg{
				gen:     gen,
				convID:  convID,
				summary: "Previous conversation summary:\n" + strings.TrimSpace(result),
			}
		},
		streamTick(), // animate the spinner while compacting
	}
	if flush != nil {
		cmds = append(cmds, flush)
	}
	return m, tea.Batch(cmds...)
}

// ── Picker ────────────────────────────────────────────────────────────────────

func (m Model) enterPicker() (Model, tea.Cmd) {
	convs, err := m.store.ListConversations()
	if err != nil || len(convs) == 0 {
		return m.newConversation()
	}
	m.state = statePicker
	m.pickerConvs = convs
	m.pickerFilter = ""
	m.pickerCursor = 0
	return m, nil
}

func (m Model) updatePicker(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.state = stateChat
		return m, nil

	case tea.KeyCtrlN:
		return m.newConversation()

	case tea.KeyEnter:
		filtered := m.filteredConvs()
		if len(filtered) == 0 {
			return m.newConversation()
		}
		sel := filtered[m.pickerCursor]
		if sel.ID == m.conv.ID {
			m.state = stateChat
			return m, nil
		}
		return m.switchConversation(sel.ID)

	case tea.KeyUp:
		if m.pickerCursor > 0 {
			m.pickerCursor--
		}
		return m, nil

	case tea.KeyDown:
		filtered := m.filteredConvs()
		if m.pickerCursor < len(filtered)-1 {
			m.pickerCursor++
		}
		return m, nil

	case tea.KeyBackspace, tea.KeyDelete:
		if len(m.pickerFilter) > 0 {
			_, size := utf8.DecodeLastRuneInString(m.pickerFilter)
			m.pickerFilter = m.pickerFilter[:len(m.pickerFilter)-size]
			m.pickerCursor = 0
		}
		return m, nil

	case tea.KeySpace:
		m.pickerFilter += " "
		m.pickerCursor = 0
		return m, nil

	case tea.KeyRunes:
		m.pickerFilter += string(msg.Runes)
		m.pickerCursor = 0
		return m, nil
	}
	return m, nil
}

func (m Model) filteredConvs() []*store.Conversation {
	if m.pickerFilter == "" {
		return m.pickerConvs
	}
	q := strings.ToLower(m.pickerFilter)
	var out []*store.Conversation
	for _, c := range m.pickerConvs {
		if strings.Contains(strings.ToLower(c.Title), q) {
			out = append(out, c)
		}
	}
	return out
}

func (m Model) switchConversation(id int64) (Model, tea.Cmd) {
	conv, err := m.store.GetConversation(id)
	if err != nil {
		m.errMsg = err.Error()
		m.state = stateChat
		return m, nil
	}
	msgs, err := m.store.GetMessages(id)
	if err != nil {
		m.errMsg = err.Error()
		m.state = stateChat
		return m, nil
	}

	// The model is globally sticky: switching conversations must NOT flip
	// back to whatever model that conversation used long ago. The user's
	// last selection stays until they pick another.
	m.stopStream()
	// Flush any pending memory curation for the conversation we're leaving.
	// It runs as a background command; the result handler writes memory.md
	// regardless of which conversation is active by then (the cursor is
	// advanced inside the command).
	flush := m.maybeFlushMemory()
	m.conv = conv
	m.messages = msgs
	m.streamBuf.Reset()
	m.autoTitled = conv.Title != "Untitled"
	m.state = stateChat
	m.errMsg = ""
	m.loadConvDocs()
	m.loadMemoryCursor()

	m.refreshContent()
	m.viewport.GotoBottom()
	m.atBottom = true
	if flush != nil {
		return m, flush
	}
	return m, nil
}

func (m Model) newConversation() (Model, tea.Cmd) {
	conv, err := m.store.CreateConversation(m.model)
	if err != nil {
		m.errMsg = err.Error()
		m.state = stateChat
		return m, nil
	}
	m.stopStream()
	flush := m.maybeFlushMemory()
	m.conv = conv
	m.messages = nil
	m.streamBuf.Reset()
	m.autoTitled = false
	m.state = stateChat
	m.errMsg = ""
	m.loadConvDocs()
	m.memPending = nil
	m.lastCurated = 0
	m.memIdleGen = 0

	m.refreshContent()
	m.viewport.GotoBottom()
	m.atBottom = true
	if flush != nil {
		return m, flush
	}
	return m, nil
}

// ── Search (ctrl+g / /grep) ───────────────────────────────────────────────────

func (m Model) enterSearch() (Model, tea.Cmd) {
	convs, _ := m.store.ListConversations()
	convMap := make(map[int64]*store.Conversation, len(convs))
	for _, c := range convs {
		convMap[c.ID] = c
	}

	allMsgs, _ := m.store.SearchMessages("") // load everything
	var results []searchResult
	for _, msg := range allMsgs {
		if msg.Role == "system" {
			continue
		}
		c := convMap[msg.ConvID]
		if c == nil {
			continue
		}
		results = append(results, searchResult{conv: c, msg: msg})
	}

	m.state = stateSearch
	m.searchInput = ""
	m.searchAll = results
	m.searchCursor = 0
	return m, nil
}

func (m Model) updateSearch(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.state = stateChat
		return m, nil

	case tea.KeyEnter:
		filtered := m.filteredSearch()
		if len(filtered) == 0 {
			m.state = stateChat
			return m, nil
		}
		sel := filtered[m.searchCursor]
		return m.switchConversation(sel.conv.ID)

	case tea.KeyUp:
		if m.searchCursor > 0 {
			m.searchCursor--
		}
		return m, nil

	case tea.KeyDown:
		filtered := m.filteredSearch()
		if m.searchCursor < len(filtered)-1 {
			m.searchCursor++
		}
		return m, nil

	case tea.KeyBackspace, tea.KeyDelete:
		if len(m.searchInput) > 0 {
			_, size := utf8.DecodeLastRuneInString(m.searchInput)
			m.searchInput = m.searchInput[:len(m.searchInput)-size]
			m.searchCursor = 0
		}
		return m, nil

	case tea.KeySpace:
		m.searchInput += " "
		m.searchCursor = 0
		return m, nil

	case tea.KeyRunes:
		m.searchInput += string(msg.Runes)
		m.searchCursor = 0
		return m, nil
	}
	return m, nil
}

func (m Model) filteredSearch() []searchResult {
	if m.searchInput == "" {
		return m.searchAll
	}
	q := strings.ToLower(m.searchInput)
	var out []searchResult
	for _, r := range m.searchAll {
		if strings.Contains(strings.ToLower(r.msg.Content), q) ||
			strings.Contains(strings.ToLower(r.conv.Title), q) {
			out = append(out, r)
		}
	}
	return out
}

// ── Model picker (ctrl+t / /models) ──────────────────────────────────────────

func (m Model) enterModelPicker() (Model, tea.Cmd) {
	m.state = stateModelPicker
	m.modelFilter = ""
	m.modelCursor = 0

	// If we already fetched models, reuse them
	if len(m.modelList) > 0 {
		return m, nil
	}

	// Fetch async
	m.modelLoading = true
	apiKey := m.cfg.OpenRouter.APIKey
	return m, func() tea.Msg {
		models, err := llm.FetchOpenRouterModels(apiKey)
		if err != nil {
			return modelsErrorMsg("failed to fetch models: " + err.Error())
		}
		return modelsLoadedMsg(models)
	}
}

func (m Model) updateModelPicker(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.state = stateChat
		return m, nil

	case tea.KeyEnter:
		filtered := m.filteredModels()
		if len(filtered) == 0 {
			return m, nil
		}
		sel := filtered[m.modelCursor]
		prov, err := llm.ForModel(sel.ID, m.cfg)
		if err != nil {
			m.errMsg = err.Error()
			m.state = stateChat
			return m, nil
		}
		m.model = sel.ID
		m.provider = prov
		m.store.UpdateModel(m.conv.ID, sel.ID)
		config.SaveLastModel(sel.ID)
		m.state = stateChat
		m.injectSystemLine("switched to " + sel.ID)
		return m, nil

	case tea.KeyUp:
		if m.modelCursor > 0 {
			m.modelCursor--
		}
		return m, nil

	case tea.KeyDown:
		filtered := m.filteredModels()
		if m.modelCursor < len(filtered)-1 {
			m.modelCursor++
		}
		return m, nil

	case tea.KeyBackspace, tea.KeyDelete:
		if len(m.modelFilter) > 0 {
			_, size := utf8.DecodeLastRuneInString(m.modelFilter)
			m.modelFilter = m.modelFilter[:len(m.modelFilter)-size]
			m.modelCursor = 0
		}
		return m, nil

	case tea.KeySpace:
		m.modelFilter += " "
		m.modelCursor = 0
		return m, nil

	case tea.KeyRunes:
		m.modelFilter += string(msg.Runes)
		m.modelCursor = 0
		return m, nil
	}
	return m, nil
}

func (m Model) filteredModels() []llm.ModelInfo {
	if m.modelFilter == "" {
		return m.modelList
	}
	q := strings.ToLower(m.modelFilter)
	var out []llm.ModelInfo
	for _, mi := range m.modelList {
		if strings.Contains(strings.ToLower(mi.ID), q) ||
			strings.Contains(strings.ToLower(mi.Name), q) {
			out = append(out, mi)
		}
	}
	return out
}

func (m Model) modelPickerView() string {
	var sb strings.Builder
	sb.WriteString("\n\n")
	sb.WriteString(pickerTitleStyle.Render("   Models") + "\n\n")

	filterText := m.modelFilter
	if filterText == "" {
		filterText = dimStyle.Render("type to filter...")
	}
	sb.WriteString(promptStyle.Render("   > ") + filterText + "\n\n")

	if m.modelLoading {
		sb.WriteString(dimStyle.Render("   fetching models...") + "\n")
	} else {
		filtered := m.filteredModels()
		if len(filtered) == 0 {
			sb.WriteString(dimStyle.Render("   no matches") + "\n")
		} else {
			maxVisible := max(m.height-10, 1)
			start := 0
			if m.modelCursor >= maxVisible {
				start = m.modelCursor - maxVisible + 1
			}
			end := start + maxVisible
			if end > len(filtered) {
				end = len(filtered)
			}

			for i := start; i < end; i++ {
				mi := filtered[i]
				isCurrent := mi.ID == m.model

				marker := "   "
				if isCurrent {
					marker = " ● "
				}
				if i == m.modelCursor {
					marker = " › "
				}

				name := mi.ID
				maxName := max(m.width-8, 20)
				if utf8.RuneCountInString(name) > maxName {
					name = string([]rune(name)[:maxName-1]) + "…"
				}

				if i == m.modelCursor {
					sb.WriteString(pickerSelectedStyle.Render(marker+name) + "\n")
				} else {
					sb.WriteString(pickerRowStyle.Render(marker+name) + "\n")
				}
			}
			if len(filtered) > maxVisible {
				sb.WriteString("\n")
				sb.WriteString(dimStyle.Render(fmt.Sprintf("   … %d more", len(filtered)-maxVisible)) + "\n")
			}
		}
	}

	sb.WriteString("\n\n")
	sb.WriteString(dimStyle.Render("   ↑↓ navigate  enter select  esc back"))
	return sb.String()
}

// ── Editor (ctrl+e) ───────────────────────────────────────────────────────────

func (m Model) openEditor() (Model, tea.Cmd) {
	// The draft lives in the data dir and is NEVER deleted: whatever happens
	// to cx or the textarea, the text survives at this path.
	// Expand any collapsed paste placeholders so the editor shows the real
	// content — the user wants to edit their text, not "[paste #1, 23 lines]".
	// When the editor closes, editorDoneMsg re-collapses large blocks back
	// into placeholders the normal way.
	draft := filepath.Join(config.DataDir(), "draft.md")
	expanded := expandPastes(m.input.Value(), m.pastes)
	m.pastes = nil // cleared: editor output becomes the canonical text
	if err := os.WriteFile(draft, []byte(expanded), 0o644); err != nil {
		m.errMsg = "could not create draft file: " + err.Error()
		return m, nil
	}

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vim"
	}

	return m, tea.ExecProcess(exec.Command(editor, draft), func(err error) tea.Msg {
		if err != nil {
			return nil
		}
		data, readErr := os.ReadFile(draft)
		if readErr != nil {
			return nil
		}
		return editorDoneMsg(strings.TrimRight(string(data), "\r\n"))
	})
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m Model) View() string {
	if !m.ready {
		return "\n  loading..."
	}
	switch m.state {
	case statePicker:
		return m.pickerView()
	case stateSearch:
		return m.searchView()
	case stateModelPicker:
		return m.modelPickerView()
	case stateDocPicker:
		return m.docPickerView()
	case stateDocListPicker:
		return m.docListPickerView()
	case stateForkPicker:
		return m.forkPickerView()
	case stateMemAddPicker, stateMemRemovePicker:
		return m.memPickerView()
	case stateCopyPicker:
		return m.copyPickerView()
	default:
		return m.chatView()
	}
}

func (m Model) chatView() string {
	// The dictation banner floats above the input so the state stays
	// visible even when the terminal is narrow enough to truncate the
	// status bar. sepView is skipped while it's showing — we don't want
	// two things competing for that row.
	if banner := m.dictationBannerView(); banner != "" {
		return lipgloss.JoinVertical(
			lipgloss.Left,
			m.viewport.View(),
			banner,
			m.inputView(),
			"",
			m.statusView(),
		)
	}
	return lipgloss.JoinVertical(
		lipgloss.Left,
		m.viewport.View(),
		m.sepView(),
		m.inputView(),
		"",
		m.statusView(),
	)
}

func (m Model) sepView() string {
	val := strings.TrimSpace(m.input.Value())
	if _, tok, ok := lastMentionToken(m.input.Value()); ok {
		if cands := mentionCandidates(tok); len(cands) > 0 {
			if len(cands) > 6 {
				cands = append(cands[:6], "…")
			}
			return completionStyle.Render("  @" + strings.Join(cands, "  @"))
		}
	}
	if strings.HasPrefix(val, "/") {
		matches := completionsFor(val)
		if len(matches) > 0 {
			return completionStyle.Render("  " + strings.Join(matches, "  "))
		}
	}
	if m.errMsg != "" {
		return errStyle.Render("  " + m.errMsg)
	}
	return "" // the input box border is separation enough
}

func (m Model) inputView() string {
	border := inputBoxStyle
	if m.streaming {
		border = inputBoxDimStyle
	}
	return border.Width(m.width - 2).Render(m.input.View())
}

// syncInputHeight resizes the textarea and viewport after input content changes.
func (m *Model) syncInputHeight() {
	if h := m.inputHeight(); h != m.input.Height() {
		m.input.SetHeight(h)
	}
	if m.ready {
		m.viewport.Height = m.viewportHeight()
	}
}

func (m Model) inputHeight() int {
	// One spare row beyond the content: the keystroke that wraps onto a new
	// row lands in the spare instead of forcing the textarea's internal
	// viewport to scroll (its offset never heals and hides the first rows).
	// The spare doubles as breathing room, Claude Code style.
	return min(wrappedRows(m.input.Value(), m.input.Width())+1, 12)
}

func (m Model) statusView() string {
	// Shorten model name for display (e.g. "anthropic/claude-sonnet-4-5" → "claude-sonnet-4-5")
	modelDisplay := m.model
	if idx := strings.LastIndex(modelDisplay, "/"); idx >= 0 {
		modelDisplay = modelDisplay[idx+1:]
	}

	left := "  " + modelDisplay + "  ·  " + m.conv.Title
	if m.incognito {
		left += "  ·  🕶 INCOGNITO"
	}
	switch len(m.docs) {
	case 0:
	case 1:
		left += "  ·  doc: " + filepath.Base(m.docs[0].path)
	default:
		left += fmt.Sprintf("  ·  docs: %d", len(m.docs))
	}
	if sel := cachedSelection(); sel != nil && (len(m.docs) == 0 || m.findDoc(sel.file) != nil) {
		left += fmt.Sprintf("  ·  sel L%d-%d", sel.start, sel.end)
	}
	if !m.webSearch {
		left += "  ·  no-web"
	}
	switch {
	case m.dictating:
		el := dictationElapsed()
		if el == "" {
			el = "0:00"
		}
		left += "  ·  🎙 rec " + el + " (ctrl+r to stop)"
	case m.dictationBusy:
		left += "  ·  ⚙ transcribing…"
	}

	tok := m.contextTokens()
	var tokDisplay string
	if tok >= 1000 {
		tokDisplay = fmt.Sprintf("~%.1fk tok", float64(tok)/1000)
	} else {
		tokDisplay = fmt.Sprintf("~%d tok", tok)
	}

	var right string
	if !m.atBottom {
		right = tokDisplay + "  ·  ↑ scroll  "
	} else {
		right = tokDisplay + "  "
	}

	pad := max(m.width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return statusStyle.Render(left + strings.Repeat(" ", pad) + right)
}

// ── Picker view ───────────────────────────────────────────────────────────────

func (m Model) pickerView() string {
	filtered := m.filteredConvs()
	maxVisible := max(m.height-10, 1)

	start := 0
	if m.pickerCursor >= maxVisible {
		start = m.pickerCursor - maxVisible + 1
	}
	end := min(start+maxVisible, len(filtered))

	var sb strings.Builder
	sb.WriteString("\n\n")
	sb.WriteString(pickerTitleStyle.Render("   Conversations") + "\n\n")

	filterText := m.pickerFilter
	if filterText == "" {
		filterText = dimStyle.Render("type to filter...")
	}
	sb.WriteString(promptStyle.Render("   > ") + filterText + "\n\n")

	if len(filtered) == 0 {
		sb.WriteString(dimStyle.Render("   no matches") + "\n")
	} else {
		for i := start; i < end; i++ {
			c := filtered[i]
			age := dimStyle.Render(timeAgo(c.UpdatedAt))
			title := c.Title
			maxTitle := max(m.width-18, 10)
			if utf8.RuneCountInString(title) > maxTitle {
				title = string([]rune(title)[:maxTitle-1]) + "…"
			}

			isCurrent := c.ID == m.conv.ID
			isSelected := i == m.pickerCursor

			marker := "   "
			if isCurrent {
				marker = " ● "
			}
			if isSelected {
				marker = " › "
			}

			line := fmt.Sprintf("%-*s  %s", maxTitle, title, age)

			if isSelected {
				sb.WriteString(pickerSelectedStyle.Render(marker+line) + "\n")
			} else {
				sb.WriteString(pickerRowStyle.Render(marker+line) + "\n")
			}
		}
		if len(filtered) > maxVisible {
			sb.WriteString("\n")
			sb.WriteString(dimStyle.Render(fmt.Sprintf("   … %d more", len(filtered)-maxVisible)) + "\n")
		}
	}

	sb.WriteString("\n\n")
	sb.WriteString(dimStyle.Render("   ↑↓ navigate  enter select  ctrl+n new  esc back"))
	return sb.String()
}

// ── Search view ───────────────────────────────────────────────────────────────

func (m Model) searchView() string {
	filtered := m.filteredSearch()
	maxVisible := max(m.height-10, 1)

	start := 0
	if m.searchCursor >= maxVisible {
		start = m.searchCursor - maxVisible + 1
	}
	end := min(start+maxVisible, len(filtered))

	var sb strings.Builder
	sb.WriteString("\n\n")
	sb.WriteString(pickerTitleStyle.Render("   Search") + "\n\n")

	filterText := m.searchInput
	if filterText == "" {
		filterText = dimStyle.Render("search conversations and messages...")
	}
	sb.WriteString(promptStyle.Render("   > ") + filterText + "\n\n")

	if len(m.searchAll) == 0 {
		sb.WriteString(dimStyle.Render("   no messages yet") + "\n")
	} else if len(filtered) == 0 {
		sb.WriteString(dimStyle.Render("   no matches") + "\n")
	} else {
		for i := start; i < end; i++ {
			r := filtered[i]
			convTitle := r.conv.Title
			maxTitle := 22
			if utf8.RuneCountInString(convTitle) > maxTitle {
				convTitle = string([]rune(convTitle)[:maxTitle-1]) + "…"
			}

			// Preview: first line, truncated
			preview := r.msg.Content
			if nl := strings.Index(preview, "\n"); nl >= 0 {
				preview = preview[:nl]
			}
			maxPreview := max(m.width-maxTitle-16, 20)
			if utf8.RuneCountInString(preview) > maxPreview {
				preview = string([]rune(preview)[:maxPreview-1]) + "…"
			}

			marker := "   "
			if i == m.searchCursor {
				marker = " › "
			}

			titlePart := dimStyle.Render(convTitle)
			line := fmt.Sprintf("%s%s  %s", marker, preview, titlePart)

			if i == m.searchCursor {
				sb.WriteString(pickerSelectedStyle.Render(marker+preview) + "  " + titlePart + "\n")
			} else {
				sb.WriteString(pickerRowStyle.Render(line) + "\n")
			}
		}
		if len(filtered) > maxVisible {
			sb.WriteString("\n")
			sb.WriteString(dimStyle.Render(fmt.Sprintf("   … %d more", len(filtered)-maxVisible)) + "\n")
		}
	}

	sb.WriteString("\n\n")
	sb.WriteString(dimStyle.Render("   ↑↓ navigate  enter open  esc back"))
	return sb.String()
}

// ── Content rendering ─────────────────────────────────────────────────────────

// ensureRenderer (re)builds the shared glamour renderer when the width changes
// and invalidates the per-message render cache.
func (m *Model) ensureRenderer() {
	w := m.viewport.Width - 2
	if w < 10 {
		w = 78
	}
	if m.mdRenderer != nil && m.mdWidth == w {
		return
	}
	// Wrap 2 cols narrower than the target: glamour adds its own left margin,
	// and lines that land exactly at terminal width spill one word onto the
	// next row (the "orphan word" glitch).
	r, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(w-2),
	)
	if err == nil {
		m.mdRenderer = r
	}
	m.mdWidth = w
	m.mdCache = make(map[*store.Message]string)
}

func (m *Model) refreshContent() {
	if !m.ready {
		return
	}
	m.ensureRenderer()
	var sb strings.Builder
	sb.WriteString("\n")
	for _, msg := range m.messages {
		rendered, ok := m.mdCache[msg]
		if !ok {
			rendered = m.renderMsg(msg)
			m.mdCache[msg] = rendered
		}
		sb.WriteString(rendered)
		sb.WriteString("\n")
	}
	if m.streaming {
		sb.WriteString(assistantLabelStyle.Render("cx") + "\n")
		if m.streamBuf.Len() > 0 {
			sb.WriteString(m.renderMarkdown(stripEditBlocks(m.streamBuf.String())))
			sb.WriteString(cursorStyle.Render(" █"))
		} else {
			frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
			frame := frames[int(time.Now().UnixMilli()/100)%len(frames)]
			sb.WriteString(dimStyle.Render("  " + frame + " thinking..."))
		}
	}
	m.viewport.SetContent(sb.String())
}

func (m Model) renderMsg(msg *store.Message) string {
	w := m.viewport.Width
	if w < 10 {
		w = 80
	}

	switch msg.Role {
	case "user":
		label := userStyle.Render("you")
		wrapped := wordWrap(msg.Content, w-4) // 2 indent + 2 safety
		lines := strings.Split(wrapped, "\n")
		for i, line := range lines {
			lines[i] = "  " + line
		}
		return label + "\n" + strings.Join(lines, "\n") + "\n"

	case "summary":
		if !m.verbose {
			return dimStyle.Render("── context compacted ──") + "\n"
		}
		lines := strings.Split(wordWrap(msg.Content, w-4), "\n")
		for i, l := range lines {
			lines[i] = dimStyle.Render("  " + l)
		}
		return dimStyle.Render("── context ──") + "\n" + strings.Join(lines, "\n") + "\n"

	case "note":
		if !m.verbose {
			// clean mode: one clipped dim line (the full text still goes to
			// the model; /debug expand shows it)
			first := msg.Content
			if i := strings.IndexByte(first, '\n'); i >= 0 {
				first = first[:i]
			}
			return dimStyle.Render("  "+truncate(first, max(w-4, 20))) + "\n"
		}
		lines := strings.Split(wordWrap(msg.Content, w-4), "\n")
		for i, l := range lines {
			lines[i] = dimStyle.Render("  " + l)
		}
		return strings.Join(lines, "\n") + "\n"

	case "system":
		lines := strings.Split(wordWrap(msg.Content, w-4), "\n")
		for i, l := range lines {
			lines[i] = dimStyle.Render("  " + l)
		}
		return strings.Join(lines, "\n") + "\n"

	case "diff":
		// Pre-styled doc-edit preview — render as-is
		return msg.Content + "\n"

	default: // assistant
		label := assistantLabelStyle.Render("cx")
		body := m.renderMarkdown(stripEditBlocks(msg.Content))
		return label + "\n" + body + "\n"
	}
}

// renderMarkdown renders content with the shared glamour renderer;
// falls back to plain wrap on error.
func (m *Model) renderMarkdown(content string) string {
	if m.mdRenderer == nil {
		return wordWrap(content, m.mdWidth)
	}
	out, err := m.mdRenderer.Render(content)
	if err != nil {
		return wordWrap(content, m.mdWidth)
	}
	// Glamour adds a trailing newline; trim it since we add our own separator
	return strings.TrimRight(out, "\n")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (m Model) viewportHeight() int {
	// sep(1) + input box borders(2) + input(dynamic) + gap(1) + status(1)
	base := m.height - 5 - m.inputHeight()
	// While the dictation banner is showing it replaces the 1-row sep with
	// a 4-row bordered card, so the viewport gives up 3 extra rows.
	if m.dictating || m.dictationBusy {
		base -= 3
	}
	return max(base, 1)
}

// wordWrap wraps text at width, preserving explicit newlines.
func wordWrap(text string, width int) string {
	if width <= 0 {
		return text
	}
	var result strings.Builder
	paragraphs := strings.Split(text, "\n")
	for i, p := range paragraphs {
		if i > 0 {
			result.WriteByte('\n')
		}
		result.WriteString(wrapParagraph(p, width))
	}
	return result.String()
}

func wrapParagraph(text string, width int) string {
	if utf8.RuneCountInString(text) <= width {
		return text
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return text
	}
	var lines []string
	var cur strings.Builder
	curLen := 0
	for _, w := range words {
		wLen := utf8.RuneCountInString(w)
		switch {
		case curLen == 0:
			cur.WriteString(w)
			curLen = wLen
		case curLen+1+wLen <= width:
			cur.WriteByte(' ')
			cur.WriteString(w)
			curLen += 1 + wLen
		default:
			lines = append(lines, cur.String())
			cur.Reset()
			cur.WriteString(w)
			curLen = wLen
		}
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	return strings.Join(lines, "\n")
}

// lastMentionToken splits input at an in-progress trailing @mention.
// Returns everything before the token, the query after @, and ok.
func lastMentionToken(input string) (string, string, bool) {
	if input == "" || strings.HasSuffix(input, " ") {
		return "", "", false
	}
	i := strings.LastIndexAny(input, " \n")
	tok := input[i+1:]
	if !strings.HasPrefix(tok, "@") {
		return "", "", false
	}
	return input[:i+1], tok[1:], true
}

// isKnownCommand reports whether input starts with a known /command verb.
func isKnownCommand(input string) bool {
	verb := strings.SplitN(strings.TrimSpace(input), " ", 2)[0]
	for _, c := range commands {
		if strings.SplitN(strings.TrimSpace(c), " ", 2)[0] == verb {
			return true
		}
	}
	return false
}

func completionsFor(input string) []string {
	if !strings.HasPrefix(input, "/") {
		return nil
	}
	if strings.Contains(strings.SplitN(input, " ", 2)[0][1:], "/") {
		return nil // an absolute path, not a command
	}
	var out []string
	for _, c := range commands {
		if strings.HasPrefix(c, input) {
			out = append(out, c)
		}
	}
	return out
}

// splitPathToken extracts a leading file path from input, handling quoted
// paths ('...' or "...") and backslash-escaped spaces (foo\ bar.png) the way
// terminals produce them on drag-and-drop. Returns the path and the rest.
//
// Also handles the macOS-Terminal quirk where dragging multiple files at
// once concatenates the paths with NO separator between them: after we
// take one path, if it contains an image extension followed by a "/"
// (start of the next path), we split there.
func splitPathToken(s string) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	// Quoted path
	if s[0] == '\'' || s[0] == '"' {
		q := s[0]
		if i := strings.IndexByte(s[1:], q); i >= 0 {
			return s[1 : 1+i], strings.TrimSpace(s[i+2:])
		}
	}
	// Unquoted: respect backslash-escaped spaces
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == ' ' {
			b.WriteByte(' ')
			i += 2
			continue
		}
		if s[i] == ' ' {
			break
		}
		b.WriteByte(s[i])
		i++
	}
	head := b.String()
	rest := strings.TrimSpace(s[i:])

	// Multi-drag concatenation guard: if `head` contains an image extension
	// followed by a slash (start of another absolute path), split there and
	// push the tail back onto rest.
	for _, ext := range []string{".png/", ".jpg/", ".jpeg/", ".gif/", ".webp/", ".pdf/"} {
		if idx := strings.Index(strings.ToLower(head), ext); idx >= 0 {
			// Split just before the "/" so the tail starts with an absolute path
			cut := idx + len(ext) - 1
			if rest == "" {
				rest = head[cut:]
			} else {
				rest = head[cut:] + " " + rest
			}
			head = head[:cut]
			break
		}
	}
	return head, rest
}

// detectImagePathsInInput extracts every resolvable image path from a drag-
// drop input. Handles every terminal quirk we've hit:
//
//   - one path per line
//   - N paths concatenated on ONE line with no separator (`foo.png/bar.png`)
//   - `\ ` escape sequences
//   - mixed escaping in the same filename
//   - path appended directly to prose with no separator (`...end./var/...png`)
//
// Approach: unescape. For each occurrence of an image extension in the
// input, walk BACKWARD through every `/` (or `~` at a word boundary) trying
// each as a candidate path start. First candidate substring that `os.Stat`
// resolves to an existing file wins. Consumed spans are removed from the
// leftover text so a "drag image + type prose" send still delivers both.
func (m Model) detectImagePathsInInput(input string) (paths []string, rest string) {
	s := strings.ReplaceAll(input, `\ `, ` `)

	exts := []string{".png", ".jpg", ".jpeg", ".gif", ".webp"}
	lower := strings.ToLower(s)

	type span struct{ start, end int }
	var consumed []span

	cursor := 0
	for cursor < len(s) {
		// Find next earliest image-extension hit from cursor.
		bestIdx := -1
		bestEnd := 0
		for _, ext := range exts {
			if i := strings.Index(lower[cursor:], ext); i >= 0 {
				abs := cursor + i
				end := abs + len(ext)
				if bestIdx < 0 || abs < bestIdx {
					bestIdx = abs
					bestEnd = end
				}
			}
		}
		if bestIdx < 0 {
			break
		}
		// Walk back from bestIdx through every `/` (or word-boundary `~`)
		// trying each as a candidate path start. First stat that resolves
		// is our path. Path resolution is O(N) per candidate, but paths
		// are short so total cost is negligible.
		found := -1
		for i := bestIdx; i >= 0; i-- {
			isPathStart := false
			switch {
			case s[i] == '/':
				isPathStart = true
			case s[i] == '~':
				if i == 0 || s[i-1] == ' ' || s[i-1] == '\n' || s[i-1] == '\t' {
					isPathStart = true
				}
			}
			if !isPathStart {
				continue
			}
			cand := s[i:bestEnd]
			p := cand
			if strings.HasPrefix(p, "~") {
				if home, err := os.UserHomeDir(); err == nil {
					p = home + p[1:]
				}
			}
			if _, statErr := os.Stat(p); statErr == nil {
				found = i
				paths = append(paths, p)
				break
			}
		}
		if found >= 0 {
			consumed = append(consumed, span{start: found, end: bestEnd})
		}
		cursor = bestEnd
	}

	// Build leftover text by removing consumed spans.
	var leftover strings.Builder
	prev := 0
	for _, sp := range consumed {
		if sp.start > prev {
			leftover.WriteString(s[prev:sp.start])
		}
		prev = sp.end
	}
	if prev < len(s) {
		leftover.WriteString(s[prev:])
	}
	rest = strings.TrimSpace(strings.Join(strings.Fields(leftover.String()), " "))
	return paths, rest
}

// detectImagePath checks if the input starts with a file path ending in an image extension.
// Returns the resolved path, remaining text, and true if detected.
func (m Model) detectImagePath(input string) (string, string, bool) {
	candidate, rest := splitPathToken(input)
	if candidate == "" {
		return "", "", false
	}

	ext := strings.ToLower(filepath.Ext(candidate))
	if _, ok := imageExts[ext]; !ok {
		return "", "", false
	}

	// Expand ~ and resolve
	p := candidate
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = home + p[1:]
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", "", false
	}
	if _, err := os.Stat(abs); err != nil {
		return "", "", false
	}
	return abs, rest, true
}

// Note: no .svg — vision APIs reject image/svg+xml data URLs.
var imageExts = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp",
}

// encodeAttachment turns a local image or PDF into a data URL.
func encodeAttachment(path string) (string, error) {
	if strings.ToLower(filepath.Ext(path)) == ".pdf" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if len(data) > 32*1024*1024 {
			return "", fmt.Errorf("pdf too large (>32MB)")
		}
		return "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(data), nil
	}
	return encodeImageToDataURL(path)
}

func encodeImageToDataURL(path string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	mime, ok := imageExts[ext]
	if !ok {
		return "", fmt.Errorf("unsupported image type: %s", ext)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	// Cap at 20MB
	if len(data) > 20*1024*1024 {
		return "", fmt.Errorf("image too large (%d MB, max 20MB)", len(data)/1024/1024)
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	return "data:" + mime + ";base64," + encoded, nil
}

// pasteClipboardImage writes the clipboard image to the data dir and returns its path.
// macOS: pngpaste (brew install pngpaste). Linux: xclip.
func pasteClipboardImage() (string, error) {
	dir := filepath.Join(config.DataDir(), "images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("paste-%d.png", time.Now().UnixNano()))

	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("pngpaste"); err != nil {
			return "", fmt.Errorf("pngpaste not installed (brew install pngpaste)")
		}
		if out, err := exec.Command("pngpaste", path).CombinedOutput(); err != nil {
			return "", fmt.Errorf("no image on clipboard (%s)", strings.TrimSpace(string(out)))
		}
	case "linux":
		if _, err := exec.LookPath("xclip"); err != nil {
			return "", fmt.Errorf("xclip not installed")
		}
		data, err := exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-o").Output()
		if err != nil || len(data) == 0 {
			return "", fmt.Errorf("no image on clipboard")
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("clipboard paste not supported on %s", runtime.GOOS)
	}
	return path, nil
}

func copyToClipboard(text string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "pbcopy"
	case "linux":
		name = "xclip"
		args = []string{"-selection", "clipboard"}
	default:
		name = "pbcopy" // best effort
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}

func timeAgo(unix int64) string {
	d := time.Since(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return time.Unix(unix, 0).Format("Jan 2")
	}
}

// ── Fork picker (/fork) ───────────────────────────────────────────────────────

// enterForkPicker lists this conversation's prompts, newest first.
func (m Model) enterForkPicker() (Model, tea.Cmd) {
	var items []*store.Message
	for i := len(m.messages) - 1; i >= 0; i-- {
		if m.messages[i].Role == "user" && m.messages[i].ID > 0 {
			items = append(items, m.messages[i])
		}
	}
	if len(items) == 0 {
		m.errMsg = "no prompts to fork from"
		return m, nil
	}
	m.state = stateForkPicker
	m.forkItems = items
	m.forkFilter = ""
	m.forkCursor = 0
	return m, nil
}

func (m Model) filteredForkItems() []*store.Message {
	if m.forkFilter == "" {
		return m.forkItems
	}
	q := strings.ToLower(m.forkFilter)
	var exact, fuzzy []*store.Message
	for _, msg := range m.forkItems {
		lc := strings.ToLower(msg.Content)
		switch {
		case strings.Contains(lc, q):
			exact = append(exact, msg)
		case fuzzyMatch(q, lc):
			fuzzy = append(fuzzy, msg)
		}
	}
	return append(exact, fuzzy...)
}

func (m Model) updateForkPicker(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.state = stateChat
		return m, nil

	case tea.KeyEnter:
		filtered := m.filteredForkItems()
		if len(filtered) == 0 {
			m.state = stateChat
			return m, nil
		}
		return m.forkFrom(filtered[m.forkCursor])

	case tea.KeyUp:
		if m.forkCursor > 0 {
			m.forkCursor--
		}
		return m, nil

	case tea.KeyDown:
		if m.forkCursor < len(m.filteredForkItems())-1 {
			m.forkCursor++
		}
		return m, nil

	case tea.KeyBackspace, tea.KeyDelete:
		if len(m.forkFilter) > 0 {
			_, size := utf8.DecodeLastRuneInString(m.forkFilter)
			m.forkFilter = m.forkFilter[:len(m.forkFilter)-size]
			m.forkCursor = 0
		}
		return m, nil

	case tea.KeySpace:
		m.forkFilter += " "
		m.forkCursor = 0
		return m, nil

	case tea.KeyRunes:
		m.forkFilter += string(msg.Runes)
		m.forkCursor = 0
		return m, nil
	}
	return m, nil
}

// forkFrom rewrites the CURRENT chat's history in place: everything from the
// chosen prompt onwards is deleted, and the picked prompt is loaded into the
// input for editing. No new conversation is created — the current chat now
// diverges from that point when the user hits enter.
func (m Model) forkFrom(sel *store.Message) (Model, tea.Cmd) {
	m.stopStream() // cancel any streaming so we don't append a stale reply
	if err := m.store.DeleteMessagesFrom(m.conv.ID, sel.ID); err != nil {
		m.errMsg = "fork failed: " + err.Error()
		m.state = stateChat
		return m, nil
	}
	// Drop those messages from the live slice too, using the same predicate
	// as the DB (id >= sel.ID). Messages are stored in insertion order so a
	// tail-trim by ID matches the SQL DELETE.
	kept := m.messages[:0]
	for _, msg := range m.messages {
		if msg.ID != 0 && msg.ID >= sel.ID {
			continue
		}
		kept = append(kept, msg)
	}
	m.messages = kept
	// The memory model's episodic bullet described the pre-fork history that
	// still exists after the truncation — no rewrite needed.
	m.state = stateChat
	m.errMsg = ""
	m.streamBuf.Reset()
	m.lastApplied = nil // the following turns are gone, so /undo scope resets too
	m.input.SetValue(stripPlaceholders(sel.Content))
	m.input.CursorEnd()
	m.syncInputHeight()
	m.injectSystemLine("history rewritten from this prompt — edit and hit enter to send")
	m.refreshContent()
	m.viewport.GotoBottom()
	m.atBottom = true
	return m, nil
}

func (m Model) forkPickerView() string {
	filtered := m.filteredForkItems()
	maxVisible := max(m.height-10, 1)
	start := 0
	if m.forkCursor >= maxVisible {
		start = m.forkCursor - maxVisible + 1
	}
	end := min(start+maxVisible, len(filtered))

	var sb strings.Builder
	sb.WriteString("\n\n")
	sb.WriteString(pickerTitleStyle.Render("   Fork from prompt  ") + dimStyle.Render("(newest first)") + "\n\n")

	filterText := m.forkFilter
	if filterText == "" {
		filterText = dimStyle.Render("type to filter...")
	}
	sb.WriteString(promptStyle.Render("   > ") + filterText + "\n\n")

	if len(filtered) == 0 {
		sb.WriteString(dimStyle.Render("   no matches") + "\n")
	} else {
		for i := start; i < end; i++ {
			preview := stripPlaceholders(filtered[i].Content)
			if nl := strings.IndexByte(preview, '\n'); nl >= 0 {
				preview = preview[:nl]
			}
			preview = truncate(preview, max(m.width-10, 20))
			if i == m.forkCursor {
				sb.WriteString(pickerSelectedStyle.Render(" › "+preview) + "\n")
			} else {
				sb.WriteString(pickerRowStyle.Render("   "+preview) + "\n")
			}
		}
		if len(filtered) > maxVisible {
			sb.WriteString("\n" + dimStyle.Render(fmt.Sprintf("   … %d more", len(filtered)-maxVisible)) + "\n")
		}
	}

	sb.WriteString("\n\n")
	sb.WriteString(dimStyle.Render("   ↑↓ navigate  enter fork  esc back"))
	return sb.String()
}

// ── External memory picker (/mem) ─────────────────────────────────────────────
//
// External memory files are the user's persistent notes (vault entries, life
// planning docs, whatever) that get pasted into the system prompt of every
// conversation as read-only context. Managed globally, not per-chat: attach a
// file with /mem, drop one with /mem off. Contents flow through the system
// prompt (see externalMemorySection in prompt.go).

// handleMemCommand routes /mem subcommands:
//
//	/mem              → fuzzy picker over cwd (add)
//	/mem <path>       → add path directly (tilde/relative resolved)
//	/mem list         → show currently attached external memory files
//	/mem off          → picker to detach one
//	/mem off <path>   → detach path directly
func (m Model) handleMemCommand(parts []string) (Model, tea.Cmd) {
	if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
		return m.enterMemAddPicker()
	}
	arg := strings.TrimSpace(parts[1])
	switch {
	case arg == "list":
		paths := config.LoadExternalMemoryPaths()
		if len(paths) == 0 {
			m.injectSystemLine("no external memory files attached. use /mem <path> or /mem to pick one")
			return m, nil
		}
		m.injectSystemLine("── external memory files ──\n" + strings.Join(paths, "\n"))
		return m, nil

	case arg == "off":
		return m.enterMemRemovePicker()

	case strings.HasPrefix(arg, "off "):
		target := strings.TrimSpace(strings.TrimPrefix(arg, "off "))
		if target == "" {
			return m.enterMemRemovePicker()
		}
		if err := config.RemoveExternalMemoryPath(target); err != nil {
			m.errMsg = "mem off: " + err.Error()
			return m, nil
		}
		m.injectSystemLine("detached from external memory: " + target)
		return m, nil

	default:
		abs, err := config.AddExternalMemoryPath(arg)
		if err != nil {
			m.errMsg = "mem: " + err.Error()
			return m, nil
		}
		m.injectSystemLine("attached as external memory: " + abs)
		return m, nil
	}
}

func (m Model) enterMemAddPicker() (Model, tea.Cmd) {
	files := listDocFiles()
	if len(files) == 0 {
		m.errMsg = "no .md/.txt files under " + cwdBase() + " (use /mem <path> directly)"
		return m, nil
	}
	m.state = stateMemAddPicker
	m.memMode = "add"
	m.memFiles = files
	m.memFilter = ""
	m.memCursor = 0
	return m, nil
}

func (m Model) enterMemRemovePicker() (Model, tea.Cmd) {
	paths := config.LoadExternalMemoryPaths()
	if len(paths) == 0 {
		m.injectSystemLine("no external memory files attached")
		return m, nil
	}
	m.state = stateMemRemovePicker
	m.memMode = "remove"
	m.memFiles = paths
	m.memFilter = ""
	m.memCursor = 0
	return m, nil
}

func (m Model) filteredMemFiles() []string {
	if m.memFilter == "" {
		return m.memFiles
	}
	q := strings.ToLower(m.memFilter)
	var exact, fuzzy []string
	for _, f := range m.memFiles {
		lf := strings.ToLower(f)
		switch {
		case strings.Contains(lf, q):
			exact = append(exact, f)
		case fuzzyMatch(q, lf):
			fuzzy = append(fuzzy, f)
		}
	}
	return append(exact, fuzzy...)
}

func (m Model) updateMemPicker(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.state = stateChat
		return m, nil

	case tea.KeyEnter:
		filtered := m.filteredMemFiles()
		if len(filtered) == 0 {
			m.state = stateChat
			return m, nil
		}
		picked := filtered[m.memCursor]
		m.state = stateChat
		if m.memMode == "remove" {
			if err := config.RemoveExternalMemoryPath(picked); err != nil {
				m.errMsg = "mem off: " + err.Error()
				return m, nil
			}
			m.injectSystemLine("detached from external memory: " + picked)
			return m, nil
		}
		abs, err := config.AddExternalMemoryPath(picked)
		if err != nil {
			m.errMsg = "mem: " + err.Error()
			return m, nil
		}
		m.injectSystemLine("attached as external memory: " + abs)
		return m, nil

	case tea.KeyUp:
		if m.memCursor > 0 {
			m.memCursor--
		}
		return m, nil

	case tea.KeyDown:
		if m.memCursor < len(m.filteredMemFiles())-1 {
			m.memCursor++
		}
		return m, nil

	case tea.KeyBackspace, tea.KeyDelete:
		if len(m.memFilter) > 0 {
			_, size := utf8.DecodeLastRuneInString(m.memFilter)
			m.memFilter = m.memFilter[:len(m.memFilter)-size]
			m.memCursor = 0
		}
		return m, nil

	case tea.KeySpace:
		m.memFilter += " "
		m.memCursor = 0
		return m, nil

	case tea.KeyRunes:
		m.memFilter += string(msg.Runes)
		m.memCursor = 0
		return m, nil
	}
	return m, nil
}

func (m Model) memPickerView() string {
	filtered := m.filteredMemFiles()
	maxVisible := max(m.height-10, 1)
	start := 0
	if m.memCursor >= maxVisible {
		start = m.memCursor - maxVisible + 1
	}
	end := min(start+maxVisible, len(filtered))

	title := "   Attach external memory  "
	sub := "(" + cwdBase() + ")"
	if m.memMode == "remove" {
		title = "   Detach external memory  "
		sub = "(currently attached)"
	}

	var sb strings.Builder
	sb.WriteString("\n\n")
	sb.WriteString(pickerTitleStyle.Render(title) + dimStyle.Render(sub) + "\n\n")

	filterText := m.memFilter
	if filterText == "" {
		filterText = dimStyle.Render("type to filter...")
	}
	sb.WriteString(promptStyle.Render("   > ") + filterText + "\n\n")

	if len(filtered) == 0 {
		sb.WriteString(dimStyle.Render("   no matches") + "\n")
	} else {
		for i := start; i < end; i++ {
			label := truncate(filtered[i], max(m.width-10, 20))
			if i == m.memCursor {
				sb.WriteString(pickerSelectedStyle.Render(" › "+label) + "\n")
			} else {
				sb.WriteString(pickerRowStyle.Render("   "+label) + "\n")
			}
		}
		if len(filtered) > maxVisible {
			sb.WriteString("\n" + dimStyle.Render(fmt.Sprintf("   … %d more", len(filtered)-maxVisible)) + "\n")
		}
	}

	verb := "attach"
	if m.memMode == "remove" {
		verb = "detach"
	}
	sb.WriteString("\n\n")
	sb.WriteString(dimStyle.Render("   ↑↓ navigate  enter " + verb + "  esc back"))
	return sb.String()
}

// ── Dictation vocab (/vocab) ─────────────────────────────────────────────────
//
// The dictation cleanup pass consumes ~/.config/cx/dictation-vocab.txt as
// custom-vocabulary hints (proper nouns, easily-misheard words). These
// commands make it trivial to keep the file up to date without leaving cx.

// handleVocabCommand routes:
//
//	/vocab                        → show the file inline
//	/vocab add <hint>             → append a line (auto-seeds defaults if missing)
//	/vocab remove <substr>        → drop lines containing substr (case-insensitive)
//	/vocab edit                   → open the file in $EDITOR
func (m Model) handleVocabCommand(parts []string) (Model, tea.Cmd) {
	if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
		content := config.LoadDictationVocab()
		// Skip the boilerplate comments in the display so a busy vocab is
		// still readable inline. Full contents still land via /vocab edit.
		var out []string
		for _, ln := range strings.Split(content, "\n") {
			t := strings.TrimSpace(ln)
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			out = append(out, ln)
		}
		if len(out) == 0 {
			m.injectSystemLine("dictation vocab is empty. use /vocab add <hint>")
			return m, nil
		}
		m.injectSystemLine("── dictation vocab ──\n" + strings.Join(out, "\n"))
		return m, nil
	}
	arg := strings.TrimSpace(parts[1])
	switch {
	case strings.HasPrefix(arg, "add "):
		line := strings.TrimSpace(strings.TrimPrefix(arg, "add "))
		if line == "" {
			m.errMsg = "usage: /vocab add <hint>"
			return m, nil
		}
		if err := config.AppendDictationVocab(line); err != nil {
			m.errMsg = "vocab: " + err.Error()
			return m, nil
		}
		m.injectSystemLine("added to dictation vocab: " + line)
		return m, nil

	case strings.HasPrefix(arg, "remove "):
		query := strings.TrimSpace(strings.TrimPrefix(arg, "remove "))
		if query == "" {
			m.errMsg = "usage: /vocab remove <substring>"
			return m, nil
		}
		content := config.LoadDictationVocab()
		q := strings.ToLower(query)
		var kept []string
		dropped := 0
		for _, ln := range strings.Split(content, "\n") {
			t := strings.TrimSpace(ln)
			// Preserve blank + comment lines untouched; only match real entries.
			if t == "" || strings.HasPrefix(t, "#") {
				kept = append(kept, ln)
				continue
			}
			if strings.Contains(strings.ToLower(t), q) {
				dropped++
				continue
			}
			kept = append(kept, ln)
		}
		if dropped == 0 {
			m.errMsg = "no vocab lines matched " + query
			return m, nil
		}
		if err := config.SaveDictationVocab(strings.Join(kept, "\n")); err != nil {
			m.errMsg = "vocab: " + err.Error()
			return m, nil
		}
		m.injectSystemLine(fmt.Sprintf("dropped %d vocab line(s) matching %q", dropped, query))
		return m, nil

	default:
		m.errMsg = "usage: /vocab [add <hint> | remove <substr>]"
		return m, nil
	}
}

// ── Copy picker (/copy with no args) ─────────────────────────────────────────

func (m Model) enterCopyPicker() (Model, tea.Cmd) {
	items := m.copyItemsForConv()
	if len(items) == 0 {
		m.errMsg = "no messages to copy in this chat"
		return m, nil
	}
	m.state = stateCopyPicker
	m.copyItems = items
	m.copyFilter = ""
	m.copyCursor = 0
	m.copyAllConvs = false
	return m, nil
}

func (m Model) copyItemsForConv() []searchResult {
	var items []searchResult
	for _, msg := range m.messages {
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		if msg.ID == 0 {
			continue // display-only
		}
		items = append(items, searchResult{conv: m.conv, msg: msg})
	}
	return items
}

func (m Model) copyItemsAllConvs() []searchResult {
	convs, _ := m.store.ListConversations()
	convMap := make(map[int64]*store.Conversation, len(convs))
	for _, c := range convs {
		convMap[c.ID] = c
	}
	allMsgs, _ := m.store.SearchMessages("")
	var items []searchResult
	for _, msg := range allMsgs {
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		c := convMap[msg.ConvID]
		if c == nil {
			continue
		}
		items = append(items, searchResult{conv: c, msg: msg})
	}
	return items
}

func (m Model) filteredCopyItems() []searchResult {
	if m.copyFilter == "" {
		return m.copyItems
	}
	q := strings.ToLower(m.copyFilter)
	var out []searchResult
	for _, r := range m.copyItems {
		if strings.Contains(strings.ToLower(r.msg.Content), q) ||
			strings.Contains(strings.ToLower(r.conv.Title), q) {
			out = append(out, r)
		}
	}
	return out
}

func (m Model) updateCopyPicker(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.state = stateChat
		return m, nil

	case tea.KeyEnter:
		filtered := m.filteredCopyItems()
		if len(filtered) == 0 {
			m.state = stateChat
			return m, nil
		}
		sel := filtered[m.copyCursor]
		text := sel.msg.Content
		if sel.msg.Role == "assistant" {
			text = stripEditBlocks(text)
		} else {
			text = stripPlaceholders(text)
		}
		m.state = stateChat
		if err := copyToClipboard(text); err != nil {
			m.errMsg = "copy failed: " + err.Error()
			return m, nil
		}
		role := "response"
		if sel.msg.Role == "user" {
			role = "prompt"
		}
		m.injectSystemLine("copied " + role + " to clipboard")
		return m, nil

	case tea.KeyTab:
		if m.copyAllConvs {
			m.copyAllConvs = false
			m.copyItems = m.copyItemsForConv()
		} else {
			m.copyAllConvs = true
			m.copyItems = m.copyItemsAllConvs()
		}
		m.copyFilter = ""
		m.copyCursor = 0
		return m, nil

	case tea.KeyUp:
		if m.copyCursor > 0 {
			m.copyCursor--
		}
		return m, nil

	case tea.KeyDown:
		filtered := m.filteredCopyItems()
		if m.copyCursor < len(filtered)-1 {
			m.copyCursor++
		}
		return m, nil

	case tea.KeyBackspace, tea.KeyDelete:
		if len(m.copyFilter) > 0 {
			_, size := utf8.DecodeLastRuneInString(m.copyFilter)
			m.copyFilter = m.copyFilter[:len(m.copyFilter)-size]
			m.copyCursor = 0
		}
		return m, nil

	case tea.KeySpace:
		m.copyFilter += " "
		m.copyCursor = 0
		return m, nil

	case tea.KeyRunes:
		m.copyFilter += string(msg.Runes)
		m.copyCursor = 0
		return m, nil
	}
	return m, nil
}

func (m Model) copyPickerView() string {
	filtered := m.filteredCopyItems()
	maxVisible := max(m.height-10, 1)
	start := 0
	if m.copyCursor >= maxVisible {
		start = m.copyCursor - maxVisible + 1
	}
	end := min(start+maxVisible, len(filtered))

	scope := "this chat"
	if m.copyAllConvs {
		scope = "all chats"
	}

	var sb strings.Builder
	sb.WriteString("\n\n")
	sb.WriteString(pickerTitleStyle.Render("   Copy message  ") + dimStyle.Render("("+scope+")") + "\n\n")

	filterText := m.copyFilter
	if filterText == "" {
		filterText = dimStyle.Render("type to filter...")
	}
	sb.WriteString(promptStyle.Render("   > ") + filterText + "\n\n")

	if len(m.copyItems) == 0 {
		sb.WriteString(dimStyle.Render("   no messages") + "\n")
	} else if len(filtered) == 0 {
		sb.WriteString(dimStyle.Render("   no matches") + "\n")
	} else {
		for i := start; i < end; i++ {
			r := filtered[i]
			role := "cx "
			if r.msg.Role == "user" {
				role = "you"
			}
			preview := r.msg.Content
			if r.msg.Role == "assistant" {
				preview = stripEditBlocks(preview)
			} else {
				preview = stripPlaceholders(preview)
			}
			if nl := strings.IndexByte(preview, '\n'); nl >= 0 {
				preview = preview[:nl]
			}
			preview = strings.TrimSpace(preview)
			maxPreview := max(m.width-16, 20)
			if utf8.RuneCountInString(preview) > maxPreview {
				preview = string([]rune(preview)[:maxPreview-1]) + "…"
			}
			roleStyled := dimStyle.Render(role)
			if i == m.copyCursor {
				sb.WriteString(" › " + roleStyled + "  " + pickerSelectedStyle.Render(preview) + "\n")
			} else {
				sb.WriteString(pickerRowStyle.Render("   "+role+"  "+preview) + "\n")
			}
		}
		if len(filtered) > maxVisible {
			sb.WriteString("\n" + dimStyle.Render(fmt.Sprintf("   … %d more", len(filtered)-maxVisible)) + "\n")
		}
	}

	sb.WriteString("\n\n")
	sb.WriteString(dimStyle.Render("   up/down navigate  enter copy  tab all-chats  esc back"))
	return sb.String()
}

// togglePasteUnderCursor expands the paste placeholder the cursor is on
// (or the nearest one), or collapses it back if already expanded. This is
// the ctrl+x "expand paste" action.
func (m Model) togglePasteUnderCursor() (Model, tea.Cmd) {
	if len(m.pastes) == 0 {
		return m, nil
	}
	val := m.input.Value()
	// Compute byte offset of cursor in the full value using line number.
	// m.input.Line() returns the 0-based row; sum line lengths up to that row.
	cursorLine := m.input.Line()
	lines := strings.Split(val, "\n")
	byteOff := 0
	for i := 0; i < cursorLine && i < len(lines); i++ {
		byteOff += len(lines[i]) + 1 // +1 for the \n
	}
	// Add the column offset within the current line (CharOffset is in runes,
	// convert to bytes for ASCII-safe search; the placeholders are all ASCII).
	li := m.input.LineInfo()
	byteOff += li.CharOffset

	// Find which placeholder the cursor is inside (or nearest to).
	bestIdx := -1
	bestDist := 1<<31 - 1
	for i, p := range m.pastes {
		start := strings.Index(val, p.placeholder)
		if start < 0 {
			continue
		}
		end := start + len(p.placeholder)
		if byteOff >= start && byteOff <= end {
			// Cursor is inside this placeholder — expand it.
			bestIdx = i
			bestDist = 0
			break
		}
		// Distance to nearest edge.
		dist := byteOff - end
		if byteOff < start {
			dist = start - byteOff
		}
		if dist < bestDist {
			bestDist = dist
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return m, nil
	}
	p := m.pastes[bestIdx]
	// Replace the placeholder with the full text in the textarea.
	newVal := strings.Replace(val, p.placeholder, p.text, 1)
	m.pastes = append(m.pastes[:bestIdx], m.pastes[bestIdx+1:]...)
	m.input.SetValue(newVal)
	// Move cursor to just after the expanded text.
	cursorAt := strings.Index(newVal, p.text) + len(p.text)
	// Set cursor to end for now (textarea doesn't expose set-by-offset).
	m.input.CursorEnd()
	_ = cursorAt
	m.syncInputHeight()
	return m, nil
}
