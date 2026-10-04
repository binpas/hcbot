// Package markov is the word chain behind the bot's chat replies. It lives
// in its own SQLite file (markov.db), apart from the main database, so the
// whole chain can be deleted and started again without touching server data.
//
// The chain is order 2: each row says how often the pair (w1, w2) was
// followed by next. Words are stored once and referred to by ID. ID 0 marks
// the start and the end of a message.
package markov

import (
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// boundary is the word ID for the start and the end of a message.
const boundary = 0

// Store is a handle to the chain database.
type Store struct {
	sql  *sql.DB
	Path string
}

// Open opens (creating if needed) the chain database at path. WAL mode lets
// the training script write while the bot runs.
func Open(path string) (*Store, error) {
	conn, err := sql.Open("sqlite", "file:"+path+
		"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	s := &Store{sql: conn, Path: path}
	for _, stmt := range schema {
		if _, err := conn.Exec(stmt); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("init markov schema: %w", err)
		}
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.sql.Close()
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS words (
		id   INTEGER PRIMARY KEY,
		word TEXT NOT NULL UNIQUE
	)`,
	`CREATE TABLE IF NOT EXISTS transitions (
		w1    INTEGER NOT NULL,
		w2    INTEGER NOT NULL,
		next  INTEGER NOT NULL,
		count INTEGER NOT NULL,
		PRIMARY KEY (w1, w2, next)
	) WITHOUT ROWID`,
}

// Learn adds each message (already tokenized) to the chain, in one
// transaction.
func (s *Store) Learn(messages [][]string) error {
	if len(messages) == 0 {
		return nil
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	addWord, err := tx.Prepare(`INSERT INTO words (word) VALUES (?) ON CONFLICT (word) DO NOTHING`)
	if err != nil {
		return err
	}
	getWord, err := tx.Prepare(`SELECT id FROM words WHERE word=?`)
	if err != nil {
		return err
	}
	addStep, err := tx.Prepare(`INSERT INTO transitions (w1, w2, next, count) VALUES (?, ?, ?, 1)
		ON CONFLICT (w1, w2, next) DO UPDATE SET count = count + 1`)
	if err != nil {
		return err
	}
	ids := map[string]int64{}
	wordID := func(w string) (int64, error) {
		if id, ok := ids[w]; ok {
			return id, nil
		}
		if _, err := addWord.Exec(w); err != nil {
			return 0, err
		}
		var id int64
		if err := getWord.QueryRow(w).Scan(&id); err != nil {
			return 0, err
		}
		ids[w] = id
		return id, nil
	}
	for _, words := range messages {
		if len(words) == 0 {
			continue
		}
		w1, w2 := int64(boundary), int64(boundary)
		for _, w := range words {
			id, err := wordID(w)
			if err != nil {
				return err
			}
			if _, err := addStep.Exec(w1, w2, id); err != nil {
				return err
			}
			w1, w2 = w2, id
		}
		if _, err := addStep.Exec(w1, w2, boundary); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// maxWords is the longest reply Generate makes.
const maxWords = 40

// Generate makes a reply. It starts from one of the seed words when the
// chain knows one, or from the start of a message otherwise. It returns ""
// when the chain is empty or every try is refused by ok.
func (s *Store) Generate(seed []string, ok func([]string) bool) (string, error) {
	var starts []int64
	for _, w := range seed {
		var id int64
		err := s.sql.QueryRow(`SELECT id FROM words WHERE word=?`, w).Scan(&id)
		if err == nil {
			var n int
			_ = s.sql.QueryRow(`SELECT COUNT(*) FROM (SELECT 1 FROM transitions WHERE w1=? LIMIT 1)`, id).Scan(&n)
			if n > 0 {
				starts = append(starts, id)
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	for try := 0; try < 5; try++ {
		var ids []int64
		var err error
		if len(starts) > 0 {
			ids, err = s.walkFrom(starts[rand.IntN(len(starts))])
		} else {
			ids, err = s.walk(boundary, boundary)
		}
		if err != nil {
			return "", err
		}
		if len(ids) == 0 {
			return "", nil // empty chain
		}
		words, err := s.words(ids)
		if err != nil {
			return "", err
		}
		if ok == nil || ok(words) {
			return strings.Join(words, " "), nil
		}
	}
	return "", nil
}

// walkFrom starts a walk with the word start as the first word.
func (s *Store) walkFrom(start int64) ([]int64, error) {
	w2, err := s.pick(`SELECT w2, SUM(count) FROM transitions WHERE w1=? GROUP BY w2`, start)
	if err != nil || w2 < 0 {
		return nil, err
	}
	rest, err := s.walk(start, w2)
	return append([]int64{start, w2}, rest...), err
}

// walk follows the chain from the pair (w1, w2) to the end of a message.
func (s *Store) walk(w1, w2 int64) ([]int64, error) {
	var out []int64
	for len(out) < maxWords {
		next, err := s.pick(`SELECT next, count FROM transitions WHERE w1=? AND w2=?`, w1, w2)
		if err != nil {
			return nil, err
		}
		if next <= boundary {
			break
		}
		out = append(out, next)
		w1, w2 = w2, next
	}
	return out, nil
}

// pick runs a query of (id, weight) rows and picks one id by weight. It
// returns -1 when there are no rows.
func (s *Store) pick(query string, args ...any) (int64, error) {
	rows, err := s.sql.Query(query, args...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	type option struct{ id, weight int64 }
	var opts []option
	var total int64
	for rows.Next() {
		var o option
		if err := rows.Scan(&o.id, &o.weight); err != nil {
			return 0, err
		}
		opts = append(opts, o)
		total += o.weight
	}
	if err := rows.Err(); err != nil || total == 0 {
		return -1, err
	}
	r := rand.Int64N(total)
	for _, o := range opts {
		if r < o.weight {
			return o.id, nil
		}
		r -= o.weight
	}
	return opts[len(opts)-1].id, nil
}

func (s *Store) words(ids []int64) ([]string, error) {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		var w string
		if err := s.sql.QueryRow(`SELECT word FROM words WHERE id=?`, id).Scan(&w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

// Stats is the size of the chain.
type Stats struct {
	Words       int64
	Transitions int64
	Bytes       int64 // database file plus its WAL
}

// Stats counts the words and transitions.
func (s *Store) Stats() (Stats, error) {
	var st Stats
	if err := s.sql.QueryRow(`SELECT COUNT(*) FROM words`).Scan(&st.Words); err != nil {
		return st, err
	}
	if err := s.sql.QueryRow(`SELECT COUNT(*) FROM transitions`).Scan(&st.Transitions); err != nil {
		return st, err
	}
	for _, p := range []string{s.Path, s.Path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			st.Bytes += fi.Size()
		}
	}
	return st, nil
}

// matchIDs returns the IDs of every stored word that matches w after
// Normalize ("cat" matches "Cat", "cat!" and "cat,"). A prefix such as
// "cat*" also matches "cats" and "catch".
func (s *Store) matchIDs(w string) ([]int64, error) {
	rows, err := s.sql.Query(`SELECT id, word FROM words`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		var word string
		if err := rows.Scan(&id, &word); err != nil {
			return nil, err
		}
		if wordMatches(w, Normalize(word)) {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// forgetWhere builds the condition that selects the transitions made by
// phrase: for one word, every transition that has it; for more words, each
// step inside the phrase. It returns "" when a word is not in the chain.
// The IDs come from the database, so they are safe to put in the SQL.
func (s *Store) forgetWhere(phrase []string) (string, error) {
	sets := make([]string, len(phrase))
	for i, w := range phrase {
		ids, err := s.matchIDs(w)
		if err != nil || len(ids) == 0 {
			return "", err
		}
		parts := make([]string, len(ids))
		for j, id := range ids {
			parts[j] = fmt.Sprint(id)
		}
		sets[i] = "(" + strings.Join(parts, ",") + ")"
	}
	switch len(phrase) {
	case 0:
		return "", nil
	case 1:
		in := sets[0]
		return fmt.Sprintf("w1 IN %s OR w2 IN %s OR next IN %s", in, in, in), nil
	case 2:
		return fmt.Sprintf("(w1 IN %[1]s AND w2 IN %[2]s) OR (w2 IN %[1]s AND next IN %[2]s)", sets[0], sets[1]), nil
	}
	var conds []string
	for i := 0; i+2 < len(sets); i++ {
		conds = append(conds, fmt.Sprintf("(w1 IN %s AND w2 IN %s AND next IN %s)", sets[i], sets[i+1], sets[i+2]))
	}
	return strings.Join(conds, " OR "), nil
}

// Form is one stored form of a word and how often the chain has seen it.
type Form struct {
	Word string
	Seen int64
}

// WordInfo is what LookupWord finds.
type WordInfo struct {
	Forms  []Form   // the stored forms that match, most seen first
	Total  int64    // how many forms match; Forms has only the first few
	Before []string // the words seen most often just before it
	After  []string // the words seen most often just after it
}

// lookupLimit is the most forms, and words before and after, LookupWord
// returns.
const lookupLimit = 10

// LookupWord finds the stored forms of a normalized word (see matchIDs). A
// form counts each time a learned message used it, so a form that forget
// or prune removed from every step is not shown.
func (s *Store) LookupWord(w string) (WordInfo, error) {
	var info WordInfo
	ids, err := s.matchIDs(w)
	if err != nil || len(ids) == 0 {
		return info, err
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	in := "(" + strings.Join(parts, ",") + ")"
	// The IDs come from the database, so they are safe to put in the SQL.
	rows, err := s.sql.Query(`SELECT w.word, SUM(t.count) FROM transitions t JOIN words w ON w.id = t.next
		WHERE t.next IN `+in+` GROUP BY t.next ORDER BY 2 DESC, 1 LIMIT ?`, lookupLimit)
	if err != nil {
		return info, err
	}
	for rows.Next() {
		var f Form
		if err := rows.Scan(&f.Word, &f.Seen); err != nil {
			_ = rows.Close()
			return info, err
		}
		info.Forms = append(info.Forms, f)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(info.Forms) == 0 {
		return info, err
	}
	if err := s.sql.QueryRow(`SELECT COUNT(DISTINCT next) FROM transitions WHERE next IN ` + in).Scan(&info.Total); err != nil {
		return info, err
	}
	if info.Before, err = s.topWords(`SELECT w.word, SUM(t.count) FROM transitions t JOIN words w ON w.id = t.w2
		WHERE t.next IN ` + in + ` AND t.w2 != 0 GROUP BY t.w2 ORDER BY 2 DESC, 1 LIMIT ?`); err != nil {
		return info, err
	}
	info.After, err = s.topWords(`SELECT w.word, SUM(t.count) FROM transitions t JOIN words w ON w.id = t.next
		WHERE t.w2 IN ` + in + ` AND t.next != 0 GROUP BY t.next ORDER BY 2 DESC, 1 LIMIT ?`)
	return info, err
}

// topWords runs a query of (word, weight) rows and returns the words.
func (s *Store) topWords(query string) ([]string, error) {
	rows, err := s.sql.Query(query, lookupLimit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var w string
		var n int64
		if err := rows.Scan(&w, &n); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// CountPhrase counts the transitions Forget would remove.
func (s *Store) CountPhrase(phrase []string) (int64, error) {
	where, err := s.forgetWhere(phrase)
	if err != nil || where == "" {
		return 0, err
	}
	var n int64
	err = s.sql.QueryRow(`SELECT COUNT(*) FROM transitions WHERE ` + where).Scan(&n)
	return n, err
}

// Forget removes the transitions made by phrase (see forgetWhere) and
// returns how many it removed.
func (s *Store) Forget(phrase []string) (int64, error) {
	where, err := s.forgetWhere(phrase)
	if err != nil || where == "" {
		return 0, err
	}
	res, err := s.sql.Exec(`DELETE FROM transitions WHERE ` + where)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Prune removes the transitions seen only once, then the words no
// transition uses, and makes the file smaller. It returns how many
// transitions it removed.
func (s *Store) Prune() (int64, error) {
	res, err := s.sql.Exec(`DELETE FROM transitions WHERE count = 1`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := s.compact(); err != nil {
		return n, err
	}
	return n, nil
}

// Reset removes the whole chain.
func (s *Store) Reset() error {
	if _, err := s.sql.Exec(`DELETE FROM transitions`); err != nil {
		return err
	}
	if _, err := s.sql.Exec(`DELETE FROM words`); err != nil {
		return err
	}
	return s.vacuum()
}

func (s *Store) compact() error {
	if _, err := s.sql.Exec(`DELETE FROM words WHERE id NOT IN
		(SELECT w1 FROM transitions UNION SELECT w2 FROM transitions UNION SELECT next FROM transitions)`); err != nil {
		return err
	}
	return s.vacuum()
}

func (s *Store) vacuum() error {
	if _, err := s.sql.Exec(`VACUUM`); err != nil {
		return err
	}
	_, err := s.sql.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}
