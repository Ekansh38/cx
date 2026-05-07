package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"cx/config"
	"cx/llm"
	"cx/store"
)

// ── states ──────────────────────────────────────────────────────────────────

type appState int

const (
	stateChat   appState = iota
	statePicker          // conversation picker overlay
)

// ── messages (tea.Msg types) ─────────────────────────────────────────────────

type tokenMsg string         // one streaming token
type streamEndMsg string     // full content when stream finishes (empty = cancelled/err)
type titleUpdatedMsg string  // auto-generated title
type editorDoneMsg string    // content returned from $EDITOR

// ── commands (available in : mode) ───────────────────────────────────────────

var commands = []string{
	":clear", ":debug", ":grep", ":help",
	":list", ":model ", ":models", ":new",
	":q", ":quit", ":rename ",
}

// ── Model ────────────────────────────────────────────────────────────────────

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
	viewport viewport.Model
	input    textinput.Model
	atBottom bool // whether viewport was at bottom before last update

	// streaming
	streaming  bool
	streamCh   <-chan string
	cancelStream context.CancelFunc
	streamBuf  strings.Builder

	// picker view
	state        appState
	pickerConvs  []*store.Conversation
	pickerFilter string
	pickerCursor int

	// misc
	systemPrompt string
	errMsg       string
	statusExtra  string // shown in status bar during special states
}

// New creates the initial bubbletea model.
func New(cfg *config.Config, st *store.Store, conv *store.Conversation, msgs []*store.Message, prov llm.Provider, modelName, sysPrompt string) Model {
	ti := textinput.New()
	ti.Prompt = ""   // we render our own "> " prefix
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
	}
}

func (m Model) Init() tea.Cmd {
	return textinput.Blink
}

// ── Update ───────────────────────────────────────────────────────────────────

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if !m.ready {
			m.viewport = viewport.New(msg.Width, m.viewportHeight())
			m.ready = true
			m.refreshContent()
			m.viewport.GotoBottom()
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = m.viewportHeight()
		}
		m.input.Width = msg.Width - 3
		return m, nil

	case tokenMsg:
		m.streamBuf.WriteString(string(msg))
		m.refreshContent()
		if m.atBottom {
			m.viewport.GotoBottom()
		}
		return m, listenToken(m.streamCh)

	case streamEndMsg:
		content := string(msg)
		m.streaming = false
		m.cancelStream = nil
		m.streamCh = nil
		m.statusExtra = ""

		if content != "" {
			m.store.AddMessage(m.conv.ID, "assistant", content)
			newMsg := &store.Message{Role: "assistant", Content: content}
			m.messages = append(m.messages, newMsg)
		}
		m.streamBuf.Reset()
		m.refreshContent()
		m.viewport.GotoBottom()
		m.atBottom = true

		// Auto-title after first exchange if still "Untitled"
		if m.conv.Title == "Untitled" && len(m.messages) >= 2 {
			return m, m.autoTitleCmd()
		}
		return m, nil

	case titleUpdatedMsg:
		m.conv.Title = string(msg)
		return m, nil

	case editorDoneMsg:
		m.input.SetValue(string(msg))
		m.input.CursorEnd()
		return m, nil

	case tea.KeyMsg:
		if m.state == statePicker {
			return m.updatePicker(msg)
		}
		return m.updateChat(msg)
	}

	return m, nil
}

// ── Chat key handling ─────────────────────────────────────────────────────────

