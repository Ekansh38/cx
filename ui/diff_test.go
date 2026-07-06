package ui

import (
	"strings"
	"testing"
)

func TestSplitEditHunksWholeDocRewrite(t *testing.T) {
	// The exact failure mode from real use: model rewrites the entire
	// document to fix two words and add lines at the end.
	doc := `# Title

PLAN:

Basic electricity and how gates work. Maybe using
transistors or an relay/electormagentic relay. whichever is easier.

So circuits plus boolean algebra.

Okay then some binary.

Then adders, RAM, and other parts.

Then instruction stuff and assembly.`

	newDoc := strings.ReplaceAll(doc, "electormagentic", "electromagnetic")
	newDoc = strings.ReplaceAll(newDoc, "whichever", "Whichever")
	newDoc += "\n\nTEST!\nTEST!\nTEST!"

	hunks := splitEditHunks(doc, newDoc, doc)
	if len(hunks) != 2 {
		t.Fatalf("got %d hunks; want 2 (typo line + tail addition)", len(hunks))
	}

	// Hunk 1: the typo line (both words are on the same line)
	if !strings.Contains(hunks[0][0], "electormagentic") ||
		!strings.Contains(hunks[0][1], "electromagnetic") {
		t.Errorf("hunk 0 = %q -> %q", hunks[0][0], hunks[0][1])
	}
	// A hunk must NOT span the whole document
	if strings.Contains(hunks[0][0], "PLAN:") && strings.Contains(hunks[0][0], "assembly.") {
		t.Error("hunk 0 spans the whole document")
	}

	// Hunk 2: the appended TEST lines
	if !strings.Contains(hunks[1][1], "TEST!") || strings.Contains(hunks[1][0], "TEST!") {
		t.Errorf("hunk 1 = %q -> %q", hunks[1][0], hunks[1][1])
	}

	// Applying all hunks sequentially must reproduce the full rewrite
	got := doc
	for _, h := range hunks {
		var ok bool
		got, ok = applyEditTo(got, h[0], h[1])
		if !ok {
			t.Fatalf("hunk %q did not locate", h[0])
		}
	}
	if got != newDoc {
		t.Errorf("applying split hunks diverged from the full rewrite:\n%q\nwant:\n%q", got, newDoc)
	}
}

func TestSplitEditHunksNoChange(t *testing.T) {
	if hunks := splitEditHunks("same\ntext", "same\ntext", "same\ntext"); hunks != nil {
		t.Errorf("no-op split into %v", hunks)
	}
}

func TestSplitEditHunksSmallEditPassesThrough(t *testing.T) {
	doc := "a\nb\nc\nd\ne"
	hunks := splitEditHunks("b\nc\nd", "b\nC\nd", doc)
	if len(hunks) != 1 {
		t.Fatalf("got %d hunks; want 1", len(hunks))
	}
	if !strings.Contains(hunks[0][0], "c") || !strings.Contains(hunks[0][1], "C") {
		t.Errorf("hunk = %v", hunks[0])
	}
}

func TestExplodeEditsDropsNoops(t *testing.T) {
	docs := []*attachedDoc{{path: "/d.md", content: "x\ny\nz"}}
	edits := []docEdit{
		{file: "/d.md", search: "y", replace: "y"},  // no-op: dropped
		{file: "/d.md", search: "y", replace: "Y"},  // kept
		{file: "/other", search: "a", replace: "b"}, // unknown doc: passthrough
	}
	out, noops := explodeEdits(edits, docs)
	if len(out) != 2 {
		t.Fatalf("got %d edits; want 2", len(out))
	}
	if noops != 1 {
		t.Errorf("noops = %d; want 1", noops)
	}

	// exact duplicates collapse to one
	dups := []docEdit{
		{file: "/d.md", search: "y", replace: "Y"},
		{file: "/d.md", search: "y", replace: "Y"},
	}
	out, _ = explodeEdits(dups, docs)
	if len(out) != 1 {
		t.Errorf("duplicates not collapsed: %d edits", len(out))
	}
}

func TestNormalizeEditsPhantomBlanks(t *testing.T) {
	doc := "# Title\n\nbody text\n\nlast line"
	docs := []*attachedDoc{{path: "/d.md", content: doc}}

	edits := []docEdit{
		// bottom anchor with phantom trailing blank (from the file's newline)
		{file: "/d.md", search: "last line\n", replace: "last line\n\ntest\ntest"},
		// top anchor with phantom leading blank
		{file: "/d.md", search: "\n# Title", replace: "test\n\n# Title"},
	}
	out := normalizeEdits(edits, docs)
	if out[0].search != "last line" {
		t.Errorf("trailing blank not trimmed: %q", out[0].search)
	}
	if out[1].search != "# Title" {
		t.Errorf("leading blank not trimmed: %q", out[1].search)
	}
	// replace text stays intact so the intended blank lines still land
	if out[0].replace != "last line\n\ntest\ntest" {
		t.Errorf("replace mutated: %q", out[0].replace)
	}
	// matching searches are left alone
	same := normalizeEdits([]docEdit{{file: "/d.md", search: "body text", replace: "x"}}, docs)
	if same[0].search != "body text" {
		t.Errorf("matching search mutated: %q", same[0].search)
	}
}

func TestPatienceSplitsAroundAnchors(t *testing.T) {
	// Whole-section rewrite between two stable heading lines: patience runs
	// tightly bound the change to the actual body (no headings swallowed).
	before := "# Title\n\n## Section A\n\nalpha\nbeta\ngamma\n\n## Section B\n\nmore stuff\n"
	after := "# Title\n\n## Section A\n\nALPHA\nBETA\nGAMMA\nDELTA\n\n## Section B\n\nmore stuff\n"

	runs := patienceRuns(strings.Split(before, "\n"), strings.Split(after, "\n"))
	if len(runs) != 1 {
		t.Fatalf("got %d runs; want 1", len(runs))
	}
	r := runs[0]
	// The run spans the body only, not the surrounding headings
	body := strings.Split(before, "\n")[r.aFrom:r.aTo]
	joined := strings.Join(body, "\n")
	if strings.Contains(joined, "Section") {
		t.Errorf("patience run swallowed a heading: %q", joined)
	}
}
