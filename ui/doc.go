package ui

// Doc-chat mode: attach a document to a conversation and discuss/edit it.
// The doc renders in a left pane with line numbers; chat lives on the right.
// The full document is sent as a system message on every turn, and the model
// can propose edits as SEARCH/REPLACE blocks the user approves one by one.

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"cx/config"
	"cx/store"
)

type docReloadMsg struct{}

// docEdit is one model-proposed SEARCH/REPLACE change awaiting review.
type docEdit struct {
	search  string
	replace string
	applied bool
	failed  bool
}

// ── LLM-facing document context ──────────────────────────────────────────────

const docEditInstructions = `You can discuss the document and, when the user asks for changes, propose edits using this exact format:

<edit>
<<<<<<< SEARCH
exact text copied verbatim from the document (do NOT include the line numbers)
=======
the replacement text
>>>>>>> REPLACE
</edit>

Rules:
- The SEARCH text must match the document exactly, character for character.
- Keep each edit small and focused; use multiple <edit> blocks for separate changes.
- The user reviews and approves each edit before it is applied, so don't repeat the diff in prose.
- The user may reference locations like @L12, @L12-30, or @## Heading Name.`

// docContextMsg builds the system message carrying the live document.
func docContextMsg(path, content string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "The user has attached a document to this conversation: %s\n\n", path)
	sb.WriteString("Current document content (line numbers added for reference — they are NOT part of the file):\n<document>\n")
	for i, ln := range strings.Split(content, "\n") {
		fmt.Fprintf(&sb, "%d│%s\n", i+1, ln)
	}
	sb.WriteString("</document>\n\n")
	sb.WriteString(docEditInstructions)
	return sb.String()
}

// ── Attach / detach / reload ─────────────────────────────────────────────────

func (m Model) attachDoc(path string) (Model, tea.Cmd) {
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = home + path[1:]
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		m.errMsg = "invalid path: " + err.Error()
		return m, nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		m.errMsg = "doc: " + err.Error()
		return m, nil
	}
	if len(data) > 512*1024 {
		m.errMsg = "doc too large (>512KB)"
		return m, nil
	}
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		m.errMsg = "doc looks binary — attach a text file"
		return m, nil
	}

	m.docPath = abs
	m.docContent = string(data)
	m.docFocus = false
	m.docReview = false
	m.docEdits = nil
	m.store.UpdateDocPath(m.conv.ID, abs)
	m.conv.DocPath = abs

	m.applyLayout()
	m.refreshDocPane()
	m.refreshContent()
	m.injectSystemLine("attached " + filepath.Base(abs) + " — ctrl+o to focus the doc pane, :doc off to close")
	return m, nil
}

func (m Model) detachDoc() (Model, tea.Cmd) {
	if m.docPath == "" {
		m.errMsg = "no document attached"
		return m, nil
	}
	name := filepath.Base(m.docPath)
	m.docPath = ""
	m.docContent = ""
	m.docFocus = false
	m.docReview = false
	m.docEdits = nil
	m.store.UpdateDocPath(m.conv.ID, "")
	m.conv.DocPath = ""

	m.applyLayout()
	m.refreshContent()
	m.injectSystemLine("closed " + name)
	return m, nil
}

// reloadDoc re-reads the attached document from disk.
func (m *Model) reloadDoc() {
	if m.docPath == "" {
		return
	}
	data, err := os.ReadFile(m.docPath)
	if err != nil {
		m.errMsg = "doc reload: " + err.Error()
		return
	}
	m.docContent = string(data)
	m.refreshDocPane()
}

// ── Layout ───────────────────────────────────────────────────────────────────

// chatWidth is the chat viewport width — full width, or right half in doc mode.
func (m Model) chatWidth() int {
	if m.docPath == "" {
		return m.width
	}
	return m.width - m.docWidth() - 1 // 1 col divider
}

func (m Model) docWidth() int {
	return m.width / 2
}

