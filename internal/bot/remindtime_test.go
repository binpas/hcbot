package bot

import (
	"testing"
	"time"
)

func TestParseReminderTime(t *testing.T) {
	// Friday, 30 October 2026, 2:15:20 PM Pacific (PDT).
	now := time.Date(2026, 10, 30, 14, 15, 20, 0, reminderZone)
	at := func(y int, m time.Month, d, h, mi, s int) time.Time {
		return time.Date(y, m, d, h, mi, s, 0, reminderZone)
	}
	for _, c := range []struct {
		in   string
		want time.Time
	}{
		{"in 2h", now.Add(2 * time.Hour)},
		{"in 1h30m", now.Add(90 * time.Minute)},
		{"in 90 minutes", now.Add(90 * time.Minute)},
		{"in 3d", at(2026, 11, 2, 14, 15, 20)}, // across the end of PDT: same clock time
		{"in 2 days and 3 hours", now.AddDate(0, 0, 2).Add(3 * time.Hour)},
		{"in an hour", now.Add(time.Hour)},
		{"in 1w", at(2026, 11, 6, 14, 15, 20)},
		{"in 2 Weeks.", at(2026, 11, 13, 14, 15, 20)},
		{"on monday", at(2026, 11, 2, 14, 15, 20)},
		{"on friday", at(2026, 11, 6, 14, 15, 20)}, // today is Friday: next week
		{"on friday at 5pm", at(2026, 10, 30, 17, 0, 0)},
		{"on friday at 9am", at(2026, 11, 6, 9, 0, 0)},
		{"at 5pm", at(2026, 10, 30, 17, 0, 0)},
		{"at 5 pm", at(2026, 10, 30, 17, 0, 0)},
		{"at 9:30am", at(2026, 10, 31, 9, 30, 0)},
		{"at 14:00", at(2026, 10, 31, 14, 0, 0)},
		{"at noon", at(2026, 10, 31, 12, 0, 0)},
		{"at 12am", at(2026, 10, 31, 0, 0, 0)},
		{"on tomorrow", at(2026, 10, 31, 14, 15, 20)},
		{"on tomorrow at noon", at(2026, 10, 31, 12, 0, 0)},
		{"on nov 5", at(2026, 11, 5, 14, 15, 20)},
		{"on November 5th at 3pm", at(2026, 11, 5, 15, 0, 0)},
		{"on 5 nov", at(2026, 11, 5, 14, 15, 20)},
		{"on the 5th of november 2026", at(2026, 11, 5, 14, 15, 20)},
		{"on oct 1", at(2027, 10, 1, 14, 15, 20)}, // passed this year
		{"on 11/5", at(2026, 11, 5, 14, 15, 20)},  // month/day
		{"on 11/5/26 at 8:00 PM", at(2026, 11, 5, 20, 0, 0)},
		{"on 2026-12-25", at(2026, 12, 25, 14, 15, 20)},
		{"on 2026-12-25 15:30", at(2026, 12, 25, 15, 30, 0)},
		{"at 3pm on dec 1", at(2026, 12, 1, 15, 0, 0)},
	} {
		_, got, err := splitReminder("thing "+c.in, now)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("%q = %v, %v; want %v", c.in, got.In(reminderZone), err, c.want)
		}
	}

	for in, want := range map[string]error{
		"in soon":              errNoTime,
		"in 30s":               errSoon,
		"in 2y":                errFar,
		"on 2025-01-01":        errPastTime,
		"on 2026-10-30 at 1pm": errPastTime,
		"on nov 31":            errNoTime,
		"at 5":                 errNoTime, // a lone number is not a time
		"at 13pm":              errNoTime,
		"on 13/5":              errNoTime, // month/day
		"on friday monday":     errNoTime,
		"":                     errNoTime,
	} {
		if _, got, err := splitReminder("thing "+in, now); err != want {
			t.Errorf("%q = %v, %v; want %v", in, got, err, want)
		}
	}
}

func TestSplitReminder(t *testing.T) {
	now := time.Date(2026, 10, 30, 14, 15, 20, 0, reminderZone)
	for in, want := range map[string]string{
		"log in on friday":              "log in",
		"check in at the office in 2h":  "check in at the office",
		"go on a walk on friday at 3pm": "go on a walk",
		"the meeting at 3pm on friday":  "the meeting",
		"call mom in 2 hours":           "call mom",
		"look at it in in 2h":           "look at it in",
	} {
		thing, _, err := splitReminder(in, now)
		if err != nil || thing != want {
			t.Errorf("%q = %q, %v; want %q", in, thing, err, want)
		}
	}
	if _, _, err := splitReminder("in 2h", now); err == nil {
		t.Error("a reminder with no thing works")
	}
}
