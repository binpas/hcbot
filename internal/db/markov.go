package db

import (
	"database/sql"
	"strings"
)

// MarkovBlocklist returns every blocked word and phrase, alphabetically.
func (d *DB) MarkovBlocklist() ([]string, error) {
	return markovBlocklist(d.sql)
}

// AddMarkovBlock adds a blocked word or phrase. It reports false when it
// was already there.
func (d *DB) AddMarkovBlock(phrase, createdBy string) (bool, error) {
	res, err := d.sql.Exec(`INSERT OR IGNORE INTO markov_blocklist (phrase, created_by) VALUES (?, ?)`,
		strings.ToLower(phrase), createdBy)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RemoveMarkovBlock removes a blocked word or phrase. It reports false when
// it was not there.
func (d *DB) RemoveMarkovBlock(phrase string) (bool, error) {
	res, err := d.sql.Exec(`DELETE FROM markov_blocklist WHERE phrase=?`, strings.ToLower(phrase))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ReadMarkovBlocklist opens the database at path read-only and returns its
// blocklist, for the training script. A database without the table gives
// an empty list.
func ReadMarkovBlocklist(path string) ([]string, error) {
	conn, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	var name string
	err = conn.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='markov_blocklist'`).Scan(&name)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return markovBlocklist(conn)
}

func markovBlocklist(conn *sql.DB) ([]string, error) {
	rows, err := conn.Query(`SELECT phrase FROM markov_blocklist ORDER BY phrase`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
