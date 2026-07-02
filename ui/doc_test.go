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

func TestHardWrap(t *testing.T) {
	long := strings.Repeat("x", 25)
	for _, ln := range strings.Split(hardWrap(long, 10), "\n") {
		if len(ln) > 10 {
			t.Errorf("line %q exceeds width", ln)
		}
	}
}
