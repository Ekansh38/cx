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

	"github.com/charmbracelet/bubbles/textinput"
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
	stateChat        appState = iota
	statePicker               // conversation picker overlay
	stateSearch               // inline message search
	stateModelPicker          // model switcher
)

// ── tea.Msg types ─────────────────────────────────────────────────────────────

type tokenMsg string
type streamEndMsg struct{ content string }
type streamErrMsg string
type titleUpdatedMsg string
type editorDoneMsg string
type streamTickMsg struct{}
type modelsLoadedMsg []llm.ModelInfo
type modelsErrorMsg string
type memoryExtractedMsg struct{ facts []string }
type memoryErrorMsg string
type compactionDoneMsg struct{ summary string }

// ── search result ─────────────────────────────────────────────────────────────

type searchResult struct {
	conv *store.Conversation
	msg  *store.Message
}

// ── available :commands ───────────────────────────────────────────────────────

var commands = []string{
	":clear", ":copy", ":debug", ":delete",
	":forget ", ":grep", ":help", ":img ",
	":list", ":model ", ":models", ":new",
	":q", ":quit", ":r", ":remember ",
	":rename ", ":retry", ":wipe",
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
	viewport    viewport.Model
	input       textinput.Model
	inputBuf     string // multiline buffer (alt+enter adds lines)
	pendingImage string // base64 data URL for next message
	atBottom     bool
	autoTitled  bool   // prevent re-triggering auto-title
	errMsg      string // shown on separator row, cleared on next input

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
	modelList     []llm.ModelInfo
	modelFilter   string
	modelCursor   int
	modelLoading  bool

	// system
	systemPrompt string
}

// ── constructor ───────────────────────────────────────────────────────────────

func New(cfg *config.Config, st *store.Store, conv *store.Conversation, msgs []*store.Message, prov llm.Provider, modelName, sysPrompt string) Model {
	ti := textinput.New()
	ti.Prompt = "" // we render our own "> " prefix
	ti.Placeholder = "message..."
	ti.Focus()
	ti.CharLimit = 0

	return Model{
		cfg:          cfg,
		store:        st,
		conv:         conv,
		messages:     msgs,
		provider:     prov,
		model:        modelName,
		input:        ti,
		systemPrompt: sysPrompt,
		atBottom:     true,
		state:        stateChat,
		streamBuf:    &strings.Builder{},
	}
}

func (m Model) Init() tea.Cmd {
	return textinput.Blink
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
		m.input.Width = msg.Width - 3
		m.refreshContent() // re-wrap on every resize
		if m.atBottom {
			m.viewport.GotoBottom()
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
		m.streamBuf.WriteString(string(msg))
		m.refreshContent()
		if m.atBottom {
			m.viewport.GotoBottom()
		}
		return m, listenToken(m.streamCh)

	case streamErrMsg:
		m.streaming = false
		m.cancelStream = nil
		m.streamCh = nil
		m.streamBuf.Reset()
		m.errMsg = string(msg)
		return m, nil

	case streamEndMsg:
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

		var cmds []tea.Cmd
		if !m.autoTitled && m.conv.Title == "Untitled" && len(m.messages) >= 2 {
			m.autoTitled = true
			cmds = append(cmds, m.autoTitleCmd())
		}
		if cmd := m.extractMemoryCmd(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if len(cmds) > 0 {
			return m, tea.Batch(cmds...)
		}
		return m, nil

	case titleUpdatedMsg:
		m.conv.Title = string(msg)
		return m, nil

	case editorDoneMsg:
		content := strings.TrimSpace(string(msg))
		if content == "" {
			return m, nil
		}
		// Send editor content directly — preserves all formatting
		return m.handleInput(content)

	case modelsLoadedMsg:
		m.modelList = []llm.ModelInfo(msg)
		m.modelLoading = false
		return m, nil

	case modelsErrorMsg:
		m.errMsg = string(msg)
		m.state = stateChat
		m.modelLoading = false
		return m, nil

	case memoryExtractedMsg:
		for _, fact := range msg.facts {
			memory.Add(config.MemoryPath(), fact)
		}
		m.reloadSystemPrompt()
		return m, nil

	case memoryErrorMsg:
		// Silent — don't disrupt the user
		return m, nil

	case compactionDoneMsg:
		// Replace old messages with summary + recent
		recentCount := 6
		if recentCount > len(m.messages) {
			recentCount = len(m.messages)
		}
		recent := make([]*store.Message, recentCount)
		copy(recent, m.messages[len(m.messages)-recentCount:])
		summaryMsg := &store.Message{Role: "summary", Content: msg.summary}
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
		default:
			return m.updateChat(msg)
		}
	}

	return m, nil
}

