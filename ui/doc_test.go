package ui

import (
	"strings"
	"testing"
)

const sampleResponse = `I'd tighten the intro. Here's my suggestion:

<edit>
<<<<<<< SEARCH
# My Notes

Some rambling intro text.
=======
# My Notes

A crisp one-line intro.
>>>>>>> REPLACE
</edit>

And fix the typo:

<edit>
<<<<<<< SEARCH
teh cat
=======
the cat
>>>>>>> REPLACE
</edit>

That's all.`

func TestParseDocEdits(t *testing.T) {
	edits := parseDocEdits(sampleResponse)
	if len(edits) != 2 {
		t.Fatalf("parsed %d edits; want 2", len(edits))
	}
	if !strings.Contains(edits[0].search, "Some rambling intro text.") {
		t.Errorf("edit 0 search = %q", edits[0].search)
	}
	if !strings.Contains(edits[0].replace, "A crisp one-line intro.") {
		t.Errorf("edit 0 replace = %q", edits[0].replace)
	}
	if edits[1].search != "teh cat" || edits[1].replace != "the cat" {
		t.Errorf("edit 1 = %q -> %q", edits[1].search, edits[1].replace)
	}
}

func TestParseDocEditsMalformed(t *testing.T) {
	if edits := parseDocEdits("<edit>no markers</edit>"); len(edits) != 0 {
		t.Errorf("malformed block parsed as %d edits; want 0", len(edits))
	}
	if edits := parseDocEdits("no blocks at all"); len(edits) != 0 {
		t.Errorf("plain text parsed as %d edits; want 0", len(edits))
	}
	// Unclosed block must not loop forever or parse
	if edits := parseDocEdits("<edit><<<<<<< SEARCH\nx\n======="); len(edits) != 0 {
		t.Errorf("unclosed block parsed as %d edits; want 0", len(edits))
	}
}

func TestApplyEditTo(t *testing.T) {
	doc := "line one\nline two\nline three"
	out, ok := applyEditTo(doc, "line two", "line 2")
	if !ok || out != "line one\nline 2\nline three" {
		t.Errorf("applyEditTo = %q, %v", out, ok)
	}

	// No match
	_, ok = applyEditTo(doc, "not present", "x")
	if ok {
		t.Error("applyEditTo matched text that isn't there")
	}

	// Trailing-whitespace fallback
	docWS := "line one  \nline two\t\nline three"
	out, ok = applyEditTo(docWS, "line one\nline two", "merged")
	if !ok || !strings.Contains(out, "merged") {
		t.Errorf("whitespace fallback failed: %q, %v", out, ok)
	}
}

func TestStripEditBlocks(t *testing.T) {
	out := stripEditBlocks(sampleResponse)
	if strings.Contains(out, "<edit>") || strings.Contains(out, "SEARCH") {
		t.Errorf("edit blocks not stripped: %q", out)
	}
	if !strings.Contains(out, "[proposed edit 1") || !strings.Contains(out, "[proposed edit 2") {
		t.Errorf("placeholders missing: %q", out)
	}
	if !strings.Contains(out, "That's all.") {
		t.Error("surrounding prose lost")
	}
}

func TestSelectionParse(t *testing.T) {
	good := "/tmp/notes.md\n12-30\nselected line one\nselected line two\n"
	if sel := parseSelectionText(good); sel == nil ||
		sel.file != "/tmp/notes.md" || sel.start != 12 || sel.end != 30 ||
		!strings.Contains(sel.text, "selected line two") {
		t.Errorf("good selection parsed as %+v", sel)
	}

	for _, bad := range []string{"", "/tmp/x.md\n", "/tmp/x.md\nnot-a-range\ntext", "/tmp/x.md\n30-12\ntext"} {
		if sel := parseSelectionText(bad); sel != nil {
			t.Errorf("bad selection %q parsed as %+v", bad, sel)
		}
	}
}

func TestSummarizeReview(t *testing.T) {
	results := []editResult{
		{Applied: true},
		{Applied: false, Reason: "too wordy"},
		{Applied: false},
	}
	note := summarizeReview(results, "notes.md")
	if !strings.Contains(note, "1/3 applied") {
		t.Errorf("note = %q; want applied count", note)
	}
	if !strings.Contains(note, `edit 2 rejected — "too wordy"`) {
		t.Errorf("note = %q; want rejection reason", note)
	}
	if !strings.Contains(note, "edit 3 skipped") {
		t.Errorf("note = %q; want skip", note)
	}
}

func TestFuzzyMatch(t *testing.T) {
	cases := []struct {
		q, s string
		want bool
	}{
		{"", "anything", true},
		{"nts", "notes.md", true},          // subsequence
		{"docmd", "docs/readme.md", true},  // spans path segments
		{"xyz", "notes.md", false},
		{"nst", "notes.md", false},         // wrong order
		{"notes", "notes.md", true},
	}
	for _, c := range cases {
		if got := fuzzyMatch(c.q, c.s); got != c.want {
			t.Errorf("fuzzyMatch(%q, %q) = %v; want %v", c.q, c.s, got, c.want)
		}
	}
}