func (m Model) updateChat(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.Type {

	case tea.KeyCtrlC:
		if m.streaming {
			m.cancelStream()
			return m, nil
		}
		return m, tea.Quit

	case tea.KeyEnter:
		if m.streaming {
			return m, nil
		}
		input := strings.TrimSpace(m.input.Value())
		if input == "" {
			return m, nil
		}
		m.input.SetValue("")
		m.errMsg = ""
		return m.handleInput(input)

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
		return m.grepMode()

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

	// Pass everything else to the textinput
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// handleInput processes a submitted message (plain text or :command).
func (m Model) handleInput(input string) (Model, tea.Cmd) {
	if strings.HasPrefix(input, ":") {
		return m.handleCommand(input)
	}

	// Regular message — send to model
	if m.provider == nil {
		m.errMsg = "no provider configured for model " + m.model
		return m, nil
	}

	// Persist user message
	m.store.AddMessage(m.conv.ID, "user", input)
	m.messages = append(m.messages, &store.Message{Role: "user", Content: input})
	m.refreshContent()
	m.viewport.GotoBottom()
	m.atBottom = true

	return m.startStream()
}

// handleCommand processes ":command" inputs.
func (m Model) handleCommand(input string) (Model, tea.Cmd) {
	cmd := strings.TrimSpace(input)
	parts := strings.SplitN(cmd, " ", 2)

	switch parts[0] {
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
		return m.grepMode()

	case ":clear":
		m.messages = nil
		m.refreshContent()
		return m, nil

	case ":debug":
		m.appendSystemLine("system prompt:\n\n" + m.systemPrompt)
		return m, nil

	case ":models":
		m.appendSystemLine("configured providers:\n  gemini  (GEMINI_API_KEY)\n  openai  (OPENAI_API_KEY)\n  ollama  (default)")
		return m, nil

	case ":rename":
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			m.errMsg = "usage: :rename <title>"
			return m, nil
		}
		title := strings.TrimSpace(parts[1])
		m.store.UpdateTitle(m.conv.ID, title)
		m.conv.Title = title
		return m, nil

	case ":model":
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			m.appendSystemLine("current model: " + m.model)
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
		return m, nil

	case ":help":
		m.appendSystemLine(helpText)
		return m, nil

	default:
		m.errMsg = "unknown command: " + parts[0] + "  (type :help)"
		return m, nil
	}
}

// appendSystemLine adds a synthetic "system" line into the viewport display only.
func (m *Model) appendSystemLine(text string) {
	m.messages = append(m.messages, &store.Message{Role: "system", Content: text})
	m.refreshContent()
	m.viewport.GotoBottom()
	m.atBottom = true
}

const helpText = `keybindings
  ctrl+c     cancel stream / quit
  ctrl+l     conversation picker
  ctrl+n     new conversation
  ctrl+e     open $EDITOR for long messages
  ctrl+g     grep all messages
  ctrl+u     scroll up half page
  ctrl+d     scroll down half page
  ↑ ↓        scroll one line
  tab        autocomplete command

commands  (type : to see completions)
  :q / :quit           quit
  :new                 new conversation
  :list                conversation picker
  :grep                search messages
  :rename <title>      rename this conversation
  :model <name>        switch model (e.g. gemini-2.0-flash, gpt-4o)
  :models              list providers
  :clear               clear view (history kept)
  :debug               show system prompt
  :help                this help`

// ── Streaming ─────────────────────────────────────────────────────────────────

func (m Model) startStream() (Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan string, 128)

	m.streaming = true
	m.cancelStream = cancel
	m.streamCh = ch
	m.statusExtra = "···"
	m.streamBuf.Reset()

	msgs := m.buildLLMMessages()

	return m, tea.Batch(
		runStream(ctx, m.provider, m.model, msgs, ch),
		listenToken(ch),
	)
}

// runStream starts the LLM call in a goroutine; sends streamEndMsg when done.
func runStream(ctx context.Context, prov llm.Provider, model string, msgs []llm.Message, ch chan<- string) tea.Cmd {
	return func() tea.Msg {
		content, _ := prov.Stream(ctx, model, msgs, func(token string) {
			select {
			case ch <- token:
			case <-ctx.Done():
			}
		})
		close(ch)
		return streamEndMsg(content)
	}
}

// listenToken returns a Cmd that waits for one token from ch.
func listenToken(ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		t, ok := <-ch
		if !ok {
			return nil // streamEndMsg already sent by runStream
		}
		return tokenMsg(t)
	}
}

func (m Model) buildLLMMessages() []llm.Message {
	out := []llm.Message{{Role: "system", Content: m.systemPrompt}}
	for _, msg := range m.messages {
		if msg.Role == "system" {
			continue // skip display-only system messages
		}
		out = append(out, llm.Message{Role: msg.Role, Content: msg.Content})
	}
	return out
}

// ── Auto-title ────────────────────────────────────────────────────────────────

