package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestDocsCRUD(t *testing.T) {
	st := testStore(t)
	conv, err := st.CreateConversation("m")
	if err != nil {
		t.Fatal(err)
	}

	if err := st.AddDoc(conv.ID, "/a.md"); err != nil {
		t.Fatal(err)
	}
	st.AddDoc(conv.ID, "/b.md")
	st.AddDoc(conv.ID, "/a.md") // duplicate: ignored

	docs, err := st.GetDocs(conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0] != "/a.md" || docs[1] != "/b.md" {
		t.Fatalf("GetDocs = %v; want [/a.md /b.md]", docs)
	}

	if err := st.RemoveDoc(conv.ID, "/a.md"); err != nil {
		t.Fatal(err)
	}
	docs, _ = st.GetDocs(conv.ID)
	if len(docs) != 1 || docs[0] != "/b.md" {
		t.Fatalf("after RemoveDoc: %v; want [/b.md]", docs)
	}

	st.AddDoc(conv.ID, "/c.md")
	if err := st.ClearDocs(conv.ID); err != nil {
		t.Fatal(err)
	}
	if docs, _ := st.GetDocs(conv.ID); len(docs) != 0 {
		t.Fatalf("after ClearDocs: %v; want empty", docs)
	}
}

func TestFindConversationByDoc(t *testing.T) {
	st := testStore(t)
	c1, _ := st.CreateConversation("m")
	c2, _ := st.CreateConversation("m")
	st.AddDoc(c1.ID, "/notes.md")
	st.AddDoc(c2.ID, "/notes.md")
	st.TouchConversation(c2.ID)

	found, err := st.FindConversationByDoc("/notes.md")
	if err != nil || found == nil {
		t.Fatalf("FindConversationByDoc = %v, %v", found, err)
	}
	if found.ID != c2.ID {
		t.Errorf("found conv %d; want most recent %d", found.ID, c2.ID)
	}

	none, err := st.FindConversationByDoc("/nope.md")
	if err != nil || none != nil {
		t.Errorf("unknown doc = %v, %v; want nil, nil", none, err)
	}
}

func TestDocsCascadeDelete(t *testing.T) {
	st := testStore(t)
	conv, _ := st.CreateConversation("m")
	st.AddDoc(conv.ID, "/a.md")
	if err := st.DeleteConversation(conv.ID); err != nil {
		t.Fatal(err)
	}
	if docs, _ := st.GetDocs(conv.ID); len(docs) != 0 {
		t.Fatalf("docs survived conversation delete: %v", docs)
	}
}

func TestForkConversation(t *testing.T) {
	st := testStore(t)
	src, _ := st.CreateConversation("m")
	m1, _ := st.AddMessage(src.ID, "user", "first question")
	st.AddMessage(src.ID, "assistant", "first answer")
	m3, _ := st.AddMessage(src.ID, "user", "second question")
	st.AddMessage(src.ID, "assistant", "second answer")
	st.AddDoc(src.ID, "/notes.md")

	fork, err := st.ForkConversation(src.ID, "m", m3.CreatedAt, m3.ID)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := st.GetMessages(fork.ID)
	if len(msgs) != 2 {
		t.Fatalf("fork has %d messages; want 2 (before the fork point)", len(msgs))
	}
	if msgs[0].Content != "first question" || msgs[1].Content != "first answer" {
		t.Errorf("fork content wrong: %q, %q", msgs[0].Content, msgs[1].Content)
	}
	if docs, _ := st.GetDocs(fork.ID); len(docs) != 1 || docs[0] != "/notes.md" {
		t.Errorf("fork docs = %v", docs)
	}
	// source untouched
	if srcMsgs, _ := st.GetMessages(src.ID); len(srcMsgs) != 4 {
		t.Errorf("source mutated: %d messages", len(srcMsgs))
	}
	_ = m1
}

func TestRecentSummaries(t *testing.T) {
	st := testStore(t)

	// Conv A: has a compaction summary.
	convA, _ := st.CreateConversation("m")
	st.UpdateTitle(convA.ID, "Project A")
	st.AddMessage(convA.ID, "user", "first prompt in A")
	st.AddMessage(convA.ID, "assistant", "reply")
	st.AddMessageAt(convA.ID, "summary", "Decided to use Go for the API. Discussed auth.", 0)
	st.AddMessage(convA.ID, "user", "next turn")

	// Conv B: no summary yet — should fall back to the first user message.
	convB, _ := st.CreateConversation("m")
	st.UpdateTitle(convB.ID, "Project B")
	st.AddMessage(convB.ID, "user", "explain how transformers work")

	// Conv C (the "current" one, excluded).
	convC, _ := st.CreateConversation("m")
	st.UpdateTitle(convC.ID, "Current")
	st.AddMessage(convC.ID, "user", "hi")

	// Touch A so it's the most recently updated (order matters for the query).
	st.TouchConversation(convA.ID)

	recaps, err := st.RecentSummaries(convC.ID, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(recaps) != 2 {
		t.Fatalf("want 2 recaps (A and B, excluding current C); got %d", len(recaps))
	}

	// A should be first (most recently updated) and carry its summary.
	a := recaps[0]
	if !strings.Contains(a.Content, "Project A") {
		t.Errorf("recap A missing title: %q", a.Content)
	}
	if !strings.Contains(a.Content, "Decided to use Go") {
		t.Errorf("recap A missing summary content: %q", a.Content)
	}

	// B falls back to its first user message.
	b := recaps[1]
	if !strings.Contains(b.Content, "Project B") {
		t.Errorf("recap B missing title: %q", b.Content)
	}
	if !strings.Contains(b.Content, "explain how transformers work") {
		t.Errorf("recap B missing fallback first-user content: %q", b.Content)
	}

	// Current conversation must be excluded.
	for _, r := range recaps {
		if r.ConvID == convC.ID {
			t.Error("current conversation leaked into recaps")
		}
	}

	// limit=0 is a no-op.
	if r, _ := st.RecentSummaries(convC.ID, 0); len(r) != 0 {
		t.Errorf("limit=0 should return nothing; got %d", len(r))
	}
}
