package db

import "time"

// Scheduled action kinds.
const (
	ActionUnban      = "unban"
	ActionRemoveRole = "remove_role"
)

// ScheduledAction is one row of the scheduled_actions table.
type ScheduledAction struct {
	ID       int64
	GuildID  string
	UserID   string
	Username string
	Action   string
	RoleID   string
}

// ScheduleAction stores an action to run at runAt.
func (d *DB) ScheduleAction(guildID, userID, username, action, roleID string, runAt time.Time) error {
	_, err := d.sql.Exec(
		`INSERT INTO scheduled_actions (guild_id, user_id, username, action, role_id, run_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		guildID, userID, username, action, nullIfEmpty(roleID), runAt.UTC().Format(SentenceTimeFormat),
	)
	return err
}

// DueActions returns every scheduled action whose time has come.
func (d *DB) DueActions() ([]ScheduledAction, error) {
	rows, err := d.sql.Query(
		`SELECT id, CAST(guild_id AS TEXT), CAST(user_id AS TEXT), username, action,
		        COALESCE(CAST(role_id AS TEXT), '')
		   FROM scheduled_actions
		  WHERE datetime(run_at) <= datetime('now')
		  ORDER BY run_at`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ScheduledAction
	for rows.Next() {
		var a ScheduledAction
		if err := rows.Scan(&a.ID, &a.GuildID, &a.UserID, &a.Username, &a.Action, &a.RoleID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAction removes a scheduled action once it has run.
func (d *DB) DeleteAction(id int64) error {
	_, err := d.sql.Exec(`DELETE FROM scheduled_actions WHERE id=?`, id)
	return err
}
