package store

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Conversation struct {
	ID           int64  `db:"id"`
	Title        string `db:"title"`
	Model        string `db:"model"`
	Pinned       bool   `db:"pinned"`
	Summary      string `db:"summary"`
	MessageCount int    `db:"message_count"`
	CreatedAt    int64  `db:"created_at"`
	UpdatedAt    int64  `db:"updated_at"`
}

type Message struct {
	ID             int64  `db:"id"`
	ConversationID int64  `db:"conversation_id"`
	Role           string `db:"role"`
	Content        string `db:"content"`
	TokenCount     *int64 `db:"token_count"`
	CreatedAt      int64  `db:"created_at"`
}

type Store struct {
	db *sqlx.DB
}

func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}

	db, err := sqlx.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, err
	}

	if err := createSchema(db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func createSchema(db *sqlx.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS conversations (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			title         TEXT    NOT NULL DEFAULT 'New conversation',
			model         TEXT    NOT NULL,
			pinned        INTEGER NOT NULL DEFAULT 0,
			summary       TEXT    NOT NULL DEFAULT '',
			message_count INTEGER NOT NULL DEFAULT 0,
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL
		)
	`)
	if err != nil {
		return err
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS messages (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			conversation_id INTEGER NOT NULL,
			role            TEXT    NOT NULL,
			content         TEXT    NOT NULL,
			token_count     INTEGER,
			created_at      INTEGER NOT NULL,
			FOREIGN KEY (conversation_id) REFERENCES conversations(id)
		)
	`)
	return err
}
