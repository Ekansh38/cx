package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"cx/config"
	"cx/llm"
	"cx/store"
	"cx/ui"
)

func main() {
	// Detached memory-curation child: reads state, writes memory.md, exits.
	// Spawned by the TUI on quit so quitting stays instant.
	if len(os.Args) >= 3 && os.Args[1] == "_curate" {
		os.Exit(ui.RunFlushChild(os.Args[2]))
	}

	// Zombie-cx guard. Every review overwrites ~/.local/share/cx/cx-review.lua
	// with whatever cx wrote it last; a stale cx from a previous build kept
	// stomping the fresh lua and breaking the diff render. On every startup,
	// look for other cx processes running from an older binary and kill them.
	// Same-binary siblings are LEFT ALONE (legitimate multi-window use is fine).
	killStaleSiblings()

	// Review handoff files (edits/edits-done/reject-now) are per-session
	// ephemera. If cx or the bridged neovim crashed last time, leftovers
	// would be replayed against a new session, leaking test-fixture text
	// or stale rejections into a real conversation. Clean them at startup.
	dd := config.DataDir()
	os.Remove(filepath.Join(dd, "edits.json"))
	os.Remove(filepath.Join(dd, "edits-done.json"))
	os.Remove(filepath.Join(dd, "reject-now.jsonl"))
	os.Remove(filepath.Join(dd, "selection.txt")) // stale editor highlight from a crashed nvim / prior tests

	// cx vim [file] — open a document in the user's editor with the cx bridge
	// (RPC socket + review lua when neovim), but WITHOUT a tmux split. Use
	// this to open any of the several docs connected to one chat. With no
	// argument, fuzzy-pick among the most recent conversation's connected
	// docs (falling back to files in the current directory).
	if len(os.Args) >= 2 && os.Args[1] == "vim" {
		runVim()
		return
	}

	// cx doc [file] — auto-launch a split: editor on the left, cx on the right.
	// Without a file, cx starts in the fuzzy document picker.
	var docPath string
	startDocPicker := false
	if len(os.Args) >= 2 && os.Args[1] == "doc" {
		if len(os.Args) >= 3 {
			docPath = launchDocSplit(os.Args[2])
		} else {
			ensureTmuxSession("doc") // so the editor split can open after picking
			startDocPicker = true
		}
	}

	// cx incognito — start an ephemeral chat: no memory injection, no external
	// memory, no system prompt, no persistence. The conversation is created
	// in the DB so tea's model shape stays uniform, but it's deleted on quit.
	incognito := len(os.Args) >= 2 && (os.Args[1] == "incognito" || os.Args[1] == "-i")

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	dbPath := filepath.Join(config.DataDir(), "cx.db")
	st, err := store.New(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "store: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	// Load or create conversation
	conv, err := st.MostRecent()
	if err != nil {
		log.Fatal(err)
	}

	// Resolve model: env var > last explicitly selected > most recent
	// conversation > config > fallback
	model := cfg.Model
	if conv != nil && conv.Model != "" {
		model = conv.Model
	}
	if lm := config.LastModel(); lm != "" {
		model = lm
	}
	if m := os.Getenv("CX_MODEL"); m != "" {
		model = m
	}
	if model == "" {
		model = "llama3.2" // last resort fallback
	}

	prov, err := llm.ForModel(model, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "provider: %v\n", err)
		os.Exit(1)
	}

	if conv == nil || incognito {
		conv, err = st.CreateConversation(model)
		if err != nil {
			log.Fatal(err)
		}
		if incognito {
			// Tag the ephemeral chat clearly so if it somehow survives
			// a crash it's obvious in /list what it was.
			_ = st.UpdateTitle(conv.ID, "(incognito)")
			conv.Title = "(incognito)"
		}
	}

	// Doc mode opens the editor beside cx and remembers the file, but does
	// NOT connect it to any conversation: that stays explicit (/connect doc)
	if docPath != "" && !incognito {
		ui.SaveLastDoc(docPath)
	}

	// Load messages (incognito starts fresh — the row was just created).
	msgs, err := st.GetMessages(conv.ID)
	if err != nil {
		log.Fatal(err)
	}

	// Build system prompt. Incognito skips memory + external memory: just
	// the base personality prompt so the model is still a decent assistant
	// but knows nothing about the user.
	var sysPrompt string
	if incognito {
		sysPrompt = ui.LoadBasePromptOnly()
	} else {
		sysPrompt = ui.BuildSystemPrompt(config.LoadMemory())
	}

	// Launch TUI
	m := ui.New(cfg, st, conv, msgs, prov, model, sysPrompt)
	if incognito {
		m = m.MarkIncognito()
	}
	if startDocPicker {
		m = m.StartInDocPicker()
	}
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
		os.Exit(1)
	}
	if incognito {
		// The whole point of incognito: leave nothing behind. Deletion
		// after Run() returns catches both clean quits and any tea.Quit
		// paths that landed us here. If it somehow fails, the conv still
		// has "(incognito)" title so it's spot-able in /list.
		_ = st.DeleteConversation(conv.ID)
	}
}

