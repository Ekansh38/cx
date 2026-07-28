package ui

// Doc-chat mode: connect documents to a conversation and discuss/edit them.
// Docs render in the user's editor (neovim in a tmux split, opened with the
// cx RPC bridge), not in cx. Every connected doc is sent as a system message
// on each turn, re-read from disk. The model proposes SEARCH/REPLACE edit
// blocks, reviewed hunk-by-hunk in neovim (fallback: in-cx y/n review).

import (
	"sync"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"cx/config"
	"cx/store"
)

type docReloadMsg struct{}
type extReviewTickMsg struct{ n int }

// attachedDoc is one document connected to the conversation.
type attachedDoc struct {
	path    string
	content string
}

// docEdit is one model-proposed SEARCH/REPLACE change awaiting review.
type docEdit struct {
	file    string // target document (absolute path)
	search  string
	replace string
	applied bool
	failed  bool
}

// editGroup batches edits per target file for sequential in-editor review.
type editGroup struct {
	file  string
	edits []docEdit
}

// ── LLM-facing document context ──────────────────────────────────────────────

const docEditInstructions = `You can discuss the documents, and when the user wants changes you propose edits. The user reviews each edit as a diff in their editor and applies, skips, or rejects it (rejections come back to you with the reason).

Edit format (exact):

<edit file="/absolute/path/to/the/document">
<<<<<<< SEARCH
text copied verbatim from the document
=======
the replacement text
>>>>>>> REPLACE
</edit>

THE RULES BELOW MATTER. Violating them makes edits silently fail or become unreviewable.

1. ANCHOR AGAINST THE CURRENT DOCUMENT, VERBATIM.
   The SEARCH text must be copied character-for-character from the document
   content shown above (without the line numbers). Only include lines that
   actually exist: do not add a blank line before the first line or after
   the last line of the file. If line 17 is the last line, a SEARCH ending
   at line 17 ends with that line's text, nothing after it.

2. EVERY EDIT IS INDEPENDENT. NEVER CHAIN.
   Each SEARCH must match the document AS IT IS NOW, never the document as
   it would look after another of your edits was applied. The user may
   apply, skip, or reject any subset in any order. If two changes touch the
   same lines, merge them into one edit block.

3. MINIMAL HUNKS.
   Only the line(s) actually changing, plus at most 1-2 unchanged neighbor
   lines when needed to make the SEARCH unambiguous. Never resend large
   unchanged regions and never the whole document. One <edit> block per
   independent change: a typo fix and a new paragraph are two blocks.
   LISTS SPECIFICALLY: if the user asks you to restructure or renumber a
   list, DO NOT send the whole list as one edit. Send ONE <edit> block per
   discrete change: one to add a header, one per moved item, one per new
   item. Renumbered items count as changes and get their own blocks. Big
   list rewrites reviewed as a single hunk are hostile to review; small
   independent edits are not.

4. INSERTIONS anchor on an existing line:
   - append at the end: SEARCH = the current last line; REPLACE = that same
     line, then the new lines.
   - insert at the top: SEARCH = the current first line; REPLACE = the new
     lines, then that same line.
   - insert in the middle: SEARCH = the line above the insertion point;
     REPLACE = that line, then the new lines.

5. WHEN AN EDIT IS REJECTED with a reason, revise ONLY that edit and propose
   an updated block anchored against the current document. Do not resend
   edits that were applied or are still under review.

6. Don't restate the diff in prose; a one-line summary per edit is plenty.
   The user may reference locations as @L12, @L12-30, or @## Heading Name.`

// docsContextMsg builds the system message carrying all connected documents.
func docsContextMsg(docs []*attachedDoc) string {
	var sb strings.Builder
	if len(docs) == 1 {
		fmt.Fprintf(&sb, "The user has connected a document to this conversation: %s\n\n", docs[0].path)
	} else {
		fmt.Fprintf(&sb, "The user has connected %d documents to this conversation.\n\n", len(docs))
	}
	sb.WriteString("Current content (line numbers added for reference — they are NOT part of the files):\n")
	for _, d := range docs {
		fmt.Fprintf(&sb, "<document path=%q>\n", d.path)
		for i, ln := range strings.Split(d.content, "\n") {
			fmt.Fprintf(&sb, "%d│%s\n", i+1, ln)
		}
		sb.WriteString("</document>\n")
	}
	sb.WriteString("\n")
	sb.WriteString(docEditInstructions)
	return sb.String()
}

// ── Connect / disconnect / reload ────────────────────────────────────────────

// lastDocPathFile remembers the most recently connected doc for `/connect doc`.
func lastDocPathFile() string {
	return filepath.Join(config.DataDir(), "last-doc.txt")
}

