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