func (m Model) autoTitleCmd() tea.Cmd {
	msgs := m.buildLLMMessages()
	prov := m.provider
	model := m.model
	st := m.store
	convID := m.conv.ID

	return func() tea.Msg {
		titlePrompt := append(msgs, llm.Message{
			Role:    "user",
			Content: "Give this conversation a short title (4 words max, no quotes, no punctuation). Reply with only the title.",
		})
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		ch := make(chan string, 64)
		var sb strings.Builder
		done := make(chan struct{})
		go func() {
			prov.Stream(ctx, model, titlePrompt, func(t string) {
				select {
				case ch <- t:
				case <-ctx.Done():
				}
			})
			close(ch)
			close(done)
		}()
		for t := range ch {
			sb.WriteString(t)
		}
		title := strings.TrimSpace(sb.String())
		if title == "" {
			return nil
		}
		// Take only the first line, cap at 60 chars
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
		selected := filtered[m.pickerCursor]
		if selected.ID == m.conv.ID {
			m.state = stateChat
			return m, nil
		}
		return m.switchConversation(selected.ID)

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
			// Remove last rune
			_, size := utf8.DecodeLastRuneInString(m.pickerFilter)
			m.pickerFilter = m.pickerFilter[:len(m.pickerFilter)-size]
			m.pickerCursor = 0
		}
		return m, nil

	default:
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 0 {
			m.pickerFilter += string(msg.Runes)
			m.pickerCursor = 0
		}
		return m, nil
	}
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
		// non-fatal: keep old provider
		prov = m.provider
	}

	m.conv = conv
	m.messages = msgs
	m.model = conv.Model
	m.provider = prov
	m.streaming = false
	m.streamBuf.Reset()
	m.state = stateChat

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
	m.state = stateChat

	m.refreshContent()
	m.viewport.GotoBottom()
	m.atBottom = true
	return m, nil
}

// ── Grep (shell out to fzf) ───────────────────────────────────────────────────

func (m Model) grepMode() (Model, tea.Cmd) {
	convs, _ := m.store.ListConversations()
	msgs, _ := m.store.SearchMessages("")

	// Build fzf input: "convID\tdisplay line"
	var sb strings.Builder
	convMap := make(map[int64]*store.Conversation)
	for _, c := range convs {
		convMap[c.ID] = c
	}
	for _, msg := range msgs {
		title := "?"
		if c, ok := convMap[msg.ConvID]; ok {
			title = c.Title
		}
		preview := msg.Content
		if len(preview) > 80 {
			preview = preview[:80]
		}
		preview = strings.ReplaceAll(preview, "\n", " ")
		fmt.Fprintf(&sb, "%d\t[%s]  %-20s  %s\n", msg.ConvID, msg.Role, title, preview)
	}

	input := sb.String()
	if input == "" {
		m.errMsg = "no messages to search"
		return m, nil
	}

	fzfCmd := exec.Command("fzf", "--with-nth=2..", "--delimiter=\t", "--reverse", "--height=50%")
	fzfCmd.Stdin = strings.NewReader(input)

	return m, tea.ExecProcess(fzfCmd, func(err error) tea.Msg {
		if err != nil {
			return nil // user escaped fzf
		}
		return nil // handled differently below — see note
	})
	// NOTE: fzf output goes to its own stdout — we can't capture it here because
	// tea.ExecProcess connects fzf's stdout to the terminal. For now grep just
	// lets users view messages; switching conv via grep is a future improvement.
}

// ── Editor (Ctrl+E) ───────────────────────────────────────────────────────────

func (m Model) openEditor() (Model, tea.Cmd) {
	current := m.input.Value()

	tmp, err := os.CreateTemp("", "cx-*.md")
	if err != nil {
		m.errMsg = "could not create temp file"
		return m, nil
	}
	tmpName := tmp.Name()
	tmp.WriteString(current)
	tmp.Close()

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vim"
	}

	editorCmd := exec.Command(editor, tmpName)
	return m, tea.ExecProcess(editorCmd, func(err error) tea.Msg {
		defer os.Remove(tmpName)
		if err != nil {
			return nil
		}
		data, readErr := os.ReadFile(tmpName)
		if readErr != nil {
			return nil
		}
		content := strings.TrimRight(string(data), "\r\n")
		return editorDoneMsg(content)
	})
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m Model) View() string {
	if !m.ready {
		return "\n  loading..."
	}

	if m.state == statePicker {
		return m.pickerView()
	}
	return m.chatView()
}