// applyLayout sizes the chat viewport, doc pane, and input for the current mode.
func (m *Model) applyLayout() {
	if !m.ready {
		return
	}
	m.viewport.Width = m.chatWidth()
	m.viewport.Height = m.viewportHeight()
	m.input.SetWidth(m.width - 4)
	if m.docPath != "" {
		m.docVP.Width = m.docWidth()
		m.docVP.Height = m.viewportHeight() - 1 // header row
		m.refreshDocPane()
	}
}

// refreshDocPane re-renders the numbered, wrapped document into its viewport.
func (m *Model) refreshDocPane() {
	w := m.docVP.Width
	if w < 8 {
		return
	}
	lines := strings.Split(m.docContent, "\n")
	numW := len(fmt.Sprint(len(lines)))
	avail := w - numW - 1
	if avail < 4 {
		avail = 4
	}
	var sb strings.Builder
	for i, ln := range lines {
		for j, wl := range strings.Split(hardWrap(ln, avail), "\n") {
			if j == 0 {
				sb.WriteString(dimStyle.Render(fmt.Sprintf("%*d ", numW, i+1)))
			} else {
				sb.WriteString(strings.Repeat(" ", numW+1))
			}
			sb.WriteString(wl)
			sb.WriteByte('\n')
		}
	}
	m.docVP.SetContent(strings.TrimRight(sb.String(), "\n"))
}

