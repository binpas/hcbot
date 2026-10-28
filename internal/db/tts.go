package db

import "time"

// TTSJob is one /say clip that waits for the text to speech worker.
type TTSJob struct {
	WorkerID  string // the worker's job ID
	GuildID   string
	UserID    string
	ChannelID string
	ReplyTo   string // the message to reply to, or ""
	Voice     string
	Text      string
	VoiceMsg  bool
	AppID     string // the interaction, for the member's status message
	Token     string
	Created   time.Time
}

// AddTTSJob stores a clip job.
func (d *DB) AddTTSJob(j TTSJob) error {
	_, err := d.sql.Exec(
		`INSERT INTO tts_jobs (worker_id, guild_id, user_id, channel_id, reply_to, voice, text, voice_msg, app_id, token, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.WorkerID, j.GuildID, j.UserID, j.ChannelID, j.ReplyTo, j.Voice, j.Text, j.VoiceMsg, j.AppID, j.Token,
		j.Created.UTC().Format(time.RFC3339Nano),
	)
	return err
}

// TTSJobs returns all clip jobs, the oldest first.
func (d *DB) TTSJobs() ([]TTSJob, error) {
	rows, err := d.sql.Query(
		`SELECT worker_id, guild_id, user_id, channel_id, reply_to, voice, text, voice_msg, app_id, token, created_at
		   FROM tts_jobs ORDER BY created_at`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TTSJob
	for rows.Next() {
		var j TTSJob
		var created string
		if err := rows.Scan(&j.WorkerID, &j.GuildID, &j.UserID, &j.ChannelID, &j.ReplyTo, &j.Voice, &j.Text,
			&j.VoiceMsg, &j.AppID, &j.Token, &created); err != nil {
			return nil, err
		}
		j.Created, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, j)
	}
	return out, rows.Err()
}

// DeleteTTSJob removes a clip job.
func (d *DB) DeleteTTSJob(workerID string) error {
	_, err := d.sql.Exec(`DELETE FROM tts_jobs WHERE worker_id=?`, workerID)
	return err
}
