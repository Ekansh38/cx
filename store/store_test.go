package store

import (
	"path/filepath"
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