// ensureTmuxSession re-execs cx inside a new tmux session when run outside
// one (and exits). Inside tmux, or when tmux isn't installed, it's a no-op —
// without tmux the doc view still works, just without the editor split.
func ensureTmuxSession(args ...string) {
	if os.Getenv("TMUX") != "" {
		return
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	cmd := exec.Command("tmux", append([]string{"new-session", exe}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// launchDocSplit resolves the doc path and sets up the tmux split view.
// Outside tmux it re-execs itself inside a new tmux session and exits;
// inside tmux it opens the editor in a left pane and returns the path.
func launchDocSplit(path string) string {
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = home + path[1:]
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doc: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stat(abs); err != nil {
		fmt.Fprintf(os.Stderr, "doc: %v\n", err)
		os.Exit(1)
	}

	ensureTmuxSession("doc", abs)

	// Inside tmux — open the editor in a pane to the left of cx,
	// wired up with the cx bridge (RPC socket + review lua) when it's neovim
	args := append([]string{"split-window", "-h", "-b", "-l", "60%"}, ui.EditorArgs(abs)...)
	exec.Command("tmux", args...).Run()
	return abs
}

// runVim implements `cx vim [path]`: open a document in the user's editor
// with the cx bridge wired up (so in-neovim edit review still works), but
// without a tmux split — the editor runs in the foreground and cx exits.
// With no path, fuzzy-pick among the most recent conversation's connected
// docs (the multi-doc-per-chat case), falling back to files in the cwd.
func runVim() {
	if len(os.Args) >= 3 && strings.TrimSpace(os.Args[2]) != "" {
		openInEditorForeground(os.Args[2])
		return
	}

	// No path given: build a candidate list and pipe it to fzf.
	dbPath := filepath.Join(config.DataDir(), "cx.db")
	st, err := store.New(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "store: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	var candidates []string
	if conv, _ := st.MostRecent(); conv != nil {
		if docs, _ := st.GetDocs(conv.ID); len(docs) > 0 {
			candidates = append(candidates, docs...)
		}
	}
	// Always include files under the cwd as additional candidates so a
	// not-yet-connected document is reachable too.
	candidates = append(candidates, cwdFiles()...)

	if len(candidates) == 0 {
		fmt.Fprintln(os.Stderr, "vim: no documents to pick from (connect one with /connect doc, or run from a directory with files)")
		os.Exit(1)
	}

	picked, ok := fuzzyPick(candidates)
	if !ok {
		return
	}
	openInEditorForeground(picked)
}

// openInEditorForeground resolves the path and execs the editor with the cx
// bridge args, replacing the current process (no tmux split).
func openInEditorForeground(path string) {
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = home + path[1:]
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vim: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stat(abs); err != nil {
		fmt.Fprintf(os.Stderr, "vim: %v\n", err)
		os.Exit(1)
	}
	args := ui.EditorArgs(abs)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
}

// cwdFiles returns a recursive list of files under the current directory,
// capped so the fzf menu stays snappy. Uses git ls-files when inside a repo
// (respects .gitignore), else falls back to a find.
func cwdFiles() []string {
	// git ls-files is fast and respects .gitignore
	if out, err := exec.Command("git", "ls-files").Output(); err == nil {
		var files []string
		for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if ln != "" {
				files = append(files, ln)
			}
		}
		if len(files) > 0 {
			return files
		}
	}
	// Fallback: walk the cwd, skipping hidden dirs and common junk.
	var files []string
	filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != "." && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "target") {
				return filepath.SkipDir
			}
			return nil
		}
		if len(files) < 2000 {
			files = append(files, path)
		}
		return nil
	})
	return files
}