// SaveLastDoc records the path for later no-arg /connect doc.
func SaveLastDoc(path string) {
	os.WriteFile(lastDocPathFile(), []byte(path+"\n"), 0o644)
}

func lastDocPath() string {
	data, err := os.ReadFile(lastDocPathFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// resolveDocPath expands ~, resolves to absolute, and validates the file.
func resolveDocPath(path string) (string, error) {
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = home + path[1:]
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	if len(data) > 512*1024 {
		return "", fmt.Errorf("doc too large (>512KB)")
	}
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return "", fmt.Errorf("looks binary, connect a text file")
	}
	return abs, nil
}

// connectDoc connects a document to the conversation and remembers it as the
// last doc. No-op (with a note) if already connected.
func (m Model) connectDoc(path string) (Model, tea.Cmd) {
	abs, err := resolveDocPath(path)
	if err != nil {
		m.errMsg = "doc: " + err.Error()
		return m, nil
	}
	if m.findDoc(abs) != nil {
		m.injectSystemLine(filepath.Base(abs) + " is already connected")
		return m, nil
	}
	data, _ := os.ReadFile(abs)
	m.docs = append(m.docs, &attachedDoc{path: abs, content: string(data)})
	m.store.AddDoc(m.conv.ID, abs)
	SaveLastDoc(abs)
	m.injectSystemLine("connected " + filepath.Base(abs))
	return m, nil
}

// disconnectDoc removes one connected document.
func (m *Model) disconnectDoc(path string) {
	var kept []*attachedDoc
	for _, d := range m.docs {
		if d.path != path {
			kept = append(kept, d)
		}
	}
	m.docs = kept
	m.store.RemoveDoc(m.conv.ID, path)
	m.injectSystemLine("disconnected " + filepath.Base(path))
}

// disconnectAllDocs removes every connected document.
func (m *Model) disconnectAllDocs() {
	n := len(m.docs)
	m.docs = nil
	m.store.ClearDocs(m.conv.ID)
	m.injectSystemLine(fmt.Sprintf("disconnected all %d docs", n))
}

// findDoc returns the connected doc with the given path, or nil.
func (m Model) findDoc(path string) *attachedDoc {
	for _, d := range m.docs {
		if d.path == path {
			return d
		}
	}
	return nil
}

// docPaths lists the connected document paths in order.
func (m Model) docPaths() []string {
	out := make([]string, len(m.docs))
	for i, d := range m.docs {
		out[i] = d.path
	}
	return out
}

// reloadDocs re-reads all connected documents from disk.
func (m *Model) reloadDocs() {
	for _, d := range m.docs {
		if data, err := os.ReadFile(d.path); err == nil {
			d.content = string(data)
		}
	}
}

// loadConvDocs restores the connected docs for the current conversation,
// silently dropping files that no longer exist.
func (m *Model) loadConvDocs() {
	m.docReview = false
	m.docEdits = nil
	m.extGroups = nil
	m.extNotes = nil
	m.extRetry = false
	m.lastApplied = nil
	m.docs = nil
	paths, err := m.store.GetDocs(m.conv.ID)
	if err != nil {
		return
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			m.store.RemoveDoc(m.conv.ID, p) // moved or deleted
			continue
		}
		m.docs = append(m.docs, &attachedDoc{path: p, content: string(data)})
	}
}

// disconnectDocFlow handles /disconnect doc / /doc off — instant when one
// doc is connected, a picker (with ALL) when several are.
func (m Model) disconnectDocFlow() (Model, tea.Cmd) {
	switch len(m.docs) {
	case 0:
		m.errMsg = "no documents connected"
		return m, nil
	case 1:
		m.disconnectDoc(m.docs[0].path)
		return m, nil
	default:
		return m.enterDocListPicker("disconnect")
	}
}

// ── Opening the doc in the user's editor ─────────────────────────────────────

//go:embed cx-review.lua
var reviewLua string

func nvimSockPath() string {
	return filepath.Join(config.DataDir(), "nvim.sock")
}

func reviewLuaPath() string {
	return filepath.Join(config.DataDir(), "cx-review.lua")
}

// EditorArgs builds the command to open the doc in the user's editor.
// For neovim it wires up the cx bridge: an RPC socket (hot reload + in-editor
// edit review) and the embedded review lua.
func EditorArgs(docPath string) []string {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "nvim"
	}
	if !strings.Contains(filepath.Base(editor), "nvim") {
		return []string{editor, docPath}
	}
	os.WriteFile(reviewLuaPath(), []byte(reviewLua), 0o644)
	sock := nvimSockPath()
	if !nvimAlive(sock) {
		os.Remove(sock) // stale socket would block --listen
		return []string{editor, "--listen", sock, "-c", "luafile " + reviewLuaPath(), docPath}
	}
	// An earlier cx-spawned nvim already owns the socket — plain open.
	return []string{editor, docPath}
}