func (m Model) chatView() string {
	sep := m.sepView()
	input := m.inputView()
	status := m.statusView()

	return lipgloss.JoinVertical(
		lipgloss.Left,
		m.viewport.View(),
		sep,
		input,
		status,
	)
}

func (m Model) sepView() string {
	inputVal := m.input.Value()
	if strings.HasPrefix(inputVal, ":") {
		matches := completionsFor(inputVal)
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
	return promptStyle.Render("> ") + m.input.View()
}

func (m Model) statusView() string {
	extra := m.statusExtra
	if m.streaming && extra == "" {
		extra = "···"
	}

	titlePart := m.conv.Title
	if extra != "" {
		titlePart = extra
	}

	left := fmt.Sprintf("  %s  ·  #%d  ·  %s", m.model, m.conv.ID, titlePart)
	right := time.Now().Format("15:04") + "  "

	pad := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 1 {
		pad = 1
	}
	return statusStyle.Render(left + strings.Repeat(" ", pad) + right)
}

// ── Picker view ───────────────────────────────────────────────────────────────

func (m Model) pickerView() string {
	filtered := m.filteredConvs()

	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(pickerTitleStyle.Render("  conversations") + "\n\n")

	// Filter input
	filterDisplay := m.pickerFilter
	if filterDisplay == "" {
		filterDisplay = dimStyle.Render("type to filter...")
	}
	sb.WriteString(promptStyle.Render("  > ") + filterDisplay + "\n")
	sb.WriteString(sepStyle.Render(strings.Repeat("─", m.width)) + "\n")

	if len(filtered) == 0 {
		sb.WriteString(dimStyle.Render("  no matches\n"))
	} else {
		for i, c := range filtered {
			age := timeAgo(c.UpdatedAt)
			title := c.Title
			if len(title) > m.width-20 {
				title = title[:m.width-23] + "..."
			}

			prefix := "  "
			if c.ID == m.conv.ID {
				prefix = "● "
			}

			line := fmt.Sprintf("%s%-*s  %s", prefix, m.width-20, title, age)

			if i == m.pickerCursor {
				sb.WriteString(pickerSelectedStyle.Render(line) + "\n")
			} else {
				sb.WriteString(pickerRowStyle.Render(line) + "\n")
			}
		}
	}

	sb.WriteString("\n")
	sb.WriteString(dimStyle.Render("  ↑↓ navigate   enter select   ctrl+n new   esc cancel"))

	return sb.String()
}

// ── Content rendering ─────────────────────────────────────────────────────────

// refreshContent re-renders all messages into the viewport.
func (m *Model) refreshContent() {
	if !m.ready {
		return
	}
	var sb strings.Builder
	for _, msg := range m.messages {
		sb.WriteString(m.renderMsg(msg))
		sb.WriteString("\n\n")
	}
	if m.streaming && m.streamBuf.Len() > 0 {
		sb.WriteString(wordWrap(m.streamBuf.String(), m.viewport.Width))
		sb.WriteString("▊")
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

	case "system":
		// Display-only system annotations (e.g. :help output)
		return dimStyle.Render(wordWrap(msg.Content, w))

	default: // assistant
		return wordWrap(msg.Content, w)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (m Model) viewportHeight() int {
	h := m.height - 3 // viewport + sep + input + status
	if h < 1 {
		h = 1
	}
	return h
}

// wordWrap wraps text at width characters, preserving existing newlines.
func wordWrap(text string, width int) string {
	if width <= 0 {
		return text
	}
	var result strings.Builder
	for i, paragraph := range strings.Split(text, "\n") {
		if i > 0 {
			result.WriteByte('\n')
		}
		result.WriteString(wrapParagraph(paragraph, width))
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
		if curLen == 0 {
			cur.WriteString(w)
			curLen = wLen
		} else if curLen+1+wLen <= width {
			cur.WriteByte(' ')
			cur.WriteString(w)
			curLen += 1 + wLen
		} else {
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

// completionsFor returns commands that match the given prefix.
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

// timeAgo returns a human-readable relative time.
func timeAgo(unix int64) string {
	d := time.Since(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return "just now"
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
