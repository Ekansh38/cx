package ui

// Line-level diffing to explode model-proposed edits into minimal hunks.
// Models love rewriting whole documents even when asked for a typo fix; a
// whole-doc SEARCH/REPLACE makes review useless (everything red, everything
// green, all-or-nothing). splitEditHunks diffs search vs replace and returns
// one small hunk per contiguous change, each with just enough context to be
// located unambiguously in the document.

import "strings"

const (
	hunkCtx      = 2         // context lines around each change
	maxHunkCtx   = 8         // uniqueness expansion cap
	maxDiffCells = 4_000_000 // LCS table guard for huge inputs
)

type lineOp struct {
	kind byte // '=', '-', '+'
}

// lineDiff returns the op sequence transforming a into b (LCS-based).
func lineDiff(a, b []string) []lineOp {
	n, m := len(a), len(b)
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	var ops []lineOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, lineOp{'='})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, lineOp{'-'})
			i++
		default:
			ops = append(ops, lineOp{'+'})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, lineOp{'-'})
	}
	for ; j < m; j++ {
		ops = append(ops, lineOp{'+'})
	}
	return ops
}

// changeRun is one contiguous changed region, as ranges into a and b.
type changeRun struct {
	aFrom, aTo int // deleted lines a[aFrom:aTo]
	bFrom, bTo int // inserted lines b[bFrom:bTo]
}

// changeRuns walks the ops and merges changes separated by small equal gaps.
func changeRuns(ops []lineOp) []changeRun {
	var runs []changeRun
	ai, bi := 0, 0
	inRun := false
	var cur changeRun
	equalGap := 0

	flush := func() {
		if inRun {
			runs = append(runs, cur)
			inRun = false
		}
	}
	for _, op := range ops {
		switch op.kind {
		case '=':
			if inRun {
				equalGap++
				if equalGap > hunkCtx*2 {
					flush()
				}
			}
			ai++
			bi++
		default:
			if inRun && equalGap > 0 {
				// still within merge distance: extend the run over the gap
				equalGap = 0
			}
			if !inRun {
				inRun = true
				cur = changeRun{aFrom: ai, aTo: ai, bFrom: bi, bTo: bi}
				equalGap = 0
			}
			if op.kind == '-' {
				ai++
			} else {
				bi++
			}
			cur.aTo, cur.bTo = ai, bi
		}
	}
	flush()
	return runs
}

// splitEditHunks explodes one search/replace pair into minimal (search,
// replace) hunks. docContent is used to grow context until each hunk locates
// uniquely. Returns nil when splitting isn't possible or worthwhile.
func splitEditHunks(search, replace, docContent string) [][2]string {
	a := strings.Split(search, "\n")
	b := strings.Split(replace, "\n")
	if len(a)*len(b) > maxDiffCells {
		return nil
	}
	runs := changeRuns(lineDiff(a, b))
	if len(runs) == 0 {
		return nil // no actual change
	}

	var out [][2]string
	for i, r := range runs {
		// Context must stay within the equal lines between neighboring runs;
		// crossing into another run's changed lines breaks the a/b alignment.
		loBound := 0
		if i > 0 {
			loBound = runs[i-1].aTo
		}
		hiBound := len(a)
		if i+1 < len(runs) {
			hiBound = runs[i+1].aFrom
		}
		for ctx := hunkCtx; ; ctx++ {
			aStart := max(r.aFrom-ctx, loBound)
			aEnd := min(r.aTo+ctx, hiBound)
			bStart := r.bFrom - (r.aFrom - aStart)
			bEnd := r.bTo + (aEnd - r.aTo)
			hs := strings.Join(a[aStart:aEnd], "\n")
			hr := strings.Join(b[bStart:bEnd], "\n")
			unique := strings.Count(docContent, hs) <= 1
			atCap := ctx >= maxHunkCtx || (aStart == loBound && aEnd == hiBound)
			if unique || atCap {
				if hs != hr {
					out = append(out, [2]string{hs, hr})
				}
				break
			}
		}
	}
	return out
}

// normalizeEdits trims phantom leading/trailing empty lines from a SEARCH
// that doesn't match its document. Models anchoring on the start or end of a
// file often include a blank line that only exists as the trailing newline,
// which would otherwise fail to locate. The replace text is left intact, so
// intended blank lines still land.
func normalizeEdits(edits []docEdit, docs []*attachedDoc) []docEdit {
	content := map[string]string{}
	for _, d := range docs {
		content[d.path] = d.content
	}
	for i := range edits {
		doc, ok := content[edits[i].file]
		if !ok || strings.Contains(doc, edits[i].search) {
			continue
		}
		lines := strings.Split(edits[i].search, "\n")
		for len(lines) > 1 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
			if strings.Contains(doc, strings.Join(lines, "\n")) {
				break
			}
		}
		for len(lines) > 1 && lines[0] == "" {
			lines = lines[1:]
			if strings.Contains(doc, strings.Join(lines, "\n")) {
				break
			}
		}
		if cand := strings.Join(lines, "\n"); strings.Contains(doc, cand) {
			edits[i].search = cand
		}
	}
	return edits
}

// deChainEdits re-anchors edits whose SEARCH assumes earlier edits were
// already applied (models chain edits like that). Substituting prior
// replacements back out lets every hunk match the real document and be
// reviewed independently.
func deChainEdits(edits []docEdit, docs []*attachedDoc) []docEdit {
	content := map[string]string{}
	for _, d := range docs {
		content[d.path] = d.content
	}
	for i := range edits {
		doc, ok := content[edits[i].file]
		if !ok || strings.Contains(doc, edits[i].search) {
			continue
		}
		candidate := edits[i].search
		for j := 0; j < i; j++ {
			if edits[j].file != edits[i].file || edits[j].replace == "" {
				continue
			}
			candidate = strings.ReplaceAll(candidate, edits[j].replace, edits[j].search)
		}
		if candidate != edits[i].search && strings.Contains(doc, candidate) {
			edits[i].search = candidate
		}
	}
	return edits
}

// explodeEdits splits every edit into minimal hunks against its target doc.
// Edits that can't be split (unknown doc, no-op, oversized) pass through,
// except pure no-ops which are dropped.
func explodeEdits(edits []docEdit, docs []*attachedDoc) []docEdit {
	var out []docEdit
	for _, e := range edits {
		if e.search == e.replace {
			continue
		}
		var doc *attachedDoc
		for _, d := range docs {
			if d.path == e.file {
				doc = d
				break
			}
		}
		if doc == nil {
			out = append(out, e)
			continue
		}
		hunks := splitEditHunks(e.search, e.replace, doc.content)
		if hunks == nil {
			out = append(out, e)
			continue
		}
		for _, h := range hunks {
			out = append(out, docEdit{file: e.file, search: h[0], replace: h[1]})
		}
	}
	return out
}