// fuzzyPick pipes the candidates to fzf and returns the user's choice. If
// fzf isn't installed or the user cancels, ok is false.
func fuzzyPick(candidates []string) (string, bool) {
	fzf, err := exec.LookPath("fzf")
	if err != nil {
		fmt.Fprintln(os.Stderr, "vim: fzf not found (install it for fuzzy search, or pass a path: cx vim <file>)")
		os.Exit(1)
	}
	cmd := exec.Command(fzf, "--prompt", "open> ", "--height", "40%", "--layout", "reverse")
	cmd.Stdin = strings.NewReader(strings.Join(candidates, "\n"))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	picked := strings.TrimSpace(string(out))
	if picked == "" {
		return "", false
	}
	return picked, true
}

// killStaleSiblings finds any other running cx processes whose executable
// is older than ours and sends them SIGTERM. Same-binary siblings and any
// process whose binary path we can't resolve are left alone.
//
// Why: startExternalReview writes ~/.local/share/cx/cx-review.lua from the
// currently-running binary's embedded copy. If a stale cx from a previous
// build performs a review, it clobbers a newer sibling's lua. The user
// then sees a review that renders no diff and cx feels broken. Killing
// the stale processes on startup makes this class of bug impossible.
//
// Best-effort: any error (no lsof, weird ps output, no permission) short-
// circuits without noise. The guard is a safety net, not a hard requirement.
func killStaleSiblings() {
	myExe, err := os.Executable()
	if err != nil {
		return
	}
	myExeAbs, err := filepath.EvalSymlinks(myExe)
	if err != nil {
		myExeAbs = myExe
	}
	myFi, err := os.Stat(myExeAbs)
	if err != nil {
		return
	}
	myPID := os.Getpid()

	// Find candidate cx PIDs by name. `pgrep -x cx` matches processes whose
	// basename is exactly "cx" — safer than a substring match that would
	// hit "cx-something".
	pgrepOut, err := exec.Command("pgrep", "-x", filepath.Base(myExe)).Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(pgrepOut)), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || pid == myPID || pid <= 0 {
			continue
		}
		peerExe := binaryPathOfPID(pid)
		if peerExe == "" {
			continue
		}
		peerFi, err := os.Stat(peerExe)
		if err != nil {
			continue
		}
		// Skip identical binaries. Two legitimate concurrent cx windows on
		// the same build are fine.
		if peerFi.ModTime().Equal(myFi.ModTime()) && peerFi.Size() == myFi.Size() {
			continue
		}
		// Only kill if the sibling's binary is STRICTLY older. Newer
		// siblings might have upgraded past us — don't fight them.
		if !peerFi.ModTime().Before(myFi.ModTime()) {
			continue
		}
		fmt.Fprintf(os.Stderr,
			"cx: killing stale sibling PID %d (binary from %s)\n",
			pid, peerFi.ModTime().Format(time.Kitchen))
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}

// binaryPathOfPID returns the absolute path of the executable a running PID
// is bound to, or "" if it can't be resolved. Uses lsof on macOS/Linux:
// the `txt` file for a process is its executable image. Falls back to
// empty string if lsof isn't installed.
func binaryPathOfPID(pid int) string {
	out, err := exec.Command("lsof", "-p", strconv.Itoa(pid), "-a", "-d", "txt", "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		// lsof -F output prefixes fields with a single char. "n" = name.
		if strings.HasPrefix(line, "n/") {
			return strings.TrimPrefix(line, "n")
		}
	}
	return ""
}
