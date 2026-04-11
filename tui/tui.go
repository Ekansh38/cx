package tui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term"
)

const (
	clearScreen = "\033[2J"
	clearLine   = "\033[2K"
	resetScroll = "\033[r"
	colorCyan   = "\033[36m"
	colorReset  = "\033[0m"
)

func pos(row, col int) string          { return fmt.Sprintf("\033[%d;%dH", row, col) }
func scrollRegion(top, bot int) string { return fmt.Sprintf("\033[%d;%dr", top, bot) }

type Message struct {
	Role    string
	Content string
}

type TUI struct {
	width        int
	height       int
	fd           int
	stdin        *bufio.Reader
	status       string
	lines        []string        // all rendered lines, for scroll redraw
	currentLine  strings.Builder // partial line being built
	col          int             // current column, for word wrap
	scrollOffset int             // 0 = bottom; positive = scrolled up
	closed       bool
	oldState     *term.State // nil if raw mode unavailable
}

func New(status string) (*TUI, error) {
	fd := int(os.Stdin.Fd())
	w, h, err := term.GetSize(fd)
	if err != nil {
		return nil, err
	}
	t := &TUI{
		width:  w,
		height: h,
		fd:     fd,
		stdin:  bufio.NewReader(os.Stdin),
		status: status,
	}
	t.oldState, _ = term.MakeRaw(fd) // nil on failure; ReadLine falls back to line mode
	t.setup()
	return t, nil
}

func (t *TUI) histEnd() int  { return t.height - 3 }
func (t *TUI) sepRow() int   { return t.height - 2 }
func (t *TUI) inputRow() int { return t.height - 1 }
func (t *TUI) statRow() int  { return t.height }

func (t *TUI) setup() {
	fmt.Print(clearScreen)
	fmt.Print(scrollRegion(1, t.histEnd()))
	t.drawFixed()
	fmt.Print(pos(t.histEnd(), 1))
}

func (t *TUI) drawFixed() {
	fmt.Print(pos(t.sepRow(), 1) + clearLine + strings.Repeat("─", t.width))
	fmt.Print(pos(t.inputRow(), 1) + clearLine + colorCyan + "> " + colorReset)
	fmt.Print(pos(t.statRow(), 1) + clearLine + t.status)
}

// writeRune appends a rune to the current line, wrapping at terminal width.
func (t *TUI) writeRune(r rune) {
	if t.col >= t.width {
		fmt.Print("\r\n")
		t.flushLine()
	}
	fmt.Print(string(r))
	t.currentLine.WriteRune(r)
	t.col++
}

// newline flushes the current line to the buffer and moves to the next.
func (t *TUI) newline() {
	fmt.Print("\r\n")
	t.flushLine()
}

func (t *TUI) flushLine() {
	t.lines = append(t.lines, t.currentLine.String())
	t.currentLine.Reset()
	t.col = 0
}

// printLine writes text to the scroll region and records it in the line buffer.
func (t *TUI) printLine(text string) {
	for _, r := range text {
		if r == '\n' {
			t.newline()
			continue
		}
		t.writeRune(r)
	}
	t.newline()
}

// redrawViewport clears the scroll region and repaints the visible window of t.lines.
func (t *TUI) redrawViewport() {
	histHeight := t.histEnd()
	total := len(t.lines)

	maxOffset := total - histHeight
	if maxOffset < 0 {
		maxOffset = 0
	}
	if t.scrollOffset > maxOffset {
		t.scrollOffset = maxOffset
	}

	end := total - t.scrollOffset
	start := end - histHeight
	if start < 0 {
		start = 0
	}

	for i := 0; i < histHeight; i++ {
		fmt.Print(pos(i+1, 1) + clearLine)
		lineIdx := start + i
		if lineIdx >= start && lineIdx < end && lineIdx < total {
			fmt.Print(t.lines[lineIdx])
		}
	}
}

func (t *TUI) PrintHistory(msgs []Message) {
	fmt.Print(pos(1, 1))
	for _, m := range msgs {
		if m.Role == "user" {
			fmt.Print(colorCyan)
			t.printLine("> " + m.Content)
			fmt.Print(colorReset)
		} else {
			t.printLine(m.Content)
			t.lines = append(t.lines, "") // blank separator
			fmt.Print("\r\n")
		}
	}
}

