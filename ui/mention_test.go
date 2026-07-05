package ui

import "testing"

func TestLastMentionToken(t *testing.T) {
	cases := []struct {
		in        string
		head, tok string
		ok        bool
	}{
		{"look at @not", "look at ", "not", true},
		{"@", "", "", true},                // just typed @
		{"@nts", "", "nts", true},          // at start
		{"plain text", "", "", false},      // no mention
		{"done @notes.md ", "", "", false}, // completed (trailing space)
		{"a@b.com", "", "", false},         // email, @ mid-token
	}
	for _, c := range cases {
		head, tok, ok := lastMentionToken(c.in)
		if ok != c.ok || head != c.head || tok != c.tok {
			t.Errorf("lastMentionToken(%q) = %q,%q,%v; want %q,%q,%v", c.in, head, tok, ok, c.head, c.tok, c.ok)
		}
	}
}

func TestExpandPastes(t *testing.T) {
	pastes := []pasteRef{
		{placeholder: "[paste #1, 3 lines]", text: "a\nb\nc"},
		{placeholder: "[paste #2, 2 lines]", text: "x\ny"},
	}
	in := "look at [paste #1, 3 lines] and [paste #2, 2 lines] please"
	got := expandPastes(in, pastes)
	want := "look at a\nb\nc and x\ny please"
	if got != want {
		t.Errorf("expandPastes = %q; want %q", got, want)
	}

	// user deleted part of a placeholder: leave it as literal text
	got = expandPastes("mangled [paste #1, 3 li", pastes[:1])
	if got != "mangled [paste #1, 3 li" {
		t.Errorf("mangled placeholder mutated: %q", got)
	}
}