// rpc runs an nvim remote command with a hard timeout so a busy editor
// (e.g. one sitting in an input prompt) can never freeze the cx UI.
func rpc(timeout time.Duration, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, "nvim", args...).Run()
}

// nvimAlive reports whether a cx-spawned neovim is listening on the socket.
// The 3-second timeout is deliberate — the check forks a fresh nvim (100-
// 500ms of cold start on macOS) and pings via socket, and a 1s budget was
// racy under real load. False negatives here dropped the user into the
// in-cx fallback review instead of the diff-in-nvim flow.
func nvimAlive(sock string) bool {
	if _, err := os.Stat(sock); err != nil {
		return false
	}
	return rpc(3*time.Second, "--server", sock, "--remote-expr", "1") == nil
}

// syncDocs asks the bridged neovim to save any unsaved changes in connected
// docs, so the disk (what the model sees) always matches the buffer (what the
// user sees). Undoing past a review's save no longer desyncs the two.
//
// Runs the RPC in a goroutine and returns immediately. Each `nvim --server
// --remote-expr` call forks a fresh nvim (100-500ms on macOS), and blocking
// enter for that cost was the biggest source of felt lag on every send. The
// tradeoff: if nvim hasn't flushed by the time the message hits the model,
// the model reads the last-saved version. In practice users `:w` before
// switching to cx; syncDocs is a safety net, not a load-bearing sync.
func syncDocs(paths []string) {
	sock := nvimSockPath()
	if _, err := os.Stat(sock); err != nil || len(paths) == 0 {
		return
	}
	os.WriteFile(filepath.Join(config.DataDir(), "docs.txt"), []byte(strings.Join(paths, "\n")+"\n"), 0o644)
	go rpc(3*time.Second, "--server", sock, "--remote-expr", "v:lua.CxSyncDocs()")
}

// pokeChecktime asks the bridged neovim to re-read changed files from disk.
// Async — nobody waits on the return.
func pokeChecktime() {
	sock := nvimSockPath()
	if _, err := os.Stat(sock); err != nil {
		return
	}
	go rpc(2*time.Second, "--server", sock, "--remote-expr", "v:lua.CxChecktime()")
}

// openDocEditor opens a connected document in the editor. Inside tmux it
// opens a split pane beside cx; otherwise it suspends cx into $EDITOR.
func (m Model) openDocEditor(path string) (Model, tea.Cmd) {
	args := EditorArgs(path)
	if os.Getenv("TMUX") != "" {
		tmuxArgs := append([]string{"split-window", "-h", "-b", "-l", "60%"}, args...)
		if err := exec.Command("tmux", tmuxArgs...).Run(); err == nil {
			return m, nil
		}
	}
	return m, tea.ExecProcess(exec.Command(args[0], args[1:]...), func(err error) tea.Msg {
		return docReloadMsg{}
	})
}

// autoEditorSplit opens the editor beside cx after an explicit /doc attach
// (no-op outside tmux; keeps focus on cx).
func (m *Model) autoEditorSplit(path string) {
	if path == "" || os.Getenv("TMUX") == "" {
		return
	}
	tmuxArgs := append([]string{"split-window", "-h", "-b", "-d", "-l", "60%"}, EditorArgs(path)...)
	exec.Command("tmux", tmuxArgs...).Run()
}

// ── In-neovim edit review ────────────────────────────────────────────────────
//
// When the bridged neovim is alive, proposed edits are reviewed there instead
// of in cx: cx writes edits.json, calls CxReview() over RPC, and polls for
// edits-done.json. Neovim renders the inline diff, applies approved hunks to
// the buffer itself (no reload races), and records rejection reasons.

func editsReqPath() string  { return filepath.Join(config.DataDir(), "edits.json") }
func editsDonePath() string { return filepath.Join(config.DataDir(), "edits-done.json") }
func rejectNowPath() string { return filepath.Join(config.DataDir(), "reject-now.jsonl") }

type editResult struct {
	Applied  bool   `json:"applied"`
	Reason   string `json:"reason"`
	Reported bool   `json:"reported"` // already sent to the model via reject-now
}

// rejectEvent is one N-with-note decision flagged for an immediate revision.
type rejectEvent struct {
	Reason        string `json:"reason"`
	Search        string `json:"search"`
	Replace       string `json:"replace"`
	SelectionFile string `json:"selection_file,omitempty"`
	SelectionFrom int    `json:"selection_from,omitempty"`
	SelectionTo   int    `json:"selection_to,omitempty"`
	SelectionText string `json:"selection_text,omitempty"`
}

