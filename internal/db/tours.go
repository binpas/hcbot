package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
)

// Tour is one row of the tours table.
type Tour struct {
	ID            int64
	GuildID       string
	Name          string
	ForumID       string
	TitleTemplate string
	PostTemplate  string
}

// TourShow is one row of the tour_shows table. Fields has the columns of
// the upload other than tour and date.
type TourShow struct {
	ID          int64
	TourID      int64
	Date        string // YYYY-MM-DD
	Fields      map[string]string
	ThreadID    string // "" until the bot posts it
	PostedTitle string
	PostedPost  string
}

// ErrTourName is the error for a tour name that the server already has.
var ErrTourName = errors.New("the server already has a tour with that name")

const tourCols = `id, guild_id, name, forum_id, title_template, post_template`

func scanTours(rows *sql.Rows) ([]Tour, error) {
	defer func() { _ = rows.Close() }()
	var out []Tour
	for rows.Next() {
		var t Tour
		if err := rows.Scan(&t.ID, &t.GuildID, &t.Name, &t.ForumID, &t.TitleTemplate, &t.PostTemplate); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (d *DB) oneTour(query string, args ...any) (*Tour, error) {
	rows, err := d.sql.Query(`SELECT `+tourCols+` FROM tours WHERE `+query, args...)
	if err != nil {
		return nil, err
	}
	out, err := scanTours(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

// GetTour returns a tour of a server, or nil when there is none.
func (d *DB) GetTour(guildID string, id int64) (*Tour, error) {
	return d.oneTour(`guild_id = ? AND id = ?`, guildID, id)
}

// TourByName returns a tour by its name (any case), or nil.
func (d *DB) TourByName(guildID, name string) (*Tour, error) {
	return d.oneTour(`guild_id = ? AND name = ?`, guildID, name)
}

// ActiveTours returns the tours of a server with a show on or after today
// (YYYY-MM-DD), by their first show.
func (d *DB) ActiveTours(guildID, today string) ([]Tour, error) {
	rows, err := d.sql.Query(`SELECT t.id, t.guild_id, t.name, t.forum_id, t.title_template, t.post_template
		FROM tours t JOIN tour_shows s ON s.tour_id = t.id
		WHERE t.guild_id = ? GROUP BY t.id HAVING MAX(s.date) >= ? ORDER BY MIN(s.date), t.id`, guildID, today)
	if err != nil {
		return nil, err
	}
	return scanTours(rows)
}

// CreateTour adds a tour and returns its ID.
func (d *DB) CreateTour(t Tour) (int64, error) {
	res, err := d.sql.Exec(`INSERT INTO tours (guild_id, name, forum_id, title_template, post_template) VALUES (?, ?, ?, ?, ?)`,
		t.GuildID, t.Name, t.ForumID, t.TitleTemplate, t.PostTemplate)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateTour saves the name, the forum, and the templates of a tour.
func (d *DB) UpdateTour(t Tour) error {
	if other, err := d.TourByName(t.GuildID, t.Name); err != nil {
		return err
	} else if other != nil && other.ID != t.ID {
		return ErrTourName
	}
	_, err := d.sql.Exec(`UPDATE tours SET name = ?, forum_id = ?, title_template = ?, post_template = ? WHERE id = ? AND guild_id = ?`,
		t.Name, t.ForumID, t.TitleTemplate, t.PostTemplate, t.ID, t.GuildID)
	return err
}

// TourShows returns the shows of a tour by date.
func (d *DB) TourShows(tourID int64) ([]TourShow, error) {
	rows, err := d.sql.Query(`SELECT id, tour_id, date, fields, thread_id, posted_title, posted_post
		FROM tour_shows WHERE tour_id = ? ORDER BY date, id`, tourID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TourShow
	for rows.Next() {
		var s TourShow
		var fields string
		if err := rows.Scan(&s.ID, &s.TourID, &s.Date, &fields, &s.ThreadID, &s.PostedTitle, &s.PostedPost); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(fields), &s.Fields); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpsertTourShow adds a show, or changes the fields of the show with the
// same date. It reports whether the show is new, and whether it changed.
func (d *DB) UpsertTourShow(tourID int64, date string, fields map[string]string) (added, changed bool, err error) {
	data, err := json.Marshal(fields)
	if err != nil {
		return false, false, err
	}
	var id int64
	var old string
	switch err := d.sql.QueryRow(`SELECT id, fields FROM tour_shows WHERE tour_id = ? AND date = ?`, tourID, date).Scan(&id, &old); {
	case errors.Is(err, sql.ErrNoRows):
		_, err := d.sql.Exec(`INSERT INTO tour_shows (tour_id, date, fields) VALUES (?, ?, ?)`, tourID, date, string(data))
		return err == nil, false, err
	case err != nil:
		return false, false, err
	}
	var oldFields map[string]string
	if err := json.Unmarshal([]byte(old), &oldFields); err == nil && maps.Equal(oldFields, fields) {
		return false, false, nil
	}
	_, err = d.sql.Exec(`UPDATE tour_shows SET fields = ? WHERE id = ?`, string(data), id)
	return false, err == nil, err
}

// UpdateTourShow changes the date and the fields of a show.
func (d *DB) UpdateTourShow(id int64, date string, fields map[string]string) error {
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = d.sql.Exec(`UPDATE tour_shows SET date = ?, fields = ? WHERE id = ?`, date, string(data), id)
	return err
}

// DeleteTourShow removes a show. Its thread stays.
func (d *DB) DeleteTourShow(id int64) error {
	_, err := d.sql.Exec(`DELETE FROM tour_shows WHERE id = ?`, id)
	return err
}

// SetTourShowPosted saves the thread of a show and what the bot posted.
func (d *DB) SetTourShowPosted(id int64, threadID, title, post string) error {
	_, err := d.sql.Exec(`UPDATE tour_shows SET thread_id = ?, posted_title = ?, posted_post = ? WHERE id = ?`,
		threadID, title, post, id)
	return err
}