// ── Chat key handling ─────────────────────────────────────────────────────────

func (m Model) updateChat(msg tea.KeyMsg) (Model, tea.Cmd) {
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
		if msg.Alt {
			// Alt+Enter: add newline to buffer, keep typing
			m.inputBuf += m.input.Value() + "\n"
			m.input.SetValue("")
			return m, nil
		}
		// Combine buffer + current input
		full := m.inputBuf + m.input.Value()
		input := strings.TrimSpace(full)
		if input == "" {
			return m, nil
		}
		// Allow commands during streaming, block regular messages
		if m.streaming && !strings.HasPrefix(input, ":") {
			return m, nil
		}
		m.input.SetValue("")
		m.inputBuf = ""
		m.errMsg = ""
		return m.handleInput(input)

	case tea.KeyEsc:
		if m.inputBuf != "" {
			// Escape clears the multiline buffer
			m.inputBuf = ""
			return m, nil
		}

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
	return m, cmd
}

// ── Input handling ────────────────────────────────────────────────────────────

func (m Model) handleInput(input string) (Model, tea.Cmd) {
	if strings.HasPrefix(input, ":") {
		return m.handleCommand(input)
	}

	if m.provider == nil {
		m.errMsg = "no provider — set GEMINI_API_KEY, OPENAI_API_KEY, or configure ollama"
		return m, nil
	}

	if saved, err := m.store.AddMessage(m.conv.ID, "user", input); err == nil {
		m.messages = append(m.messages, saved)
	} else {
		m.messages = append(m.messages, &store.Message{Role: "user", Content: input})
	}
	m.refreshContent()
	m.viewport.GotoBottom()
	m.atBottom = true

	// Check if compaction needed before sending
	msgs := m.buildLLMMessages()
	tokenCount := llm.EstimateMessagesTokens(msgs)
	if tokenCount > m.cfg.MaxContextTokens*3/4 {
		return m.startCompaction()
	}
	return m.startStream()
}

func (m Model) handleCommand(input string) (Model, tea.Cmd) {
	input = strings.TrimSpace(input)
	parts := strings.SplitN(input, " ", 2)
	verb := parts[0]

	switch input {
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
		m.refreshContent()
		m.viewport.GotoBottom()
		m.atBottom = true
		return m, nil
	}

	switch verb {
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
		sb.WriteString(fmt.Sprintf("── debug: %s ──\n", m.model))
		sb.WriteString(fmt.Sprintf("── %d messages in payload ──\n\n", len(msgs)))
		for i, msg := range msgs {
			sb.WriteString(fmt.Sprintf("[%d] %s:\n%s\n\n", i, msg.Role, msg.Content))
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
		m.injectSystemLine("switched to " + newModel)
		return m, nil

	case ":help":
		m.injectSystemLine(helpText)
		return m, nil

	case ":copy":
		// Copy last assistant message to clipboard
		var last string
		for i := len(m.messages) - 1; i >= 0; i-- {
			if m.messages[i].Role == "assistant" {
				last = m.messages[i].Content
				break
			}
		}
		if last == "" {
			m.errMsg = "no assistant message to copy"
			return m, nil
		}
		if err := copyToClipboard(last); err != nil {
			m.errMsg = "copy failed: " + err.Error()
			return m, nil
		}
		m.errMsg = ""
		m.injectSystemLine("copied to clipboard")
		return m, nil

	case ":delete":
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
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			m.errMsg = "usage: :img /path/to/image.png [optional message]"
			return m, nil
		}
		rest := strings.TrimSpace(parts[1])
		// Split into path and optional message
		imgPath, text := rest, ""
		if idx := strings.IndexByte(rest, ' '); idx >= 0 {
			imgPath = rest[:idx]
			text = strings.TrimSpace(rest[idx+1:])
		}
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
		msgs := m.buildLLMMessages()
		tokenCount := llm.EstimateMessagesTokens(msgs)
		if tokenCount > m.cfg.MaxContextTokens*3/4 {
			return m.startCompaction()
		}
		return m.startStream()

	case ":remember":
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			m.errMsg = "usage: :remember <fact>"
			return m, nil
		}
		fact := strings.TrimSpace(parts[1])
		added, err := memory.Add(config.MemoryPath(), fact)
		if err != nil {
			m.errMsg = "memory error: " + err.Error()
			return m, nil
		}
		if added {
			m.reloadSystemPrompt()
			m.injectSystemLine("remembered: " + fact)
		} else {
			m.injectSystemLine("already in memory (or similar fact exists)")
		}
		return m, nil

	case ":forget":
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			m.errMsg = "usage: :forget <query>"
			return m, nil
		}
		query := strings.TrimSpace(parts[1])
		n, err := memory.Remove(config.MemoryPath(), query)
		if err != nil {
			m.errMsg = "memory error: " + err.Error()
			return m, nil
		}
		if n > 0 {
			m.reloadSystemPrompt()
			m.injectSystemLine(fmt.Sprintf("forgot %d fact(s) matching %q", n, query))
		} else {
			m.injectSystemLine("no matching facts found")
		}
		return m, nil

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
  ctrl+c       cancel stream / quit
  ctrl+l       conversation picker
  ctrl+n       new conversation
  ctrl+g       search all messages
  ctrl+t       model switcher
  ctrl+e       open $EDITOR for long input
  ctrl+u / d   scroll half page
  alt+enter    newline (multiline input)
  ↑ ↓          scroll one line
  tab          autocomplete :command

