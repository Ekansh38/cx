package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

// Drive a real textarea with cx's exact height logic and assert the first
// line is always visible while typing across wrap boundaries.
func TestInputBoxShowsAllRowsWhileTyping(t *testing.T) {
	ta := textarea.New()
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = 12
	ta.SetWidth(30)
	ta.SetHeight(1)
	ta.Focus()

	text := "aaaa bbbb cccc dddd eeee ffff gggg hhhh iiii jjjj kkkk llll"
	for i, r := range text {
		ta, _ = ta.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		// cx's syncInputHeight logic (spare row prevents internal scroll):
		h := min(wrappedRows(ta.Value(), ta.Width())+1, 12)
		if h != ta.Height() {
			ta.SetHeight(h)
		}
		view := ta.View()
		if i >= 4 && !strings.Contains(view, "aaaa") {
			t.Fatalf("first line hidden after %d chars (height=%d, width=%d, wrappedRows=%d):\n%q",
				i+1, ta.Height(), ta.Width(), wrappedRows(ta.Value(), ta.Width()), view)
		}
	}
}

// A long multi-line value must never be truncated by the textarea: MaxHeight
// caps input lines when set, which once ate an editor draft (only the first
// 12 lines survived). MaxHeight=0 + our own display cap prevents it.
func TestLongValueNotTruncated(t *testing.T) {
	ta := textarea.New()
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.SetWidth(60)
	ta.SetHeight(1)
	ta.Focus()

	var lines []string
	for i := 0; i < 80; i++ {
		lines = append(lines, "line of a very long prompt")
	}
	content := strings.Join(lines, "\n")
	ta.SetValue(content)
	if ta.Value() != content {
		t.Fatalf("textarea truncated: kept %d of %d lines",
			strings.Count(ta.Value(), "\n")+1, 80)
	}
	// display stays capped by cx's own logic
	if h := min(wrappedRows(ta.Value(), ta.Width())+1, 12); h != 12 {
		t.Errorf("display cap = %d; want 12", h)
	}
}
