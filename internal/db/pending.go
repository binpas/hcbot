package db

import (
	"database/sql"
	"time"
)

// SavePending stores a button's data until expires and returns its ID.
func (d *DB) SavePending(kind, userID, payload string, expires time.Time) (int64, error) {
	res, err := d.sql.Exec(`INSERT INTO pending_actions (kind, user_id, payload, expires_at) VALUES (?, ?, ?, ?)`,
		kind, userID, payload, expires.UTC().Format(SentenceTimeFormat))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// TakePending returns and deletes a pending row of the given kind and
// user. ok is false when it doesn't exist or has expired.
func (d *DB) TakePending(id int64, kind, userID string) (payload string, ok bool, err error) {
	err = d.sql.QueryRow(`SELECT payload FROM pending_actions
		WHERE id=? AND kind=? AND user_id=? AND datetime(expires_at) > datetime('now')`,
		id, kind, userID).Scan(&payload)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	_, err = d.sql.Exec(`DELETE FROM pending_actions WHERE id=?`, id)
	return payload, err == nil, err
}

// DeleteExpiredPending removes expired pending rows.
func (d *DB) DeleteExpiredPending() error {
	_, err := d.sql.Exec(`DELETE FROM pending_actions WHERE datetime(expires_at) <= datetime('now')`)
	return err
}

// LoadPending returns a pending row without deleting it (for a draft that
// changes over several clicks). ok is false when it doesn't exist or has expired.
func (d *DB) LoadPending(id int64, kind, userID string) (payload string, ok bool, err error) {
	err = d.sql.QueryRow(`SELECT payload FROM pending_actions
		WHERE id=? AND kind=? AND user_id=? AND datetime(expires_at) > datetime('now')`,
		id, kind, userID).Scan(&payload)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return payload, err == nil, err
}

// UpdatePending replaces a pending row's payload and extends its expiry.
func (d *DB) UpdatePending(id int64, payload string, expires time.Time) error {
	_, err := d.sql.Exec(`UPDATE pending_actions SET payload=?, expires_at=? WHERE id=?`,
		payload, expires.UTC().Format(SentenceTimeFormat), id)
	return err
}

// DeletePending removes a pending row.
func (d *DB) DeletePending(id int64) error {
	_, err := d.sql.Exec(`DELETE FROM pending_actions WHERE id=?`, id)
	return err
}
