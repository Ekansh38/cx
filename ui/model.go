package ui

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	stateDocPicker              // filesystem document picker for :doc
	stateDocListPicker          // connected-docs picker (:disconnect doc / :doc edit)
)

// ── tea.Msg types ─────────────────────────────────────────────────────────────

type tokenMsg string
type streamEndMsg struct{ content string }
type streamErrMsg string
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
}
type memoryErrorMsg string
type compactionDoneMsg struct{ summary string }

// ── search result ─────────────────────────────────────────────────────────────

type searchResult struct {
	conv *store.Conversation
	msg  *store.Message
}

// ── available :commands ───────────────────────────────────────────────────────

var commands = []string{
	":clear", ":connect doc", ":copy", ":copy prompt", ":debug",
	":delete", ":disconnect doc", ":doc",
	":edit", ":forget ", ":grep", ":help", ":img ",
	":list", ":memory", ":model ", ":models", ":new",
	":paste", ":q", ":quit", ":r", ":remember ",
	":rename ", ":retry", ":sel", ":stop", ":wipe",
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
	viewport     viewport.Model
	input        textarea.Model
	pendingImage string // base64 data URL for next message
	atBottom     bool
	autoTitled   bool   // prevent re-triggering auto-title
	errMsg       string // shown on separator row, cleared on next input

	// streaming
	streaming    bool
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
	docs       []*attachedDoc
	docReview  bool
	docEdits   []docEdit
	docEditIdx int
	extGroups  []editGroup   // per-file edit batches queued for in-editor review
	extNotes   []string      // per-file review summaries accumulated across groups
	extRetry   bool          // a rejection carried a note: auto-retry when done
	pendingSel *docSelection // editor highlight riding along the next message

	// doc picker state (:doc with no path)
	docFiles       []string
	docFilter      string
	docCursor      int
	docPickerQuits bool // launched via `cx doc`: esc quits instead of returning to chat

	// connected-docs list picker state (:disconnect doc / :doc edit)
	docListMode   string // "open" or "disconnect"
	docListItems  []string
	docListFilter string
	docListCursor int

	// system
	systemPrompt string

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
	ta.MaxHeight = 10
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
	}
	// Restore connected docs from a previous session
	m.loadConvDocs()
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
		m.input.SetWidth(msg.Width - 4)
		m.refreshContent() // re-wrap on every resize
		if m.atBottom {
			m.viewport.GotoBottom()
		}
		return m, nil

	case docReloadMsg:
		m.reloadDocs()
		return m, nil

	case extReviewTickMsg:
		if len(m.extGroups) == 0 {
			return m, nil
		}
		// N fired mid-review: send the revision request right away, while the
		// user keeps reviewing the remaining hunks.
		if !m.streaming && m.provider != nil {
			if reasons := consumeRejectNow(); len(reasons) > 0 {
				var sb strings.Builder
				for _, r := range reasons {
					fmt.Fprintf(&sb, "I rejected a proposed edit: %q. ", r)
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
				return m, nil
			}
			return m, extReviewTick(msg.n + 1)
		}
		m.reloadDocs() // neovim wrote the file

		// A rejection with a note means "try again, differently": retry once
		// all groups are reviewed, unless it already fired via reject-now.
		for _, r := range results {
			if !r.Applied && !r.Reported && r.Reason != "" && r.Reason != "not found in buffer" {
				m.extRetry = true
			}
		}
		g := m.extGroups[0]
		m.extGroups = m.extGroups[1:]
		m.extNotes = append(m.extNotes, summarizeReview(results, filepath.Base(g.file)))

		// More files to review: hand the next group to neovim
		if len(m.extGroups) > 0 {
			if startExternalReview(m.extGroups[0].file, m.extGroups[0].edits) {
				return m, extReviewTick(0)
			}
			m.extNotes = append(m.extNotes, fmt.Sprintf("review of %s could not start", filepath.Base(m.extGroups[0].file)))
			m.extGroups = nil
		}

		note := strings.Join(m.extNotes, "; ")
		retry := m.extRetry
		m.extNotes = nil
		m.extRetry = false
		if retry {
			note += ". Revise the rejected edits to address the notes and propose updated <edit> blocks."
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
			return m.startStream()
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
		// Just accumulate — the 150ms streamTick refreshes the view.
		m.streamBuf.WriteString(string(msg))
		return m, listenToken(m.streamCh)

	case streamErrMsg:
		m.streaming = false
		m.cancelStream = nil
		m.streamCh = nil
		m.streamBuf.Reset()
		m.errMsg = string(msg)
		return m, nil

	case streamEndMsg:
		// If the stream was cancelled (ctrl+c / :stop), the partial response
		// was already saved — drop this late completion to avoid duplicates.
		if !m.streaming {
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

		// Doc mode: hand proposed edits to neovim for review (per file, in
		// sequence), or fall back to the in-cx y/n flow without the bridge.
		var reviewCmd tea.Cmd
		if len(m.docs) > 0 && content != "" {
			m.reloadDocs() // match edits against the latest on-disk content
			edits := explodeEdits(resolveEditFiles(parseDocEdits(content), m.docs), m.docs)
			if len(edits) > 0 && len(m.extGroups) > 0 {
				// A review is already on screen in neovim: queue these after it
				m.extGroups = append(m.extGroups, groupEditsByFile(edits)...)
				m.injectSystemLine(fmt.Sprintf("queued %d more edits behind the current review", len(edits)))
			} else if len(edits) > 0 {
				groups := groupEditsByFile(edits)
				if len(groups) > 0 && startExternalReview(groups[0].file, groups[0].edits) {
					m.extGroups = groups
					m.extNotes = nil
					m.extRetry = false
					word := "edits"
					if len(edits) == 1 {
						word = "edit"
					}
					m.injectSystemLine(fmt.Sprintf("proposed %d %s in neovim", len(edits), word))
					reviewCmd = extReviewTick(0)
				} else {
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
		if !m.autoTitled && m.conv.Title == "Untitled" && len(m.messages) >= 2 {
			m.autoTitled = true
			cmds = append(cmds, m.autoTitleCmd())
		}
		if cmd := m.curateMemoryCmd(); cmd != nil {
			cmds = append(cmds, cmd)
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
		m.input.SetValue(strings.TrimSpace(string(msg)))
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

	case memoryUpdatedMsg:
		if msg.content != "" {
			memory.SaveRaw(config.MemoryPath(), msg.content)
			m.reloadSystemPrompt()
		}
		if msg.note != "" {
			m.injectSystemLine(msg.note)
		}
		return m, nil

	case memoryErrorMsg:
		if string(msg) != "" {
			m.injectSystemLine("memory error: " + string(msg))
		}
		return m, nil

	case compactionDoneMsg:
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

	switch msg.Type {

	case tea.KeyCtrlC:
		if m.streaming {
			m.cancelStream()
			m.streaming = false
			m.streamCh = nil
			m.cancelStream = nil
			// Keep whatever was streamed so far
			partial := m.streamBuf.String()
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
		// Alt+Enter / Shift+Enter handled by textarea (inserts newline)
		input := strings.TrimSpace(m.input.Value())
		if input == "" {
			return m, nil
		}
		if m.streaming && !strings.HasPrefix(input, ":") {
			return m, nil
		}
		m.input.Reset()
		m.syncInputHeight()
		m.errMsg = ""
		return m.handleInput(input)

	case tea.KeyEsc:
		m.input.Reset()
		m.syncInputHeight()
		m.errMsg = ""
		return m, nil

	case tea.KeyTab:
		matches := completionsFor(m.input.Value())
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

	case tea.KeyUp:
		m.viewport.LineUp(1)
		m.atBottom = m.viewport.AtBottom()
		return m, nil

	case tea.KeyDown:
		m.viewport.LineDown(1)
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
	if strings.HasPrefix(input, ":") {
		return m.handleCommand(input)
	}

	// Auto-detect image paths: if the input (or first word) is a file path
	// ending in an image extension, treat it like :img <path> [rest as text].
	// This makes drag-and-drop work — terminals paste the file path.
	if imgPath, text, ok := m.detectImagePath(input); ok {
		return m.handleCommand(`:img "` + imgPath + `" ` + text)
	}

	if m.provider == nil {
		m.errMsg = "no provider (set GEMINI_API_KEY, OPENAI_API_KEY, or configure ollama)"
		return m, nil
	}

	// Pick up an editor highlight, if one was sent over (see README: neovim bridge).
	// Selections from files that aren't connected docs are ignored.
	display := input
	if sel := readSelection(); sel != nil {
		if len(m.docs) == 0 {
			m, _ = m.connectDoc(sel.file) // auto-connect the highlighted file
		}
		if m.findDoc(sel.file) != nil {
			m.pendingSel = sel
			consumeSelection()
			display += fmt.Sprintf("\n[highlighted L%d-%d in %s]", sel.start, sel.end, filepath.Base(sel.file))
		}
	}
	// Re-read the docs so the payload reflects external editor saves
	m.reloadDocs()

	if saved, err := m.store.AddMessage(m.conv.ID, "user", display); err == nil {
		m.messages = append(m.messages, saved)
	} else {
		m.messages = append(m.messages, &store.Message{Role: "user", Content: display})
	}
	m.refreshContent()
	m.viewport.GotoBottom()
	m.atBottom = true

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
	case ":delete confirm":
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

	case ":wipe confirm":
		if err := m.store.WipeAll(); err != nil {
			m.errMsg = "wipe failed: " + err.Error()
			return m, nil
		}
		conv, err := m.store.CreateConversation(m.model)
		if err != nil {
			m.errMsg = err.Error()
			return m, nil
		}
		m.conv = conv
		m.messages = nil
		m.streaming = false
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
	case ":stop":
		if !m.streaming {
			m.errMsg = "not currently streaming"
			return m, nil
		}
		m.cancelStream()
		m.streaming = false
		m.streamCh = nil
		m.cancelStream = nil
		partial := m.streamBuf.String()
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

	case ":q", ":quit":
		if m.streaming {
			m.cancelStream()
		}
		return m, tea.Quit

	case ":new":
		return m.newConversation()

	case ":list":
		return m.enterPicker()

	case ":grep":
		return m.enterSearch()

	case ":clear":
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

	case ":debug":
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

	case ":models":
		return m.enterModelPicker()

	case ":rename":
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			m.errMsg = "usage: :rename <title>"
			return m, nil
		}
		title := strings.TrimSpace(parts[1])
		m.store.UpdateTitle(m.conv.ID, title)
		m.conv.Title = title
		m.autoTitled = true // don't auto-overwrite a manual rename
		return m, nil

	case ":model":
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

	case ":help":
		m.injectSystemLine(helpText)
		return m, nil

	case ":copy":
		// :copy = last assistant message, :copy prompt = your last message
		role, what := "assistant", "response"
		if len(parts) > 1 && strings.TrimSpace(parts[1]) == "prompt" {
			role, what = "user", "prompt"
		}
		var last string
		for i := len(m.messages) - 1; i >= 0; i-- {
			if m.messages[i].Role == role {
				last = m.messages[i].Content
				break
			}
		}
		if role == "user" {
			// Strip display-only placeholder lines
			var lines []string
			for line := range strings.SplitSeq(last, "\n") {
				if !strings.HasPrefix(line, "[image: ") && !strings.HasPrefix(line, "[highlighted ") {
					lines = append(lines, line)
				}
			}
			last = strings.TrimSpace(strings.Join(lines, "\n"))
		}
		if last == "" {
			m.errMsg = "no " + what + " to copy"
			return m, nil
		}
		if err := copyToClipboard(last); err != nil {
			m.errMsg = "copy failed: " + err.Error()
			return m, nil
		}
		m.errMsg = ""
		m.injectSystemLine("copied " + what + " to clipboard")
		return m, nil

	case ":delete":
		m.injectSystemLine("delete this conversation? type  :delete confirm  to proceed.")
		return m, nil

	case ":edit":
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

	case ":retry", ":r":
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
		// Remove any assistant reply after it (keep the user message)
		m.messages = m.messages[:lastUserIdx+1]
		m.refreshContent()
		m.viewport.GotoBottom()
		m.atBottom = true
		return m.startStream()

	case ":img":
		if m.streaming {
			m.errMsg = "wait for the response to finish (or :stop)"
			return m, nil
		}
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			m.errMsg = "usage: :img /path/to/image.png [optional message]"
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
		dataURL, err := encodeImageToDataURL(absPath)
		if err != nil {
			m.errMsg = "image: " + err.Error()
			return m, nil
		}
		// Save with image path, display placeholder
		display := text + "\n[image: " + filepath.Base(absPath) + "]"
		if saved, err := m.store.AddMessageWithImage(m.conv.ID, "user", display, absPath); err == nil {
			saved.ImagePath = absPath
			m.messages = append(m.messages, saved)
		} else {
			m.messages = append(m.messages, &store.Message{Role: "user", Content: display, ImagePath: absPath})
		}
		// Stash the data URL for the next buildLLMMessages call
		m.pendingImage = dataURL
		m.refreshContent()
		m.viewport.GotoBottom()
		m.atBottom = true
		if m.contextTokens() > m.cfg.MaxContextTokens*3/4 {
			return m.startCompaction()
		}
		return m.startStream()

	case ":paste":
		if m.streaming {
			m.errMsg = "wait for the response to finish (or :stop)"
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
		return m.handleCommand(`:img "` + path + `" ` + text)

	case ":doc":
		if m.streaming {
			m.errMsg = "wait for the response to finish (or :stop)"
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

	case ":connect":
		if m.streaming {
			m.errMsg = "wait for the response to finish (or :stop)"
			return m, nil
		}
		arg := ""
		if len(parts) > 1 {
			arg = strings.TrimSpace(parts[1])
		}
		if arg != "doc" && !strings.HasPrefix(arg, "doc ") {
			m.errMsg = "usage: :connect doc [path]"
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

	case ":disconnect":
		if len(parts) > 1 && strings.TrimSpace(parts[1]) != "doc" {
			m.errMsg = "usage: :disconnect doc"
			return m, nil
		}
		return m.disconnectDocFlow()

	case ":sel":
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
		note := "(attached to your next message · :sel clear to drop)"
		if len(m.docs) > 0 && m.findDoc(sel.file) == nil {
			note = "(from a file that isn't a connected doc, will NOT be sent)"
		}
		m.injectSystemLine(fmt.Sprintf("── selection: L%d-%d of %s ──\n%s\n%s",
			sel.start, sel.end, filepath.Base(sel.file), preview, note))
		return m, nil

	case ":memory":
		content, err := memory.Raw(config.MemoryPath())
		if err != nil {
			m.errMsg = "memory error: " + err.Error()
			return m, nil
		}
		if content == "" {
			m.injectSystemLine("memory is empty. chat or use :remember <fact>")
			return m, nil
		}
		m.injectSystemLine("── memory file ──\n" + content)
		return m, nil

	case ":remember":
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			m.errMsg = "usage: :remember <fact>"
			return m, nil
		}
		fact := strings.TrimSpace(parts[1])
		m.injectSystemLine("updating memory...")
		return m, m.editMemoryCmd("Remember this: "+fact, "memory updated, remembered: "+fact)

	case ":forget":
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			m.errMsg = "usage: :forget <query>"
			return m, nil
		}
		query := strings.TrimSpace(parts[1])
		m.injectSystemLine("updating memory...")
		return m, m.editMemoryCmd("Forget/remove anything related to: "+query, "memory updated, forgot: "+query)

	case ":wipe":
		m.injectSystemLine("this will delete ALL conversations and messages.\ntype  :wipe confirm  to proceed, or anything else to cancel.")
		return m, nil

	default:
		m.errMsg = "unknown command: " + verb + "  (tab to complete, :help for list)"
		return m, nil
	}
}

// injectSystemLine adds a display-only note into the viewport (not persisted).
func (m *Model) injectSystemLine(text string) {
	// ID=0 marks display-only messages
	m.messages = append(m.messages, &store.Message{Role: "system", Content: text})
	m.refreshContent()
	m.viewport.GotoBottom()
	m.atBottom = true
}

const helpText = `keybindings
  ctrl+c       cancel stream / quit (or :stop)
  ctrl+l       conversation picker
  ctrl+n       new conversation
  ctrl+g       search all messages
  ctrl+t       model switcher
  ctrl+e       open $EDITOR for long input
  ctrl+u / d   scroll half page
  alt+enter    newline (multiline input)
  esc          clear the input field
  ↑ ↓          scroll one line
  tab          autocomplete :command

commands  (type : to see completions)
  :help                 this help
  :q / :quit            quit
  :new                  new conversation
  :list                 conversation picker
  :grep                 search messages
  :copy                 copy last assistant message to clipboard
  :copy prompt          copy your last message to clipboard
  :edit                 edit your last message (loads into input, re-send)
  :retry / :r           re-send last message (gets a new response)
  :img <path> [text]    send an image (png/jpg/gif/webp)
                        (or just paste/drop an image path, auto-detected)
  :paste [text]         send the image on your clipboard
  :doc [path]           connect a document and open it in your editor
                        (no path = fuzzy picker over the current directory)
  :doc edit             reopen a connected doc in your editor
  :doc off              disconnect (same as :disconnect doc)
  :connect doc [path]   connect a doc without opening the editor
                        (no path = the last doc you connected)
  :disconnect doc       disconnect a doc (picker with ALL when several)
  :sel                  preview the editor selection waiting for your next msg
  :sel clear            drop it
  :stop                 stop the current response (same as ctrl+c)
  :delete               delete current conversation (asks confirm)
  :rename <title>       rename this conversation
  :model <name>         switch model mid-conversation
  :models               model switcher (fetches from OpenRouter)
  :memory               show the memory file
  :remember <fact>      ask the memory model to add a fact
  :forget <query>       ask the memory model to drop matching content
  :clear                clear injected notes (history kept)
  :debug                show full API payload
  :wipe                 delete ALL conversations and messages (asks confirm)

doc mode  (:doc / :connect doc)
  documents live in YOUR editor (opened beside cx in tmux);
  cx just knows the files. a chat can have several docs connected;
  all of them are sent to the model every turn, re-read from disk,
  so every save is picked up automatically.
  reference passages as @L12, @L12-30, @## Heading.
  proposed edits are reviewed IN NEOVIM. cx diffs whatever the
  model sends and splits it into minimal hunks, so a typo fix is
  a one-line diff even if the model rewrote the whole file. every
  hunk renders at once (word-level highlighting for small changes)
  and fills the quickfix list: ]q / [q jump between them. with the
  cursor on a hunk:
  [y] apply  [n] skip  [N] reject+note  [a] all  [u] undo  [q] quit
  N fires the revision immediately; new edits queue behind the
  current review.
  (without the neovim bridge, the y/n review happens in cx instead)

neovim side-by-side
  cx doc <file>   (from your shell) sets everything up: a tmux
  session with neovim on the left, cx on the right, doc attached.
  inside tmux, :doc / the doc picker / e also auto-open the editor
  in a split pane. add the keybinding from the README, highlight
  text in neovim, press <leader>cs, and cx attaches the selection to
  your next message (auto-attaching the doc if needed). status bar
  shows "sel L12-30" while one is waiting; :sel previews it.

memory
  cx keeps a structured markdown profile at ~/.config/cx/memory.md,
  organized into sections like Identity / Preferences / Projects /
  Tools & Workflow / Feedback / References.
  after each response, the memory model rewrites the file:
  merging, generalizing, and pruning what it knows about you.
  :remember and :forget also route through the model so edits stay
  organized and don't leave dangling bullets.
  set memory_model in config.toml to pick a stronger curation model.
  context auto-compacted when the conversation gets long.`

// ── Streaming ─────────────────────────────────────────────────────────────────

func (m Model) startStream() (Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan string, 128)

	m.streaming = true
	m.cancelStream = cancel
	m.streamCh = ch
	m.streamBuf.Reset()

	msgs := m.buildLLMMessages()
	m.pendingImage = "" // consumed
	m.pendingSel = nil  // consumed
	return m, tea.Batch(
		runStream(ctx, m.provider, m.model, msgs, ch),
		listenToken(ch),
		streamTick(),
	)
}

func runStream(ctx context.Context, prov llm.Provider, model string, msgs []llm.Message, ch chan<- string) tea.Cmd {
	return func() tea.Msg {
		content, err := prov.Stream(ctx, model, msgs, func(token string) {
			select {
			case ch <- token:
			case <-ctx.Done():
			}
		})
		close(ch)
		if err != nil && ctx.Err() == nil {
			return streamErrMsg(err.Error())
		}
		return streamEndMsg{content: content}
	}
}

func streamTick() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg {
		return streamTickMsg{}
	})
}

func listenToken(ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		t, ok := <-ch
		if !ok {
			return nil
		}
		return tokenMsg(t)
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
		// Attach image if this message has one
		if msg.ImagePath != "" {
			dataURL, err := encodeImageToDataURL(msg.ImagePath)
			if err == nil {
				lm.Images = append(lm.Images, dataURL)
			}
		}
		// Attach pending image to the last user message
		if m.pendingImage != "" && i == len(m.messages)-1 && msg.Role == "user" {
			lm.Images = append(lm.Images, m.pendingImage)
		}
		out = append(out, lm)
	}
	return out
}

// contextTokens estimates the text token count of the next API payload.
// Images are deliberately excluded — base64 data URLs would wildly inflate
// the estimate and trigger spurious compaction.
func (m Model) contextTokens() int {
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
	return n
}

// reloadSystemPrompt rebuilds the system prompt from memory.md on disk.
func (m *Model) reloadSystemPrompt() {
	mem := config.LoadMemory()
	base := "You are a helpful, direct, and thoughtful assistant. Be concise unless detail is needed."
	if strings.TrimSpace(mem) == "" {
		m.systemPrompt = base
	} else {
		m.systemPrompt = base + "\n\n## Things you know about the user\n" + mem
	}
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
- Every ## section that survives must have at least one bullet.
- Keep the total file under 100 lines. Quality over quantity.
- Never invent facts or extrapolate beyond what was actually said.

The "## Recent conversations" section is an episodic log so future sessions know what was already discussed:
- Exactly one bullet per conversation: "- {date} · {title}: {one sentence: what was discussed, decided, or concluded}"
- The current conversation is "%s" (today: %s). If it already has a bullet, UPDATE that bullet to reflect the conversation so far; otherwise add one at the top of the section. If the title is "Untitled", write a short descriptive title yourself — and if the newest bullet from today clearly describes this same discussion, update it rather than adding a duplicate.
- Newest first. Keep at most 15 bullets; drop the oldest beyond that.
- Before dropping an old bullet, promote anything still durable into the appropriate section above.

Below is the current memory file and the latest exchange. Rewrite the memory to incorporate anything genuinely worth remembering.

If nothing should change, reply with exactly: NO_CHANGES
Otherwise reply with ONLY the complete new memory file content — no code fences, no commentary.

Current memory file:
<memory>
%s
</memory>

Latest exchange:
User: %s

Assistant: %s`

func (m Model) curateMemoryCmd() tea.Cmd {
	var userMsg, assistantMsg string
	for i := len(m.messages) - 1; i >= 0; i-- {
		if m.messages[i].Role == "assistant" && assistantMsg == "" {
			assistantMsg = m.messages[i].Content
		}
		if m.messages[i].Role == "user" && userMsg == "" {
			userMsg = m.messages[i].Content
		}
		if userMsg != "" && assistantMsg != "" {
			break
		}
	}
	if userMsg == "" || assistantMsg == "" {
		return nil
	}

	memPath := config.MemoryPath()
	cfg := m.cfg
	convTitle := m.conv.Title
	today := time.Now().Format("2006-01-02")

	return func() tea.Msg {
		memModel := cfg.MemoryModel
		if memModel == "" {
			memModel = "google/gemini-2.5-flash-lite"
		}
		prov, err := llm.ForModel(memModel, cfg)
		if err != nil {
			return memoryErrorMsg(err.Error())
		}

		existing, _ := memory.Raw(memPath)
		if existing == "" {
			existing = "(empty — this is a new memory file)"
		}

		prompt := []llm.Message{
			{Role: "user", Content: fmt.Sprintf(curationPrompt, convTitle, today, existing, userMsg, assistantMsg)},
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		result, err := prov.Complete(ctx, memModel, prompt)
		if err != nil {
			return memoryErrorMsg(err.Error())
		}

		result = strings.TrimSpace(result)
		// Strip code fences if the model wrapped its output anyway
		result = strings.TrimPrefix(result, "```markdown")
		result = strings.TrimPrefix(result, "```")
		result = strings.TrimSuffix(result, "```")
		result = strings.TrimSpace(result)

		if result == "" || result == "NO_CHANGES" || strings.HasPrefix(result, "NO_CHANGES") {
			return memoryUpdatedMsg{content: ""}
		}
		return memoryUpdatedMsg{content: result}
	}
}

// editMemoryPrompt is used by :remember and :forget — an explicit instruction
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
- Keep the total file under 80 lines.

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

	return func() tea.Msg {
		memModel := cfg.MemoryModel
		if memModel == "" {
			memModel = "google/gemini-2.5-flash-lite"
		}
		prov, err := llm.ForModel(memModel, cfg)
		if err != nil {
			return memoryErrorMsg(err.Error())
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
		result = strings.TrimPrefix(result, "```")
		result = strings.TrimSuffix(result, "```")
		result = strings.TrimSpace(result)

		if result == "" || strings.HasPrefix(result, "NO_CHANGES") {
			return memoryUpdatedMsg{content: "", note: "memory unchanged (no matching content or already reflected)"}
		}
		return memoryUpdatedMsg{content: result, note: successNote}
	}
}

// ── Context compaction ───────────────────────────────────────────────────────

const compactionPrompt = `Summarize the following conversation concisely. Preserve key facts, decisions, code snippets, and context needed to continue the conversation. Be thorough but brief.

%s`

func (m Model) startCompaction() (Model, tea.Cmd) {
	m.streaming = true // show streaming indicator
	m.injectSystemLine("compacting context...")

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

	return m, tea.Batch(
		func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			prompt := []llm.Message{
				{Role: "user", Content: fmt.Sprintf(compactionPrompt, oldText)},
			}
			result, err := prov.Complete(ctx, model, prompt)
			if err != nil {
				return streamErrMsg("compaction failed: " + err.Error())
			}
			return compactionDoneMsg{summary: "Previous conversation summary:\n" + strings.TrimSpace(result)}
		},
		streamTick(), // animate the spinner while compacting
	)
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
	m.conv = conv
	m.messages = msgs
	m.streaming = false
	m.streamBuf.Reset()
	m.autoTitled = conv.Title != "Untitled"
	m.state = stateChat
	m.errMsg = ""
	m.loadConvDocs()

	m.refreshContent()
	m.viewport.GotoBottom()
	m.atBottom = true
	return m, nil
}

func (m Model) newConversation() (Model, tea.Cmd) {
	conv, err := m.store.CreateConversation(m.model)
	if err != nil {
		m.errMsg = err.Error()
		m.state = stateChat
		return m, nil
	}
	m.conv = conv
	m.messages = nil
	m.streaming = false
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

// ── Search (ctrl+g / :grep) ───────────────────────────────────────────────────

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

// ── Model picker (ctrl+t / :models) ──────────────────────────────────────────

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
	tmp, err := os.CreateTemp("", "cx-*.md")
	if err != nil {
		m.errMsg = "could not create temp file: " + err.Error()
		return m, nil
	}
	tmpName := tmp.Name()
	tmp.WriteString(m.input.Value())
	tmp.Close()

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vim"
	}

	return m, tea.ExecProcess(exec.Command(editor, tmpName), func(err error) tea.Msg {
		defer os.Remove(tmpName)
		if err != nil {
			return nil
		}
		data, readErr := os.ReadFile(tmpName)
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
	default:
		return m.chatView()
	}
}

func (m Model) chatView() string {
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
	if strings.HasPrefix(val, ":") {
		matches := completionsFor(val)
		if len(matches) > 0 {
			return completionStyle.Render("  " + strings.Join(matches, "  "))
		}
	}
	if m.errMsg != "" {
		return errStyle.Render("  " + m.errMsg)
	}
	return sepStyle.Render(strings.Repeat("─", m.width))
}

func (m Model) inputView() string {
	prefix := promptStyle.Render(" > ")
	if m.streaming {
		prefix = dimStyle.Render(" > ")
	}
	return prefix + m.input.View()
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
	return min(max(m.input.LineCount(), 1), 10)
}

func (m Model) statusView() string {
	// Shorten model name for display (e.g. "anthropic/claude-sonnet-4-5" → "claude-sonnet-4-5")
	modelDisplay := m.model
	if idx := strings.LastIndex(modelDisplay, "/"); idx >= 0 {
		modelDisplay = modelDisplay[idx+1:]
	}

	left := "  " + modelDisplay + "  ·  " + m.conv.Title
	switch len(m.docs) {
	case 0:
	case 1:
		left += "  ·  doc: " + filepath.Base(m.docs[0].path)
	default:
		left += fmt.Sprintf("  ·  docs: %d", len(m.docs))
	}
	if sel := readSelection(); sel != nil && (len(m.docs) == 0 || m.findDoc(sel.file) != nil) {
		left += fmt.Sprintf("  ·  sel L%d-%d", sel.start, sel.end)
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
		lines := strings.Split(wordWrap(msg.Content, w-4), "\n")
		for i, l := range lines {
			lines[i] = dimStyle.Render("  " + l)
		}
		return dimStyle.Render("── context ──") + "\n" + strings.Join(lines, "\n") + "\n"

	case "system", "note":
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
	// sep(1) + input(dynamic) + blank(1) + status(1) = 3 + inputHeight
	return max(m.height-3-m.inputHeight(), 1)
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

func completionsFor(input string) []string {
	if !strings.HasPrefix(input, ":") {
		return nil
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
	return b.String(), strings.TrimSpace(s[i:])
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