commands  (type : to see completions)
  :help                 this help
  :q / :quit            quit
  :new                  new conversation
  :list                 conversation picker
  :grep                 search messages
  :copy                 copy last assistant message to clipboard
  :retry / :r           re-send last message (gets a new response)
  :img <path> [text]    send an image (png/jpg/gif/webp)
  :delete               delete current conversation
  :rename <title>       rename this conversation
  :model <name>         switch model mid-conversation
  :models               model switcher (fetches from OpenRouter)
  :remember <fact>       save a fact to memory
  :forget <query>        remove matching facts from memory
  :clear                clear injected notes (history kept)
  :debug                show full API payload
  :wipe                 delete ALL conversations and messages (asks confirm)

memory
  auto-extracted after each response (via cheap model)
  :remember / :forget for manual control
  stored in ~/.config/cx/memory.md
  context auto-compacted when conversation gets long`

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
	for i, msg := range m.messages {
		if msg.Role == "summary" {
			out = append(out, llm.Message{Role: "system", Content: msg.Content})
			continue
		}
		if msg.Role == "system" {
			continue
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
	msgs := m.buildLLMMessages()
	prov, model, st, convID := m.provider, m.model, m.store, m.conv.ID

	return func() tea.Msg {
		prompt := append(msgs, llm.Message{
			Role:    "user",
			Content: "Give this conversation a short title (4 words max, no quotes, no punctuation). Reply with only the title.",
		})
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		ch := make(chan string, 64)
		go func() {
			prov.Stream(ctx, model, prompt, func(t string) {
				select {
				case ch <- t:
				case <-ctx.Done():
				}
			})
			close(ch)
		}()

		var sb strings.Builder
		for t := range ch {
			sb.WriteString(t)
		}
		title := strings.TrimSpace(sb.String())
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
		return titleUpdatedMsg(title)
	}
}

// ── Auto-memory extraction ───────────────────────────────────────────────────

const extractionPrompt = `You are a memory extraction system. Given a conversation exchange, extract key facts about the user that would be useful to remember across conversations.

Current memory:
%s

Latest exchange:
User: %s
Assistant: %s

