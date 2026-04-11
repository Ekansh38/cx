package store

import (
	"time"
)

func (s *Store) AddMessage(convID int64, role, content string) (*Message, error) {
	now := time.Now().Unix()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT INTO messages (conversation_id, role, content, created_at) VALUES (?, ?, ?, ?)`,
		convID, role, content, now,
	)
	if err != nil {
		return nil, err
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(
		`UPDATE conversations SET updated_at = ? WHERE id = ?`,
		now, convID,
	); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &Message{
		ID:             id,
		ConversationID: convID,
		Role:           role,
		Content:        content,
		CreatedAt:      now,
	}, nil
}

func (s *Store) GetMessages(convID int64) ([]*Message, error) {
	var msgs []*Message
	err := s.db.Select(&msgs,
		`SELECT id, conversation_id, role, content, token_count, created_at FROM messages WHERE conversation_id = ? ORDER BY created_at ASC`,
		convID,
	)
	return msgs, err
}