func (t *TUI) PrintUserMessage(msg string) {
	if t.scrollOffset != 0 {
		t.scrollOffset = 0
		t.redrawViewport()
	}
	fmt.Print(pos(t.histEnd(), 1))
	fmt.Print("\r\n")
	fmt.Print(colorCyan)
	t.printLine("> " + msg)
	fmt.Print(colorReset)
	t.drawFixed()
	fmt.Print(pos(t.inputRow(), 3))
}

func (t *TUI) BeginStream() {
	if t.scrollOffset != 0 {
		t.scrollOffset = 0
		t.redrawViewport()
		t.drawFixed()
	}
	// blank the input row so buffered keystrokes don't appear there
	fmt.Print(pos(t.inputRow(), 1) + clearLine)
	fmt.Print(pos(t.histEnd(), 1))
	fmt.Print("\r\n")
	t.col = 0
}

// WriteToken is passed directly as the onToken callback to provider.Stream.
func (t *TUI) WriteToken(token string) {
	for _, r := range token {
		if r == '\n' {
			t.newline()
			continue
		}
		t.writeRune(r)
	}
}

func (t *TUI) EndStream() {
	if t.col > 0 {
		t.newline()
	}
	t.lines = append(t.lines, "") // blank line after response
	fmt.Print("\r\n")
	t.drawFixed()
	fmt.Print(pos(t.inputRow(), 3))
	// discard anything typed during streaming
	if n := t.stdin.Buffered(); n > 0 {
		t.stdin.Discard(n)
	}
}

func (t *TUI) ReadLine() (string, error) {
	if t.oldState == nil {
		// raw mode unavailable -- fall back to line mode
		line, err := t.stdin.ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err
	}

	fmt.Print(pos(t.inputRow(), 1) + clearLine + colorCyan + "> " + colorReset)

	var buf []rune
	for {
		r, _, err := t.stdin.ReadRune()
		if err != nil {
			return "", io.EOF
		}

		switch r {
		case 13: // Enter
			fmt.Print("\r\n")
			return string(buf), nil

		case 127, 8: // Backspace
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				fmt.Print("\b \b")
			}

		case 5: // Ctrl+E -- open $EDITOR
			term.Restore(t.fd, t.oldState)
			content, editorErr := openInEditor(string(buf))
			if s, err := term.MakeRaw(t.fd); err == nil {
				t.oldState = s
			}
			if editorErr == nil {
				buf = []rune(strings.TrimRight(content, "\r\n"))
			}
			t.setup()
			t.redrawViewport()
			fmt.Print(pos(t.inputRow(), 3) + string(buf))

		case 21: // Ctrl+U -- scroll up half page
			half := t.histEnd() / 2
			t.scrollOffset += half
			t.redrawViewport()
			t.drawFixed()
			fmt.Print(pos(t.inputRow(), 3) + string(buf))

		case 4: // Ctrl+D -- scroll down half page
			half := t.histEnd() / 2
			t.scrollOffset -= half
			if t.scrollOffset < 0 {
				t.scrollOffset = 0
			}
			t.redrawViewport()
			t.drawFixed()
			fmt.Print(pos(t.inputRow(), 3) + string(buf))

		case 3: // Ctrl+C
			fmt.Print("\r\n")
			return "", io.EOF

		case 27: // ESC -- discard escape sequence (arrow keys etc.)
			next, _, _ := t.stdin.ReadRune()
			if next == '[' {
				t.stdin.ReadRune()
			}

		default:
			if r >= 32 {
				buf = append(buf, r)
				fmt.Print(string(r))
			}
		}
	}
}

func (t *TUI) Close() {
	if t.closed {
		return
	}
	t.closed = true
	if t.oldState != nil {
		term.Restore(t.fd, t.oldState)
	}
	fmt.Print(resetScroll)
	fmt.Print(pos(t.height, 1) + "\r\n")
}

func openInEditor(content string) (string, error) {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vim"
	}

	tmp, err := os.CreateTemp("", "cx-*.md")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.WriteString(content); err != nil {
		return "", err
	}
	tmp.Close()

	cmd := exec.Command(editor, tmp.Name())
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}

	data, err := os.ReadFile(tmp.Name())
	if err != nil {
		return "", err
	}
	return string(data), nil
}