// hardWrap word-wraps then force-breaks any line still longer than width
// (long URLs etc.) so nothing bleeds into the chat pane.
func hardWrap(text string, width int) string {
	wrapped := wordWrap(text, width)
	var out []string
	for _, ln := range strings.Split(wrapped, "\n") {
		for utf8.RuneCountInString(ln) > width {
			r := []rune(ln)
			out = append(out, string(r[:width]))
			ln = string(r[width:])
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// docPaneView renders header + document viewport (left pane).
func (m Model) docPaneView() string {
	name := filepath.Base(m.docPath)
	hint := "  ctrl+o focus"
	if m.docFocus {
		hint = "  j/k scroll · e edit · r reload · esc back"
	}
	header := name + dimStyle.Render(hint)
	maxW := m.docWidth()
	if lipgloss.Width(header) > maxW {
		header = name
		if utf8.RuneCountInString(header) > maxW {
			header = string([]rune(header)[:maxW-1]) + "…"
		}
	}
	style := dimStyle
	if m.docFocus {
		style = docHeaderStyle
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		style.Render(padRight(header, maxW)),
		m.docVP.View(),
	)
}

func padRight(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// docDivider is the vertical line between the doc and chat panes.
func (m Model) docDivider() string {
	h := m.viewportHeight()
	col := make([]string, h)
	for i := range col {
		col[i] = sepStyle.Render("│")
	}
	return strings.Join(col, "\n")
}

// ── Doc-pane key handling (when focused) ─────────────────────────────────────

func (m Model) updateDocFocus(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "j", "down":
		m.docVP.LineDown(1)
	case "k", "up":
		m.docVP.LineUp(1)
	case "d", "ctrl+d", "pgdown":
		m.docVP.HalfViewDown()
	case "u", "ctrl+u", "pgup":
		m.docVP.HalfViewUp()
	case "g":
		m.docVP.GotoTop()
	case "G":
		m.docVP.GotoBottom()
	case "r":
		m.reloadDoc()
	case "e":
		return m.openDocEditor()
	case "esc", "tab", "ctrl+o":
		m.docFocus = false
	case "ctrl+c":
		return m, tea.Quit
	}
	return m, nil
}

// openDocEditor opens the attached document in $EDITOR and reloads on exit.
func (m Model) openDocEditor() (Model, tea.Cmd) {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vim"
	}
	return m, tea.ExecProcess(exec.Command(editor, m.docPath), func(err error) tea.Msg {
		return docReloadMsg{}
	})
}

// ── Edit blocks: parse / apply / review ──────────────────────────────────────

// parseDocEdits extracts SEARCH/REPLACE blocks from an assistant response.
func parseDocEdits(s string) []docEdit {
	var edits []docEdit
	for {
		start := strings.Index(s, "<edit>")
		if start < 0 {
			break
		}
		rel := strings.Index(s[start:], "</edit>")
		if rel < 0 {
			break
		}
		block := s[start+len("<edit>") : start+rel]
		s = s[start+rel+len("</edit>"):]
		if e, ok := parseEditBlock(block); ok {
			edits = append(edits, e)
		}
	}
	return edits
}

func parseEditBlock(b string) (docEdit, bool) {
	var search, replace []string
	mode := 0
	for _, ln := range strings.Split(b, "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(t, "<<<<<<<"):
			mode = 1
		case t == "=======" && mode == 1:
			mode = 2
		case strings.HasPrefix(t, ">>>>>>>") && mode == 2:
			mode = 3
		default:
			switch mode {
			case 1:
				search = append(search, ln)
			case 2:
				replace = append(replace, ln)
			}
		}
	}
	if mode != 3 || len(search) == 0 {
		return docEdit{}, false
	}
	return docEdit{
		search:  strings.Join(search, "\n"),
		replace: strings.Join(replace, "\n"),
	}, true
}

// applyEditTo replaces the first occurrence of search in content.
// Falls back to matching with trailing whitespace stripped per line.
func applyEditTo(content, search, replace string) (string, bool) {
	if strings.Contains(content, search) {
		return strings.Replace(content, search, replace, 1), true
	}
	norm := func(s string) string {
		ls := strings.Split(s, "\n")
		for i := range ls {
			ls[i] = strings.TrimRight(ls[i], " \t\r")
		}
		return strings.Join(ls, "\n")
	}
	nc, ns := norm(content), norm(search)
	if strings.Contains(nc, ns) {
		// Applying on normalized content strips trailing whitespace file-wide;
		// acceptable trade-off for still landing the edit.
		return strings.Replace(nc, ns, replace, 1), true
	}
	return content, false
}

// applyDocEdit applies edit i to the document and writes it to disk.
func (m *Model) applyDocEdit(i int) bool {
	e := &m.docEdits[i]
	newContent, ok := applyEditTo(m.docContent, e.search, e.replace)
	if !ok {
		e.failed = true
		return false
	}
	if err := os.WriteFile(m.docPath, []byte(newContent), 0o644); err != nil {
		m.errMsg = "doc write: " + err.Error()
		e.failed = true
		return false
	}
	m.docContent = newContent
	e.applied = true
	m.refreshDocPane()
	return true
}

// showDocEdit injects the current pending edit as a colored diff into the chat.
func (m *Model) showDocEdit() {
	e := m.docEdits[m.docEditIdx]
	maxW := m.chatWidth() - 6
	if maxW < 20 {
		maxW = 20
	}
	var sb strings.Builder
	sb.WriteString(dimStyle.Render(fmt.Sprintf("── proposed edit %d/%d ──", m.docEditIdx+1, len(m.docEdits))) + "\n")
	for _, l := range strings.Split(e.search, "\n") {
		sb.WriteString(diffOldStyle.Render(truncate("  - "+l, maxW)) + "\n")
	}
	for _, l := range strings.Split(e.replace, "\n") {
		sb.WriteString(diffNewStyle.Render(truncate("  + "+l, maxW)) + "\n")
	}
	sb.WriteString(dimStyle.Render("  [y] apply   [n] skip   [a] apply all   [esc] cancel"))
	m.messages = append(m.messages, &store.Message{Role: "diff", Content: sb.String()})
	m.refreshContent()
	m.viewport.GotoBottom()
	m.atBottom = true
}

func truncate(s string, w int) string {
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	return string([]rune(s)[:w-1]) + "…"
}

// updateDocReview handles y/n/a/esc while edits are pending approval.
func (m Model) updateDocReview(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "y":
		if !m.applyDocEdit(m.docEditIdx) {
			m.injectSystemLine("couldn't locate that text in the document — skipped (doc may have changed)")
		}
		m.advanceDocReview()
	case "n":
		m.advanceDocReview()
	case "a":
		for i := m.docEditIdx; i < len(m.docEdits); i++ {
			m.applyDocEdit(i)
		}
		m.docEditIdx = len(m.docEdits)
		m.advanceDocReview()
	case "esc", "q", "ctrl+c":
		m.docReview = false
		m.injectSystemLine("edit review cancelled — remaining edits discarded")
	}
	return m, nil
}

