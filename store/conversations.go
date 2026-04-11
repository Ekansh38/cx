package store

import (
	"database/sql"
	"errors"
	"time"
)

func (s *Store) CreateConversation(model string) (*Conversation, error) {
	now := time.Now().Unix()

	res, err := s.db.Exec(
		`INSERT INTO conversations (title, model, pinned, created_at, updated_at) VALUES (?, ?, 0, ?, ?)`,
		"New conversation", model, now, now,
	)
	if err != nil {
		return nil, err
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	return &Conversation{
		ID:        id,
		Title:     "New conversation",
		Model:     model,
		Pinned:    false,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

func (s *Store) GetConversation(id int64) (*Conversation, error) {
	var c Conversation
	err := s.db.Get(&c,
		`SELECT id, title, model, pinned, summary, message_count, created_at, updated_at FROM conversations WHERE id = ?`,
		id,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

func (s *Store) ListConversations() ([]*Conversation, error) {
	var convs []*Conversation
	err := s.db.Select(&convs,
		`SELECT id, title, model, pinned, summary, message_count, created_at, updated_at FROM conversations ORDER BY pinned DESC, updated_at DESC`,
	)
	return convs, err
}

func (s *Store) UpdateSummary(convID int64, summary string, msgCount int) error {
	_, err := s.db.Exec(
		`UPDATE conversations SET summary = ?, message_count = ?, updated_at = ? WHERE id = ?`,
		summary, msgCount, time.Now().Unix(), convID,
	)
	return err
}

// GetSummaries returns conversations that have a summary, most recent first.
func (s *Store) GetSummaries(limit int) ([]*Conversation, error) {
	var convs []*Conversation
	err := s.db.Select(&convs,
		`SELECT id, title, model, pinned, summary, message_count, created_at, updated_at
		 FROM conversations WHERE summary != '' ORDER BY updated_at DESC LIMIT ?`,
		limit,
	)
	return convs, err
}