// consumeRejectNow returns rejections neovim flagged for an immediate
// revision (written the moment the user presses N, mid-review).
func consumeRejectNow() []rejectEvent {
	// Rename before reading: a lua append racing this lands in a fresh file
	// for the next tick instead of vanishing into an unlinked inode.
	tmp := rejectNowPath() + ".consuming"
	if err := os.Rename(rejectNowPath(), tmp); err != nil {
		return nil
	}
	data, err := os.ReadFile(tmp)
	os.Remove(tmp)
	if err != nil {
		return nil
	}
	var events []rejectEvent
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var ev rejectEvent
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Reason != "" {
			events = append(events, ev)
		}
	}
	return events
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// reloadReviewLua re-sources the review lua in the live nvim so cx binary
// updates take effect on the very next review — no nvim restart needed.
func reloadReviewLua() {
	sock := nvimSockPath()
	if _, err := os.Stat(sock); err != nil {
		return
	}
	rpc(2*time.Second, "--server", sock, "--remote-expr",
		fmt.Sprintf("execute('luafile %s')", reviewLuaPath()))
}

// abortExternalReview tells a live in-editor review to discard its pending
// proposal text (used when cx gives up on it or a conversation goes away).
func abortExternalReview() {
	sock := nvimSockPath()
	if _, err := os.Stat(sock); err != nil {
		return
	}
	reloadReviewLua()
	rpc(2*time.Second, "--server", sock, "--remote-expr", "v:lua.CxAbort()")
}

// applyAllExternalReview tells a live in-editor review to accept every
// pending hunk (like pressing 'a'). Used by the model's tool call.
func applyAllExternalReview() {
	sock := nvimSockPath()
	if _, err := os.Stat(sock); err != nil {
		return
	}
	reloadReviewLua()
	rpc(10*time.Second, "--server", sock, "--remote-expr", "v:lua.CxApplyAll()")
}

// appendExternalReview injects new edits into the live in-editor review
// (no fresh review, no aborting the current one, no queueing). Returns
// false when there's no live socket or the RPC fails; caller falls back
// to starting a fresh review.
func appendExternalReview(docPath string, edits []docEdit) bool {
	sock := nvimSockPath()
	if !nvimAlive(sock) {
		return false
	}
	type reqEdit struct {
		Search  string `json:"search"`
		Replace string `json:"replace"`
	}
	req := struct {
		File  string    `json:"file"`
		Edits []reqEdit `json:"edits"`
	}{File: docPath}
	for _, e := range edits {
		req.Edits = append(req.Edits, reqEdit{Search: e.search, Replace: e.replace})
	}
	buf, err := json.Marshal(req)
	if err != nil {
		return false
	}
	// Clear the previous review's results file before writing the new
	// request. Without this, extReviewTick could pick up stale edits-done
	// from the last review and skip polling for the new one.
	os.Remove(editsDonePath())
	if err := os.WriteFile(editsReqPath(), buf, 0o644); err != nil {
		return false
	}
	reloadReviewLua()
	return rpc(10*time.Second, "--server", sock, "--remote-expr", "v:lua.CxAddEdits()") == nil
}

// applyPendingEditByIndex applies a specific pending hunk by its 1-based
// index (matches the "cx X/N" number in the review footer).
func applyPendingEditByIndex(idx int) bool {
	sock := nvimSockPath()
	if _, err := os.Stat(sock); err != nil {
		return false
	}
	reloadReviewLua()
	return rpc(10*time.Second, "--server", sock, "--remote-expr",
		fmt.Sprintf("v:lua.CxApplyIndex(%d)", idx)) == nil
}

// rejectPendingEditByIndex rejects a specific pending hunk with a reason.
// The reason is streamed back so the model can auto-retry, same as N.
func rejectPendingEditByIndex(idx int, reason string) bool {
	sock := nvimSockPath()
	if _, err := os.Stat(sock); err != nil {
		return false
	}
	reloadReviewLua()
	esc := strings.ReplaceAll(reason, "'", "''")
	return rpc(10*time.Second, "--server", sock, "--remote-expr",
		fmt.Sprintf("v:lua.CxRejectIndex(%d, '''%s''')", idx, esc)) == nil
}

// reviewSignal carries model-tool decisions (discard, apply-all) from the
// tool goroutine to the extReviewTick handler in Update.
var reviewSignal struct {
	mu      sync.Mutex
	discard bool
}

// RequestReviewDiscard marks the current review to be dropped on the next
// tick and tells neovim to remove any pending proposal text right now.
func RequestReviewDiscard() {
	reviewSignal.mu.Lock()
	reviewSignal.discard = true
	reviewSignal.mu.Unlock()
	abortExternalReview()
}

// consumeReviewDiscard clears the flag and returns whether it was set.
func consumeReviewDiscard() bool {
	reviewSignal.mu.Lock()
	defer reviewSignal.mu.Unlock()
	d := reviewSignal.discard
	reviewSignal.discard = false
	return d
}

