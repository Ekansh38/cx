package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS conversations (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	title      TEXT    NOT NULL DEFAULT 'Untitled',
	model      TEXT    NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
	role            TEXT    NOT NULL,
	content         TEXT    NOT NULL,
	created_at      INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS conversation_docs (
	conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
	path            TEXT    NOT NULL,
	UNIQUE(conversation_id, path)
);

CREATE INDEX IF NOT EXISTS idx_messages_conv ON messages(conversation_id);
CREATE INDEX IF NOT EXISTS idx_conversations_updated ON conversations(updated_at DESC);
`

type Conversation struct {
	ID        int64
	Title     string
	Model     string
	CreatedAt int64
	UpdatedAt int64
}

type Message struct {
	ID        int64
	ConvID    int64
	Role      string
	Content   string
	ImagePath string
	CreatedAt int64
}

type Store struct {
	db *sql.DB
}

func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	// Migrations — safe to re-run (errors ignored if column already exists)
	db.Exec(`ALTER TABLE messages ADD COLUMN image_path TEXT NOT NULL DEFAULT ''`)
	// Move legacy single-doc attachments into conversation_docs (the doc_path
	// column only exists on older DBs; the error is ignored on fresh ones)
	db.Exec(`INSERT OR IGNORE INTO conversation_docs (conversation_id, path) SELECT id, doc_path FROM conversations WHERE doc_path != ''`)
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// CreateConversation inserts a new conversation and returns it.
func (s *Store) CreateConversation(model string) (*Conversation, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`INSERT INTO conversations (title, model, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		"Untitled", model, now, now,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Conversation{ID: id, Title: "Untitled", Model: model, CreatedAt: now, UpdatedAt: now}, nil
}

// GetConversation returns a single conversation by ID.
func (s *Store) GetConversation(id int64) (*Conversation, error) {
	row := s.db.QueryRow(`SELECT id, title, model, created_at, updated_at FROM conversations WHERE id = ?`, id)
	var c Conversation
	if err := row.Scan(&c.ID, &c.Title, &c.Model, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

// MostRecent returns the most recently updated conversation, or nil if none exist.
func (s *Store) MostRecent() (*Conversation, error) {
	row := s.db.QueryRow(`SELECT id, title, model, created_at, updated_at FROM conversations ORDER BY updated_at DESC LIMIT 1`)
	var c Conversation
	if err := row.Scan(&c.ID, &c.Title, &c.Model, &c.CreatedAt, &c.UpdatedAt); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListConversations returns all conversations, newest first.
func (s *Store) ListConversations() ([]*Conversation, error) {
	rows, err := s.db.Query(`SELECT id, title, model, created_at, updated_at FROM conversations ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var convs []*Conversation
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.Title, &c.Model, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		convs = append(convs, &c)
	}
	return convs, rows.Err()
}

// FindConversationByDoc returns the most recent conversation connected to a
// document path, or nil if none.
func (s *Store) FindConversationByDoc(path string) (*Conversation, error) {
	row := s.db.QueryRow(
		`SELECT c.id, c.title, c.model, c.created_at, c.updated_at FROM conversations c
		 JOIN conversation_docs d ON d.conversation_id = c.id
		 WHERE d.path = ? ORDER BY c.updated_at DESC, c.id DESC LIMIT 1`,
		path,
	)
	var c Conversation
	if err := row.Scan(&c.ID, &c.Title, &c.Model, &c.CreatedAt, &c.UpdatedAt); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetDocs returns the document paths connected to a conversation.
func (s *Store) GetDocs(convID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT path FROM conversation_docs WHERE conversation_id = ? ORDER BY rowid ASC`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// AddDoc connects a document to a conversation (no-op if already connected).
func (s *Store) AddDoc(convID int64, path string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO conversation_docs (conversation_id, path) VALUES (?, ?)`, convID, path)
	return err
}

// RemoveDoc disconnects a document from a conversation.
func (s *Store) RemoveDoc(convID int64, path string) error {
	_, err := s.db.Exec(`DELETE FROM conversation_docs WHERE conversation_id = ? AND path = ?`, convID, path)
	return err
}

// ClearDocs disconnects all documents from a conversation.
func (s *Store) ClearDocs(convID int64) error {
	_, err := s.db.Exec(`DELETE FROM conversation_docs WHERE conversation_id = ?`, convID)
	return err
}

// UpdateTitle sets the title of a conversation.
func (s *Store) UpdateTitle(id int64, title string) error {
	_, err := s.db.Exec(`UPDATE conversations SET title = ?, updated_at = ? WHERE id = ?`, title, time.Now().Unix(), id)
	return err
}

// UpdateModel sets the model of a conversation.
func (s *Store) UpdateModel(id int64, model string) error {
	_, err := s.db.Exec(`UPDATE conversations SET model = ?, updated_at = ? WHERE id = ?`, model, time.Now().Unix(), id)
	return err
}

// TouchConversation updates the updated_at timestamp.
func (s *Store) TouchConversation(id int64) error {
	_, err := s.db.Exec(`UPDATE conversations SET updated_at = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}

// GetMessages returns all messages for a conversation in order.
func (s *Store) GetMessages(convID int64) ([]*Message, error) {
	rows, err := s.db.Query(
		`SELECT id, conversation_id, role, content, image_path, created_at FROM messages WHERE conversation_id = ? ORDER BY created_at ASC, id ASC`,
		convID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []*Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ConvID, &m.Role, &m.Content, &m.ImagePath, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, &m)
	}
	return msgs, rows.Err()
}

// AddMessage inserts a message and updates the conversation timestamp.
func (s *Store) AddMessage(convID int64, role, content string) (*Message, error) {
	return s.AddMessageWithImage(convID, role, content, "")
}

// AddMessageWithImage inserts a message with an optional image path.
func (s *Store) AddMessageWithImage(convID int64, role, content, imagePath string) (*Message, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`INSERT INTO messages (conversation_id, role, content, image_path, created_at) VALUES (?, ?, ?, ?, ?)`,
		convID, role, content, imagePath, now,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	s.db.Exec(`UPDATE conversations SET updated_at = ? WHERE id = ?`, now, convID)
	return &Message{ID: id, ConvID: convID, Role: role, Content: content, ImagePath: imagePath, CreatedAt: now}, nil
}

// SearchMessages returns messages containing query across all conversations.
func (s *Store) SearchMessages(query string) ([]*Message, error) {
	rows, err := s.db.Query(
		`SELECT id, conversation_id, role, content, image_path, created_at FROM messages WHERE content LIKE ? ORDER BY created_at DESC LIMIT 2000`,
		"%"+query+"%",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []*Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ConvID, &m.Role, &m.Content, &m.ImagePath, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, &m)
	}
	return msgs, rows.Err()
}

// DeleteConversation removes a conversation and its messages (via ON DELETE CASCADE).
func (s *Store) DeleteConversation(id int64) error {
	_, err := s.db.Exec(`DELETE FROM conversations WHERE id = ?`, id)
	return err
}

// AddMessageAt inserts a message with an explicit timestamp. Used to position
// compaction summaries chronologically before the messages they precede.
func (s *Store) AddMessageAt(convID int64, role, content string, createdAt int64) (*Message, error) {
	res, err := s.db.Exec(
		`INSERT INTO messages (conversation_id, role, content, image_path, created_at) VALUES (?, ?, ?, '', ?)`,
		convID, role, content, createdAt,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Message{ID: id, ConvID: convID, Role: role, Content: content, CreatedAt: createdAt}, nil
}

// DeleteMessagesFrom removes a message and all messages after it in the conversation.
func (s *Store) DeleteMessagesFrom(convID, msgID int64) error {
	_, err := s.db.Exec(
		`DELETE FROM messages WHERE conversation_id = ? AND id >= ?`,
		convID, msgID,
	)
	return err
}

// WipeAll deletes every conversation and message from the database.
func (s *Store) WipeAll() error {
	_, err := s.db.Exec(`DELETE FROM conversations`)
	return err
}