func (m *Model) advanceDocReview() {
	m.docEditIdx++
	if m.docEditIdx < len(m.docEdits) {
		m.showDocEdit()
		return
	}
	m.docReview = false
	applied, failed := 0, 0
	for _, e := range m.docEdits {
		if e.applied {
			applied++
		}
		if e.failed {
			failed++
		}
	}
	note := fmt.Sprintf("applied %d/%d edit(s) to %s", applied, len(m.docEdits), filepath.Base(m.docPath))
	if failed > 0 {
		note += fmt.Sprintf(" — %d couldn't be located", failed)
	}
	m.injectSystemLine(note)
}

// stripEditBlocks replaces raw <edit> blocks with a compact placeholder for display.
func stripEditBlocks(s string) string {
	n := 0
	for {
		start := strings.Index(s, "<edit>")
		if start < 0 {
			break
		}
		rel := strings.Index(s[start:], "</edit>")
		if rel < 0 {
			break
		}
		n++
		s = s[:start] + fmt.Sprintf("*[proposed edit %d — review below]*", n) + s[start+rel+len("</edit>"):]
	}
	return s
}

// ── Editor selection bridge ──────────────────────────────────────────────────
//
// A keybinding in the user's editor (see README) writes the visual selection
// to a handoff file. cx picks it up on the next message: auto-attaches the
// doc if needed, and passes the highlighted passage to the model.
//
// File format:  line 1 = absolute file path, line 2 = "start-end", rest = text.

type docSelection struct {
	file  string
	start int
	end   int
	text  string
}

func selectionPath() string {
	return filepath.Join(config.DataDir(), "selection.txt")
}

// readSelection parses the handoff file without consuming it (nil if absent/invalid).
func readSelection() *docSelection {
	data, err := os.ReadFile(selectionPath())
	if err != nil {
		return nil
	}
	return parseSelectionText(string(data))
}

func parseSelectionText(raw string) *docSelection {
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	if len(lines) < 3 {
		return nil
	}
	file := strings.TrimSpace(lines[0])
	var start, end int
	if _, err := fmt.Sscanf(strings.TrimSpace(lines[1]), "%d-%d", &start, &end); err != nil {
		return nil
	}
	if file == "" || start < 1 || end < start {
		return nil
	}
	return &docSelection{file: file, start: start, end: end, text: strings.Join(lines[2:], "\n")}
}

// consumeSelection removes the handoff file after the selection has been used.
func consumeSelection() {
	os.Remove(selectionPath())
}

// selectionContextMsg builds the one-turn system message carrying the highlight.
func selectionContextMsg(sel *docSelection) string {
	return fmt.Sprintf(
		"The user is currently highlighting lines %d-%d of %s in their editor:\n<selection>\n%s\n</selection>\nTheir next message refers to this passage.",
		sel.start, sel.end, sel.file, sel.text,
	)
}

// ── Doc file picker (:doc with no args) ──────────────────────────────────────

var docSkipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "target": true,
	"dist": true, "build": true, ".git": true,
}