Rules:
- Only extract NEW facts not already in memory
- Extract concrete preferences, facts, names, tools, languages, workflows
- Do NOT extract conversation-specific context (what they asked about today)
- Do NOT extract obvious/generic things
- One fact per line, no bullets, no numbering
- If nothing worth remembering, return exactly: NONE`

func (m Model) extractMemoryCmd() tea.Cmd {
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

	return func() tea.Msg {
		memModel := cfg.MemoryModel
		if memModel == "" {
			memModel = "google/gemini-2.0-flash-001"
		}
		prov, err := llm.ForModel(memModel, cfg)
		if err != nil {
			return memoryErrorMsg(err.Error())
		}

		existingFacts, _ := memory.Load(memPath)
		existing := memory.FormatForPrompt(existingFacts)
		if existing == "" {
			existing = "(empty)"
		}

		prompt := []llm.Message{
			{Role: "user", Content: fmt.Sprintf(extractionPrompt, existing, userMsg, assistantMsg)},
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		result, err := prov.Complete(ctx, memModel, prompt)
		if err != nil {
			return memoryErrorMsg(err.Error())
		}

		result = strings.TrimSpace(result)
		if result == "NONE" || result == "" {
			return memoryExtractedMsg{facts: nil}
		}

		var facts []string
		for _, line := range strings.Split(result, "\n") {
			line = strings.TrimSpace(line)
			// Strip common prefixes the model might add
			line = strings.TrimPrefix(line, "- ")
			line = strings.TrimPrefix(line, "* ")
			if line != "" && line != "NONE" {
				facts = append(facts, line)
			}
		}
		return memoryExtractedMsg{facts: facts}
	}
}

// ── Context compaction ───────────────────────────────────────────────────────

const compactionPrompt = `Summarize the following conversation concisely. Preserve key facts, decisions, code snippets, and context needed to continue the conversation. Be thorough but brief.

