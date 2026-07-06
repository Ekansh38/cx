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