// listDocFiles finds markdown/text files under the current directory.
func listDocFiles() []string {
	root, err := os.Getwd()
	if err != nil {
		return nil
	}
	var out []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if p != root && (strings.HasPrefix(name, ".") || docSkipDirs[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".md", ".markdown", ".txt":
			if rel, err := filepath.Rel(root, p); err == nil {
				out = append(out, rel)
			}
		}
		if len(out) >= 500 {
			return fs.SkipAll
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func (m Model) enterDocPicker() (Model, tea.Cmd) {
	files := listDocFiles()
	if len(files) == 0 {
		m.errMsg = "no .md/.txt files found under " + cwdBase() + " — use :doc <path>"
		return m, nil
	}
	m.state = stateDocPicker
	m.docFiles = files
	m.docFilter = ""
	m.docCursor = 0
	return m, nil
}

func cwdBase() string {
	if wd, err := os.Getwd(); err == nil {
		return filepath.Base(wd)
	}
	return "cwd"
}

func (m Model) updateDocPicker(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.state = stateChat
		return m, nil

	case tea.KeyEnter:
		filtered := m.filteredDocFiles()
		if len(filtered) == 0 {
			m.state = stateChat
			return m, nil
		}
		m.state = stateChat
		return m.attachDoc(filtered[m.docCursor])

	case tea.KeyUp:
		if m.docCursor > 0 {
			m.docCursor--
		}
		return m, nil

	case tea.KeyDown:
		if m.docCursor < len(m.filteredDocFiles())-1 {
			m.docCursor++
		}
		return m, nil

	case tea.KeyBackspace, tea.KeyDelete:
		if len(m.docFilter) > 0 {
			_, size := utf8.DecodeLastRuneInString(m.docFilter)
			m.docFilter = m.docFilter[:len(m.docFilter)-size]
			m.docCursor = 0
		}
		return m, nil

	case tea.KeySpace:
		m.docFilter += " "
		m.docCursor = 0
		return m, nil

	case tea.KeyRunes:
		m.docFilter += string(msg.Runes)
		m.docCursor = 0
		return m, nil
	}
	return m, nil
}

func (m Model) filteredDocFiles() []string {
	if m.docFilter == "" {
		return m.docFiles
	}
	q := strings.ToLower(m.docFilter)
	var out []string
	for _, f := range m.docFiles {
		if strings.Contains(strings.ToLower(f), q) {
			out = append(out, f)
		}
	}
	return out
}

func (m Model) docPickerView() string {
	filtered := m.filteredDocFiles()
	maxVisible := m.height - 10
	if maxVisible < 1 {
		maxVisible = 1
	}
	start := 0
	if m.docCursor >= maxVisible {
		start = m.docCursor - maxVisible + 1
	}
	end := start + maxVisible
	if end > len(filtered) {
		end = len(filtered)
	}

	var sb strings.Builder
	sb.WriteString("\n\n")
	sb.WriteString(pickerTitleStyle.Render("   Documents  ") + dimStyle.Render("("+cwdBase()+")") + "\n\n")

	filterText := m.docFilter
	if filterText == "" {
		filterText = dimStyle.Render("type to filter...")
	}
	sb.WriteString(promptStyle.Render("   > ") + filterText + "\n\n")

	if len(filtered) == 0 {
		sb.WriteString(dimStyle.Render("   no matches") + "\n")
	} else {
		for i := start; i < end; i++ {
			f := filtered[i]
			maxName := m.width - 8
			if maxName < 20 {
				maxName = 20
			}
			if utf8.RuneCountInString(f) > maxName {
				f = "…" + string([]rune(f)[utf8.RuneCountInString(f)-maxName+1:])
			}
			marker := "   "
			if i == m.docCursor {
				marker = " › "
				sb.WriteString(pickerSelectedStyle.Render(marker+f) + "\n")
			} else {
				sb.WriteString(pickerRowStyle.Render(marker+f) + "\n")
			}
		}
		if len(filtered) > maxVisible {
			sb.WriteString("\n")
			sb.WriteString(dimStyle.Render(fmt.Sprintf("   … %d more", len(filtered)-maxVisible)) + "\n")
		}
	}

	sb.WriteString("\n\n")
	sb.WriteString(dimStyle.Render("   ↑↓ navigate  enter attach  esc back"))
	return sb.String()
}