%s`

func (m Model) startCompaction() (Model, tea.Cmd) {
	m.streaming = true // show streaming indicator
	m.injectSystemLine("compacting context...")

	recentCount := 6
	if recentCount > len(m.messages) {
		recentCount = len(m.messages)
	}
	cutoff := len(m.messages) - recentCount

	// Build the old messages text for summarization
	var sb strings.Builder
	for _, msg := range m.messages[:cutoff] {
		if msg.Role == "system" || msg.Role == "summary" {
			continue
		}
		sb.WriteString(msg.Role)
		sb.WriteString(": ")
		sb.WriteString(msg.Content)
		sb.WriteString("\n\n")
	}
	oldText := sb.String()

	// Use the MAIN model for compaction (quality matters)
	prov := m.provider
	model := m.model

	return m, func() tea.Msg {
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
	}
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
	prov, err := llm.ForModel(conv.Model, m.cfg)
	if err != nil {
		prov = m.provider // keep old provider if model unknown
	}

	m.conv = conv
	m.messages = msgs
	m.model = conv.Model
	m.provider = prov
	m.streaming = false
	m.streamBuf.Reset()
	m.autoTitled = conv.Title != "Untitled"
	m.state = stateChat
	m.errMsg = ""

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
	sb.WriteString("\n")
	sb.WriteString(pickerTitleStyle.Render("  models") + "\n\n")

	filterText := m.modelFilter
	if filterText == "" {
		filterText = dimStyle.Render("type to filter...")
	}
	sb.WriteString(promptStyle.Render("  > ") + filterText + "\n")
	sb.WriteString(sepStyle.Render(strings.Repeat("─", m.width)) + "\n")

	if m.modelLoading {
		sb.WriteString(dimStyle.Render("  fetching models from OpenRouter...") + "\n")
	} else {
		filtered := m.filteredModels()
		if len(filtered) == 0 {
			sb.WriteString(dimStyle.Render("  no matches") + "\n")
		} else {
			maxVisible := m.height - 9
			if maxVisible < 1 {
				maxVisible = 1
			}
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

				dot := "  "
				if isCurrent {
					dot = "● "
				}

				name := mi.ID
				maxName := m.width - 6
				if maxName < 20 {
					maxName = 20
				}
				if utf8.RuneCountInString(name) > maxName {
					name = string([]rune(name)[:maxName-1]) + "…"
				}

				line := dot + name
				if i == m.modelCursor {
					sb.WriteString(pickerSelectedStyle.Render(line) + "\n")
				} else {
					sb.WriteString(pickerRowStyle.Render(line) + "\n")
				}
			}
			if len(filtered) > maxVisible {
				sb.WriteString(dimStyle.Render(fmt.Sprintf("  … %d more", len(filtered)-maxVisible)) + "\n")
			}
		}
	}

	sb.WriteString("\n")
	sb.WriteString(dimStyle.Render("  ↑↓ navigate   enter select   esc cancel"))
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
		m.statusView(),
	)
}

func (m Model) sepView() string {
	val := m.input.Value()
	if strings.HasPrefix(val, ":") {
		matches := completionsFor(val)
		if len(matches) > 0 {
			return completionStyle.Render("  " + strings.Join(matches, "   "))
		}
	}
	if m.errMsg != "" {
		return errStyle.Render("  " + m.errMsg)
	}
	return sepStyle.Render(strings.Repeat("─", m.width))
}

func (m Model) inputView() string {
	prefix := promptStyle.Render("> ")
	if m.streaming {
		prefix = dimStyle.Render("  ")
	} else if m.inputBuf != "" {
		lines := strings.Count(m.inputBuf, "\n") + 1
		prefix = promptStyle.Render(fmt.Sprintf("%d> ", lines))
	}
	return prefix + m.input.View()
}

func (m Model) statusView() string {
	modelPart := m.model
	titlePart := m.conv.Title

	left := fmt.Sprintf("  %s  ·  %s", modelPart, titlePart)

	var right string
	if m.streaming {
		dots := strings.Repeat(".", int(time.Now().UnixMilli()/400)%4)
		right = fmt.Sprintf("  thinking%-4s", dots)
	} else if !m.atBottom {
		right = "  ↑ scroll  "
	} else {
		right = "  "
	}

	pad := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 1 {
		pad = 1
	}
	return statusStyle.Render(left + strings.Repeat(" ", pad) + right)
}

// ── Picker view ───────────────────────────────────────────────────────────────

func (m Model) pickerView() string {
	filtered := m.filteredConvs()
	maxVisible := m.height - 8
	if maxVisible < 1 {
		maxVisible = 1
	}

	// Scroll window around cursor
	start := 0
	if m.pickerCursor >= maxVisible {
		start = m.pickerCursor - maxVisible + 1
	}
	end := start + maxVisible
	if end > len(filtered) {
		end = len(filtered)
	}

	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(pickerTitleStyle.Render("  conversations") + "\n\n")

	// Filter input line
	filterText := m.pickerFilter
	if filterText == "" {
		filterText = dimStyle.Render("type to filter...")
	}
	sb.WriteString(promptStyle.Render("  > ") + filterText + "\n")
	sb.WriteString(sepStyle.Render(strings.Repeat("─", m.width)) + "\n")

	if len(filtered) == 0 {
		sb.WriteString(dimStyle.Render("  no matches") + "\n")
	} else {
		for i := start; i < end; i++ {
			c := filtered[i]
			age := timeAgo(c.UpdatedAt)
			title := c.Title
			maxTitle := m.width - 14
			if maxTitle < 10 {
				maxTitle = 10
			}
			if utf8.RuneCountInString(title) > maxTitle {
				title = string([]rune(title)[:maxTitle-1]) + "…"
			}

			isCurrent := c.ID == m.conv.ID
			isSelected := i == m.pickerCursor

			dot := "  "
			if isCurrent {
				dot = "● "
			}
			line := fmt.Sprintf("%s%-*s  %s", dot, maxTitle, title, age)

			if isSelected {
				sb.WriteString(pickerSelectedStyle.Render(line) + "\n")
			} else {
				sb.WriteString(pickerRowStyle.Render(line) + "\n")
			}
		}
		if len(filtered) > maxVisible {
			sb.WriteString(dimStyle.Render(fmt.Sprintf("  … %d more", len(filtered)-maxVisible)) + "\n")
		}
	}

	sb.WriteString("\n")
	sb.WriteString(dimStyle.Render("  ↑↓ navigate   enter select   ctrl+n new   esc cancel"))
	return sb.String()
}

// ── Search view ───────────────────────────────────────────────────────────────

func (m Model) searchView() string {
	filtered := m.filteredSearch()
	maxVisible := m.height - 9
	if maxVisible < 1 {
		maxVisible = 1
	}

	start := 0
	if m.searchCursor >= maxVisible {
		start = m.searchCursor - maxVisible + 1
	}
	end := start + maxVisible
	if end > len(filtered) {
		end = len(filtered)
	}

	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(pickerTitleStyle.Render("  search messages") + "\n\n")

	filterText := m.searchInput
	if filterText == "" {
		filterText = dimStyle.Render("type to search...")
	}
	sb.WriteString(promptStyle.Render("  > ") + filterText + "\n")
	sb.WriteString(sepStyle.Render(strings.Repeat("─", m.width)) + "\n")

	if len(m.searchAll) == 0 {
		sb.WriteString(dimStyle.Render("  no messages yet") + "\n")
	} else if len(filtered) == 0 {
		sb.WriteString(dimStyle.Render("  no matches") + "\n")
	} else {
		for i := start; i < end; i++ {
			r := filtered[i]
			role := r.msg.Role
			convTitle := r.conv.Title
			maxTitle := 20
			if utf8.RuneCountInString(convTitle) > maxTitle {
				convTitle = string([]rune(convTitle)[:maxTitle-1]) + "…"
			}

			// Preview: first line, truncated
			preview := r.msg.Content
			if nl := strings.Index(preview, "\n"); nl >= 0 {
				preview = preview[:nl]
			}
			maxPreview := m.width - maxTitle - 22
			if maxPreview < 20 {
				maxPreview = 20
			}
			if utf8.RuneCountInString(preview) > maxPreview {
				preview = string([]rune(preview)[:maxPreview-1]) + "…"
			}

			age := dimStyle.Render(timeAgo(r.msg.CreatedAt))
			roleTag := fmt.Sprintf("[%-4s]", role)
			line := fmt.Sprintf("  %s  %-*s  %s  %s", roleTag, maxTitle, convTitle, preview, age)

			if i == m.searchCursor {
				sb.WriteString(pickerSelectedStyle.Render(line) + "\n")
			} else {
				sb.WriteString(pickerRowStyle.Render(line) + "\n")
			}
		}
		if len(filtered) > maxVisible {
			sb.WriteString(dimStyle.Render(fmt.Sprintf("  … %d more", len(filtered)-maxVisible)) + "\n")
		}
	}

	sb.WriteString("\n")
	sb.WriteString(dimStyle.Render("  ↑↓ navigate   enter jump to conv   esc cancel"))
	return sb.String()
}

// ── Content rendering ─────────────────────────────────────────────────────────

func (m *Model) refreshContent() {
	if !m.ready {
		return
	}
	var sb strings.Builder
	sb.WriteString("\n") // top padding
	for _, msg := range m.messages {
		sb.WriteString(m.renderMsg(msg))
		sb.WriteString("\n\n")
	}
	if m.streaming && m.streamBuf.Len() > 0 {
		sb.WriteString(wordWrap(m.streamBuf.String(), m.viewport.Width))
		sb.WriteString(" ▊")
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
		wrapped := wordWrap(msg.Content, w-2)
		lines := strings.Split(wrapped, "\n")
		for i, line := range lines {
			if i == 0 {
				lines[i] = userStyle.Render("> ") + line
			} else {
				lines[i] = "  " + line
			}
		}
		return strings.Join(lines, "\n")

	case "summary":
		// Compaction summary — render as dim block
		lines := strings.Split(wordWrap(msg.Content, w), "\n")
		for i, l := range lines {
			lines[i] = dimStyle.Render(l)
		}
		return strings.Join(lines, "\n")

	case "system":
		// Display-only annotations (:help, :debug output, etc.)
		lines := strings.Split(wordWrap(msg.Content, w), "\n")
		for i, l := range lines {
			lines[i] = dimStyle.Render(l)
		}
		return strings.Join(lines, "\n")

	default: // assistant
		return m.renderMarkdown(msg.Content, w)
	}
}

// renderMarkdown renders content with glamour; falls back to plain wrap on error.
func (m Model) renderMarkdown(content string, width int) string {
	r, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return wordWrap(content, width)
	}
	out, err := r.Render(content)
	if err != nil {
		return wordWrap(content, width)
	}
	// Glamour adds a trailing newline; trim it since we add our own separator
	return strings.TrimRight(out, "\n")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (m Model) viewportHeight() int {
	h := m.height - 3 // history + sep + input + status = 4 rows; viewport gets the rest
	if h < 1 {
		h = 1
	}
	return h
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

var imageExts = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".svg": "image/svg+xml",
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
