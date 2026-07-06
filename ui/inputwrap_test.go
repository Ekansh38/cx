package ui

import "testing"

func TestWrappedRowsMatchesTextareaSemantics(t *testing.T) {
	// single short line
	if r := wrappedRows("hello", 40); r != 1 {
		t.Errorf("short = %d", r)
	}
	// empty input still occupies one row
	if r := wrappedRows("", 40); r != 1 {
		t.Errorf("empty = %d", r)
	}
	// explicit newlines
	if r := wrappedRows("a\nb\nc", 40); r != 3 {
		t.Errorf("newlines = %d", r)
	}
	// a long word-wrapped sentence must be > 1 row at narrow width
	long := "the quick brown fox jumps over the lazy dog again and again and again"
	if r := wrappedRows(long, 20); r < 4 {
		t.Errorf("long = %d; want >= 4", r)
	}
	// growing input never shrinks the row count
	prev := 0
	s := ""
	for i := 0; i < 200; i++ {
		s += "word "
		r := wrappedRows(s, 30)
		if r < prev {
			t.Fatalf("rows shrank from %d to %d at len %d", prev, r, len(s))
		}
		prev = r
	}
}