// isKnownEditSearch reports whether the given SEARCH text belongs to any
// edit in the current review groups. Used to reject stale/leaked events.
func isKnownEditSearch(groups []editGroup, search string) bool {
	for _, g := range groups {
		for _, e := range g.edits {
			if e.search == search {
				return true
			}
		}
	}
	return false
}

// startExternalReview hands the edits to neovim. Returns false (caller falls
// back to the in-cx review) if the bridge isn't up.
//
// No pre-flight nvimAlive() check here anymore — that fork burned another
// nvim cold-start every review, and its 1-second timeout misdiagnosed the
// bridge as dead under load. The actual RPC below fails cleanly if nvim
// isn't listening; that's an authoritative answer, not a probe.
func startExternalReview(docPath string, edits []docEdit) bool {
	sock := nvimSockPath()
	if _, err := os.Stat(sock); err != nil {
		return false // no socket = definitely no bridge
	}
	type reqEdit struct {
		Search  string `json:"search"`
		Replace string `json:"replace"`
	}
	req := struct {
		File  string    `json:"file"`
		Edits []reqEdit `json:"edits"`
	}{File: docPath}
	for _, e := range edits {
		req.Edits = append(req.Edits, reqEdit{Search: e.search, Replace: e.replace})
	}
	buf, err := json.Marshal(req)
	if err != nil {
		return false
	}
	os.Remove(editsDonePath())
	if err := os.WriteFile(editsReqPath(), buf, 0o644); err != nil {
		return false
	}
	// Re-write the lua file too, in case cx was upgraded since spawn
	os.WriteFile(reviewLuaPath(), []byte(reviewLua), 0o644)
	reloadReviewLua()
	// 10s timeout: CxReview does real work (locate text in buffer, insert
	// green blocks, set up keymaps). 3s was too tight on a loaded machine
	// or a large file and silently caused fallback to in-cx review.
	return rpc(10*time.Second, "--server", sock, "--remote-expr", "v:lua.CxReview()") == nil
}

