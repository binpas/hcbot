package markov

import (
	"database/sql"
	"errors"
)

// Item is one message for LearnOnce: its Discord ID and its words.
type Item struct {
	ID    string
	Words []string
}

// LearnOnce adds the messages whose IDs the chain has not learned yet, in
// one transaction, and remembers their IDs. It returns how many it added.
// Thus the same message can come from a search and from the live chat, and
// the chain learns it only once.
func (s *Store) LearnOnce(items []Item) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	mark, err := tx.Prepare(`INSERT INTO learned (id) VALUES (?) ON CONFLICT (id) DO NOTHING`)
	if err != nil {
		return 0, err
	}
	var fresh [][]string
	for _, it := range items {
		res, err := mark.Exec(it.ID)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 1 && len(it.Words) > 0 {
			fresh = append(fresh, it.Words)
		}
	}
	if err := learnTx(tx, fresh); err != nil {
		return 0, err
	}
	return len(fresh), tx.Commit()
}

// Learned returns how many message IDs LearnOnce has learned.
func (s *Store) Learned() (int64, error) {
	var n int64
	err := s.sql.QueryRow(`SELECT count(*) FROM learned`).Scan(&n)
	return n, err
}

// Meta returns a stored value, or nil when there is none.
func (s *Store) Meta(key string) ([]byte, error) {
	var v []byte
	err := s.sql.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return v, err
}

// SetMeta stores a value. A nil value removes it.
func (s *Store) SetMeta(key string, value []byte) error {
	if value == nil {
		_, err := s.sql.Exec(`DELETE FROM meta WHERE key=?`, key)
		return err
	}
	_, err := s.sql.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