// readExternalReviewResults returns the decisions once neovim wrote them.
func readExternalReviewResults() ([]editResult, bool) {
	data, err := os.ReadFile(editsDonePath())
	if err != nil {
		return nil, false
	}
	var out struct {
		Results []editResult `json:"results"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, false
	}
	os.Remove(editsDonePath())
	os.Remove(editsReqPath())
	return out.Results, true
}

// summarizeReview builds the note persisted into the conversation — the model
// sees this, including WHY edits were rejected.
func summarizeReview(results []editResult, docName string) string {
	applied := 0
	var parts []string
	for i, r := range results {
		switch {
		case r.Applied:
			applied++
		case r.Reason != "":
			parts = append(parts, fmt.Sprintf("edit %d rejected: %q", i+1, r.Reason))
		default:
			parts = append(parts, fmt.Sprintf("edit %d skipped", i+1))
		}
	}
	note := fmt.Sprintf("applied %d/%d edits to %s", applied, len(results), docName)
	if len(parts) > 0 {
		note += " (" + strings.Join(parts, "; ") + ")"
	}
	return note
}

func extReviewTick(n int) tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg {
		return extReviewTickMsg{n: n}
	})
}

// ── Edit blocks: parse / apply / review ──────────────────────────────────────

// parseDocEdits extracts SEARCH/REPLACE blocks from an assistant response.
// The open tag may carry a file attribute: <edit file="/abs/path">.
func parseDocEdits(s string) []docEdit {
	var edits []docEdit
	for {
		start := strings.Index(s, "<edit")
		if start < 0 {
			break
		}
		tagEnd := strings.Index(s[start:], ">")
		if tagEnd < 0 {
			break
		}
		openTag := s[start : start+tagEnd+1]
		rel := strings.Index(s[start:], "</edit>")
		if rel < 0 {
			break
		}
		block := s[start+tagEnd+1 : start+rel]
		s = s[start+rel+len("</edit>"):]
		if e, ok := parseEditBlock(block); ok {
			e.file = parseFileAttr(openTag)
			edits = append(edits, e)
		}
	}
	return edits
}

// parseFileAttr extracts the file="..." value from an <edit ...> open tag.
func parseFileAttr(tag string) string {
	i := strings.Index(tag, `file="`)
	if i < 0 {
		return ""
	}
	rest := tag[i+len(`file="`):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// resolveEditFiles fills in missing edit targets: single doc means that doc;
// otherwise the doc whose content contains the SEARCH text.
func resolveEditFiles(edits []docEdit, docs []*attachedDoc) []docEdit {
	for i := range edits {
		if edits[i].file != "" {
			continue
		}
		if len(docs) == 1 {
			edits[i].file = docs[0].path
			continue
		}
		for _, d := range docs {
			if _, ok := applyEditTo(d.content, edits[i].search, edits[i].replace); ok {
				edits[i].file = d.path
				break
			}
		}
	}
	return edits
}

// groupEditsByFile batches edits per target file, preserving order.
func groupEditsByFile(edits []docEdit) []editGroup {
	var groups []editGroup
	idx := map[string]int{}
	for _, e := range edits {
		if i, ok := idx[e.file]; ok {
			groups[i].edits = append(groups[i].edits, e)
			continue
		}
		idx[e.file] = len(groups)
		groups = append(groups, editGroup{file: e.file, edits: []docEdit{e}})
	}
	return groups
}

func parseEditBlock(b string) (docEdit, bool) {
	var search, replace []string
	mode := 0
	for ln := range strings.SplitSeq(b, "\n") {
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

// applyDocEdit applies edit i to its target document and writes it to disk.
func (m *Model) applyDocEdit(i int) bool {
	e := &m.docEdits[i]
	doc := m.findDoc(e.file)
	if doc == nil {
		e.failed = true
		return false
	}
	newContent, ok := applyEditTo(doc.content, e.search, e.replace)
	if !ok {
		e.failed = true
		return false
	}
	if err := os.WriteFile(doc.path, []byte(newContent), 0o644); err != nil {
		m.errMsg = "doc write: " + err.Error()
		e.failed = true
		return false
	}
	doc.content = newContent
	e.applied = true
	pokeChecktime() // hot-reload the buffer in a bridged editor
	return true
}

// showDocEdit injects the current pending edit as a colored diff into the chat.
func (m *Model) showDocEdit() {
	e := m.docEdits[m.docEditIdx]
	maxW := max(m.width-6, 20)
	var sb strings.Builder
	header := fmt.Sprintf("── proposed edit %d/%d", m.docEditIdx+1, len(m.docEdits))
	if e.file != "" {
		header += " · " + filepath.Base(e.file)
	}
	sb.WriteString(dimStyle.Render(header+" ──") + "\n")
	for l := range strings.SplitSeq(e.search, "\n") {
		sb.WriteString(diffOldStyle.Render(truncate("  - "+l, maxW)) + "\n")
	}
	for l := range strings.SplitSeq(e.replace, "\n") {
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
			m.injectSystemLine("couldn't locate that text in the doc, skipped")
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
		m.injectSystemLine("review cancelled")
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
			m.lastApplied = append(m.lastApplied, e)
		}
		if e.failed {
			failed++
		}
	}
	note := fmt.Sprintf("applied %d/%d edits", applied, len(m.docEdits))
	if failed > 0 {
		note += fmt.Sprintf(" (%d couldn't be located)", failed)
	}
	m.injectSystemLine(note)
}

// stripEditBlocks replaces raw <edit> blocks with a compact placeholder for
// display. An unterminated trailing block (mid-stream, or a model that forgot
// the closing tag) is hidden too — raw SEARCH/REPLACE markers render as
// blockquote garbage otherwise.
//
// Also strips BARE conflict-marker blocks (`<<<<<<< SEARCH` / `=======` /
// `>>>>>>> REPLACE`) that the model emits without the `<edit>` wrapper.
// This shows up when a doc was disconnected mid-conversation and the model
// keeps mimicking the edit format from history — cx has no doc to apply
// the edit to, so all the user sees is `>>>>>>>` rendered by glamour as
// seven nested blockquote bars. Hide the whole block behind a placeholder.
func stripEditBlocks(s string) string {
	n := 0
	for {
		start := strings.Index(s, "<edit")
		if start < 0 {
			break
		}
		rel := strings.Index(s[start:], "</edit>")
		if rel < 0 {
			s = s[:start] + fmt.Sprintf("*[proposing edit %d…]*", n+1)
			break
		}
		n++
		s = s[:start] + fmt.Sprintf("*[proposed edit %d]*", n) + s[start+rel+len("</edit>"):]
	}
	// Bare conflict-marker blocks (no <edit> wrapper). We look for the
	// SEARCH-line marker and swallow through the matching REPLACE-line
	// marker. An unterminated block gets hidden the same way as an
	// unterminated <edit> would.
	for {
		start := strings.Index(s, "<<<<<<< SEARCH")
		if start < 0 {
			break
		}
		// End marker can be on its own line as ">>>>>>> REPLACE".
		endMarker := ">>>>>>> REPLACE"
		endRel := strings.Index(s[start:], endMarker)
		if endRel < 0 {
			s = s[:start] + "*[bare edit block dropped — no doc connected]*"
			break
		}
		n++
		s = s[:start] + fmt.Sprintf("*[bare edit block %d dropped — no doc connected]*", n) + s[start+endRel+len(endMarker):]
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

// selCache throttles the per-render selection stat for the status bar.
var selCache struct {
	sel *docSelection
	at  time.Time
}

// cachedSelection is readSelection with a short TTL, for render paths.
func cachedSelection() *docSelection {
	if time.Since(selCache.at) < time.Second {
		return selCache.sel
	}
	selCache.sel = readSelection()
	selCache.at = time.Now()
	return selCache.sel
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

// ── Doc file picker (/doc with no args) ──────────────────────────────────────

var docSkipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "target": true,
	"dist": true, "build": true, ".git": true,
}

var docExts = map[string]bool{".md": true, ".markdown": true, ".txt": true}

// listFilesByExt finds files with the given extensions under the current
// directory (capped, skipping hidden/vendor dirs).
func listFilesByExt(exts map[string]bool) []string {
	root, err := os.Getwd()
	if err != nil {
		return nil
	}
	var out []string
	visited := 0
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		visited++
		if visited > 20000 {
			return fs.SkipAll // huge tree (launched from $HOME etc): stop walking
		}
		if d.IsDir() {
			name := d.Name()
			if p != root && (strings.HasPrefix(name, ".") || docSkipDirs[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		if exts[strings.ToLower(filepath.Ext(p))] {
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

// listDocFiles finds markdown/text files under the current directory.
func listDocFiles() []string {
	return listFilesByExt(docExts)
}

// mentionFileCache avoids re-walking the filesystem on every render frame
// while an @mention is being typed. Single-threaded (bubbletea Update/View).
var mentionFileCache struct {
	files []string
	at    time.Time
}

func mentionFiles() []string {
	if time.Since(mentionFileCache.at) < 10*time.Second {
		return mentionFileCache.files
	}
	exts := make(map[string]bool, len(docExts)+len(imageExts)+1)
	for e := range docExts {
		exts[e] = true
	}
	for e := range imageExts {
		exts[e] = true
	}
	exts[".pdf"] = true
	mentionFileCache.files = listFilesByExt(exts)
	mentionFileCache.at = time.Now()
	return mentionFileCache.files
}

// mentionCandidates fuzzy-matches an @mention query against connectable
// files under the current directory (docs and images).
func mentionCandidates(query string) []string {
	files := mentionFiles()
	if query == "" {
		return files
	}
	q := strings.ToLower(query)
	var exact, fuzzy []string
	for _, f := range files {
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

func (m Model) enterDocPicker() (Model, tea.Cmd) {
	files := listDocFiles()
	if len(files) == 0 {
		m.errMsg = "no .md/.txt files found under " + cwdBase() + " (use /doc <path>)"
		return m, nil
	}
	m.state = stateDocPicker
	m.docFiles = files
	m.docFilter = ""
	m.docCursor = 0
	return m, nil
}

// StartInDocPicker opens the document picker on launch (used by `cx doc`).
func (m Model) StartInDocPicker() Model {
	m2, _ := m.enterDocPicker()
	m2.docPickerQuits = true
	return m2
}

// MarkIncognito flags this session as ephemeral: no memory injection, no
// external memory, no memory curation, no /remember. Called by main.go when
// cx was launched via `cx incognito`. The conversation row is deleted on
// quit (see main.go).
func (m Model) MarkIncognito() Model {
	m.incognito = true
	m.injectSystemLine("── incognito mode ── no memory, no external files, chat deleted on quit")
	return m
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
		if m.docPickerQuits {
			return m, tea.Quit
		}
		m.state = stateChat
		return m, nil

	case tea.KeyEnter:
		filtered := m.filteredDocFiles()
		if len(filtered) == 0 {
			if m.docPickerQuits {
				return m, tea.Quit
			}
			m.state = stateChat
			return m, nil
		}
		m.state = stateChat
		fromLaunch := m.docPickerQuits
		m.docPickerQuits = false
		if fromLaunch {
			// `cx doc`: open the editor and remember the file, nothing more.
			// Connecting stays explicit via /connect doc — but don't lie
			// about connection state: a doc restored from a prior session is
			// already connected, and saying "not connected" then "already
			// connected" on /connect is a confusing contradiction.
			abs, err := resolveDocPath(filtered[m.docCursor])
			if err != nil {
				m.errMsg = "doc: " + err.Error()
				return m, nil
			}
			SaveLastDoc(abs)
			m.autoEditorSplit(abs)
			if m.findDoc(abs) != nil {
				m.injectSystemLine("opened " + filepath.Base(abs) + " (already connected to this chat)")
			} else {
				m.injectSystemLine("opened " + filepath.Base(abs) + " (not connected), /connect doc to attach it")
			}
			return m, nil
		}
		m2, cmd := m.connectDoc(filtered[m.docCursor])
		if len(m2.docs) > 0 {
			m2.autoEditorSplit(m2.docs[len(m2.docs)-1].path)
		}
		return m2, cmd

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
	// Substring matches rank above fuzzy (subsequence) matches
	var exact, fuzzy []string
	for _, f := range m.docFiles {
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

// fuzzyMatch reports whether all runes of query appear in s in order
// (fzf-style subsequence matching). Both inputs must be lowercased.
func fuzzyMatch(query, s string) bool {
	if query == "" {
		return true
	}
	qr := []rune(query)
	i := 0
	for _, r := range s {
		if r == qr[i] {
			i++
			if i == len(qr) {
				return true
			}
		}
	}
	return false
}

func (m Model) docPickerView() string {
	filtered := m.filteredDocFiles()
	maxVisible := max(m.height-10, 1)
	start := 0
	if m.docCursor >= maxVisible {
		start = m.docCursor - maxVisible + 1
	}
	end := min(start+maxVisible, len(filtered))

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
			maxName := max(m.width-8, 20)
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
	escHint := "esc back"
	if m.docPickerQuits {
		escHint = "esc quit"
	}
	sb.WriteString(dimStyle.Render("   ↑↓ navigate  enter attach  " + escHint))
	return sb.String()
}

// ── Connected-docs list picker (/disconnect doc / /doc edit with many) ───────

const docListAll = "ALL"

// enterDocListPicker opens a fuzzy list of connected docs. mode is "open"
// (open in editor) or "disconnect" (which also offers ALL).
func (m Model) enterDocListPicker(mode string) (Model, tea.Cmd) {
	if len(m.docs) == 0 {
		m.errMsg = "no documents connected"
		return m, nil
	}
	m.docListMode = mode
	m.docListItems = nil
	if mode == "disconnect" {
		m.docListItems = append(m.docListItems, docListAll)
	}
	m.docListItems = append(m.docListItems, m.docPaths()...)
	m.docListFilter = ""
	m.docListCursor = 0
	m.state = stateDocListPicker
	return m, nil
}

func (m Model) filteredDocList() []string {
	if m.docListFilter == "" {
		return m.docListItems
	}
	q := strings.ToLower(m.docListFilter)
	var exact, fuzzy []string
	for _, f := range m.docListItems {
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

func (m Model) updateDocListPicker(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.state = stateChat
		return m, nil

	case tea.KeyEnter:
		filtered := m.filteredDocList()
		if len(filtered) == 0 {
			m.state = stateChat
			return m, nil
		}
		sel := filtered[m.docListCursor]
		m.state = stateChat
		if m.docListMode == "disconnect" {
			if sel == docListAll {
				m.disconnectAllDocs()
			} else {
				m.disconnectDoc(sel)
			}
			return m, nil
		}
		return m.openDocEditor(sel)

	case tea.KeyUp:
		if m.docListCursor > 0 {
			m.docListCursor--
		}
		return m, nil

	case tea.KeyDown:
		if m.docListCursor < len(m.filteredDocList())-1 {
			m.docListCursor++
		}
		return m, nil

	case tea.KeyBackspace, tea.KeyDelete:
		if len(m.docListFilter) > 0 {
			_, size := utf8.DecodeLastRuneInString(m.docListFilter)
			m.docListFilter = m.docListFilter[:len(m.docListFilter)-size]
			m.docListCursor = 0
		}
		return m, nil

	case tea.KeySpace:
		m.docListFilter += " "
		m.docListCursor = 0
		return m, nil

	case tea.KeyRunes:
		m.docListFilter += string(msg.Runes)
		m.docListCursor = 0
		return m, nil
	}
	return m, nil
}

func (m Model) docListPickerView() string {
	filtered := m.filteredDocList()
	title := "Open document"
	if m.docListMode == "disconnect" {
		title = "Disconnect document"
	}

	var sb strings.Builder
	sb.WriteString("\n\n")
	sb.WriteString(pickerTitleStyle.Render("   "+title) + "\n\n")

	filterText := m.docListFilter
	if filterText == "" {
		filterText = dimStyle.Render("type to filter...")
	}
	sb.WriteString(promptStyle.Render("   > ") + filterText + "\n\n")

	if len(filtered) == 0 {
		sb.WriteString(dimStyle.Render("   no matches") + "\n")
	} else {
		for i, f := range filtered {
			name, dir := f, ""
			if f != docListAll {
				name, dir = filepath.Base(f), "  "+dimStyle.Render(filepath.Dir(f))
			}
			if i == m.docListCursor {
				sb.WriteString(pickerSelectedStyle.Render(" › "+name) + dir + "\n")
			} else {
				sb.WriteString(pickerRowStyle.Render("   "+name) + dir + "\n")
			}
		}
	}

	sb.WriteString("\n\n")
	sb.WriteString(dimStyle.Render("   ↑↓ navigate  enter select  esc back"))
	return sb.String()
}
