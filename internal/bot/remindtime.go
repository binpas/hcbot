package bot

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the reminder zone, also without the system zone files
)

// reminderZone is the time zone of reminder times: a day or a clock time
// in a reminder is Pacific time.
var reminderZone = func() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return loc
}()

const (
	reminderMin = time.Minute
	reminderMax = 366 * 24 * time.Hour
)

var (
	errNoTime   = errors.New("the time is not one I can read")
	errPastTime = errors.New("that time is in the past")
	errSoon     = errors.New("the time must be at least 1 minute from now")
	errFar      = errors.New("the time must be no more than 1 year from now")
)

var reminderUnits = map[string]string{
	"s": "s", "sec": "s", "secs": "s", "second": "s", "seconds": "s",
	"m": "m", "min": "m", "mins": "m", "minute": "m", "minutes": "m",
	"h": "h", "hr": "h", "hrs": "h", "hour": "h", "hours": "h",
	"d": "d", "day": "d", "days": "d",
	"w": "w", "wk": "w", "wks": "w", "week": "w", "weeks": "w",
	"mo": "mo", "mos": "mo", "month": "mo", "months": "mo",
	"y": "y", "yr": "y", "yrs": "y", "year": "y", "years": "y",
}

var (
	relPart   = regexp.MustCompile(`(\d+)([a-z]+)`)
	relWord   = regexp.MustCompile(`^(?:\d+[a-z]+)+$`)
	isoDate   = regexp.MustCompile(`^(\d{4})-(\d{1,2})-(\d{1,2})$`)
	usDate    = regexp.MustCompile(`^(\d{1,2})/(\d{1,2})(?:/(\d{2}|\d{4}))?$`)
	clockTime = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?(am|pm|a|p)?$`)
	dayNumber = regexp.MustCompile(`^(\d{1,2})(?:st|nd|rd|th)?$`)
	yearWord  = regexp.MustCompile(`^\d{4}$`)
)

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday,
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tues": time.Tuesday, "tuesday": time.Tuesday,
	"wed": time.Wednesday, "weds": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday, "thursday": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
}

var months = map[string]time.Month{
	"jan": time.January, "january": time.January, "feb": time.February, "february": time.February,
	"mar": time.March, "march": time.March, "apr": time.April, "april": time.April,
	"may": time.May, "jun": time.June, "june": time.June, "jul": time.July, "july": time.July,
	"aug": time.August, "august": time.August, "sep": time.September, "sept": time.September,
	"september": time.September, "oct": time.October, "october": time.October,
	"nov": time.November, "november": time.November, "dec": time.December, "december": time.December,
}

// reminderWords splits a time into lowercase words, without commas and
// end punctuation.
func reminderWords(s string) []string {
	s = strings.ToLower(strings.TrimRight(strings.TrimSpace(s), ".!?"))
	return strings.Fields(strings.ReplaceAll(s, ",", " "))
}

// parseRelative reads a time from now: "2h", "1h30m", "90 minutes",
// "2 days and 3 hours", "an hour".
func parseRelative(s string, now time.Time) (time.Time, error) {
	words := reminderWords(s)
	t, any := now, false
	add := func(n int, unit string) {
		switch unit {
		case "s":
			t = t.Add(time.Duration(n) * time.Second)
		case "m":
			t = t.Add(time.Duration(n) * time.Minute)
		case "h":
			t = t.Add(time.Duration(n) * time.Hour)
		case "d":
			t = t.AddDate(0, 0, n)
		case "w":
			t = t.AddDate(0, 0, 7*n)
		case "mo":
			t = t.AddDate(0, n, 0)
		case "y":
			t = t.AddDate(n, 0, 0)
		}
		any = true
	}
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "and" && any:
			continue
		case relWord.MatchString(w):
			for _, p := range relPart.FindAllStringSubmatch(w, -1) {
				unit, ok := reminderUnits[p[2]]
				n, err := strconv.Atoi(p[1])
				if !ok || err != nil || n > 100000 {
					return time.Time{}, errNoTime
				}
				add(n, unit)
			}
		case i+1 < len(words) && reminderUnits[words[i+1]] != "":
			n, err := strconv.Atoi(w)
			if w == "a" || w == "an" {
				n, err = 1, nil
			}
			if err != nil || n > 100000 {
				return time.Time{}, errNoTime
			}
			add(n, reminderUnits[words[i+1]])
			i++
		default:
			return time.Time{}, errNoTime
		}
	}
	if !any {
		return time.Time{}, errNoTime
	}
	return checkReminderTime(t, now)
}

// parseAbsolute reads a day, a clock time, or both, in reminderZone:
// "friday", "tomorrow at noon", "nov 5 3pm", "5 nov", "2026-11-05 15:30",
// "11/5" (month/day), "5pm". A day without a clock time keeps the clock
// time of now. A day or time without a year or date that has passed is the
// next one.
func parseAbsolute(s string, now time.Time) (time.Time, error) {
	now = now.In(reminderZone)
	words := reminderWords(s)
	var (
		haveDate, haveClock bool
		roll                func(time.Time) time.Time // the next one, for a passed time; nil: none
		y, d, hh, mm        int
		mon                 time.Month
		wd                  = time.Weekday(-1)
	)
	setDate := func(yy int, m time.Month, dd int) bool {
		if haveDate {
			return false
		}
		haveDate, y, mon, d = true, yy, m, dd
		return true
	}
	ok := true
	for i := 0; i < len(words) && ok; i++ {
		w := words[i]
		next := func() string {
			if i+1 < len(words) {
				return words[i+1]
			}
			return ""
		}
		switch {
		case w == "at" || w == "on" || w == "the" || w == "of" || w == "next":
		case w == "today" || w == "tomorrow" || w == "tmrw" || w == "tmr":
			day := now
			if w != "today" {
				day = now.AddDate(0, 0, 1)
			}
			ok = setDate(day.Year(), day.Month(), day.Day())
		case weekdays[w] != 0 || w == "sun" || w == "sunday":
			ok = setDate(0, 0, 0)
			wd = weekdays[w]
		case w == "noon" || w == "midnight":
			ok = !haveClock
			haveClock, hh, mm = true, 12, 0
			if w == "midnight" {
				hh = 0
			}
		case months[w] != 0:
			// "nov 5", "nov 5th 2027"
			m := dayNumber.FindStringSubmatch(next())
			if m == nil {
				ok = false
				break
			}
			i++
			dd, _ := strconv.Atoi(m[1])
			yy := 0
			if yearWord.MatchString(next()) {
				yy, _ = strconv.Atoi(next())
				i++
			}
			ok = setDate(yy, months[w], dd)
		case dayNumber.MatchString(w) && (months[next()] != 0 || (next() == "of" && i+2 < len(words) && months[words[i+2]] != 0)):
			// "5 nov", "5th of november 2027"
			dd, _ := strconv.Atoi(dayNumber.FindStringSubmatch(w)[1])
			i++
			if words[i] == "of" {
				i++
			}
			m := months[words[i]]
			yy := 0
			if yearWord.MatchString(next()) {
				yy, _ = strconv.Atoi(next())
				i++
			}
			ok = setDate(yy, m, dd)
		case isoDate.MatchString(w):
			p := isoDate.FindStringSubmatch(w)
			yy, _ := strconv.Atoi(p[1])
			m, _ := strconv.Atoi(p[2])
			dd, _ := strconv.Atoi(p[3])
			ok = setDate(yy, time.Month(m), dd)
		case usDate.MatchString(w):
			p := usDate.FindStringSubmatch(w)
			m, _ := strconv.Atoi(p[1])
			dd, _ := strconv.Atoi(p[2])
			yy, _ := strconv.Atoi(p[3])
			if yy > 0 && yy < 100 {
				yy += 2000
			}
			ok = setDate(yy, time.Month(m), dd)
		case clockTime.MatchString(w):
			p := clockTime.FindStringSubmatch(w)
			ampm := p[3]
			if ampm == "" && (next() == "am" || next() == "pm") {
				ampm = next()
				i++
			}
			if haveClock || (p[2] == "" && ampm == "") {
				ok = false // a lone number is not a time
				break
			}
			h, _ := strconv.Atoi(p[1])
			mi, _ := strconv.Atoi(p[2])
			switch {
			case mi > 59, ampm == "" && h > 23, ampm != "" && (h < 1 || h > 12):
				ok = false
			case strings.HasPrefix(ampm, "p") && h != 12:
				h += 12
			case strings.HasPrefix(ampm, "a") && h == 12:
				h = 0
			}
			haveClock, hh, mm = true, h, mi
		default:
			ok = false
		}
	}
	if !ok || (!haveDate && !haveClock) {
		return time.Time{}, errNoTime
	}

	sec := 0
	if !haveClock {
		hh, mm, sec = now.Hour(), now.Minute(), now.Second() // the time it was set
	}
	switch {
	case wd >= 0:
		ahead := (int(wd) - int(now.Weekday()) + 7) % 7
		day := now.AddDate(0, 0, ahead)
		y, mon, d = day.Year(), day.Month(), day.Day()
		roll = func(t time.Time) time.Time { return t.AddDate(0, 0, 7) }
	case !haveDate:
		y, mon, d = now.Year(), now.Month(), now.Day()
		roll = func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
	case y == 0:
		y = now.Year()
		roll = func(t time.Time) time.Time { return t.AddDate(1, 0, 0) }
	}
	if mon < 1 || mon > 12 || d < 1 {
		return time.Time{}, errNoTime
	}
	t := time.Date(y, mon, d, hh, mm, sec, 0, reminderZone)
	if t.Day() != d { // 31 November
		return time.Time{}, errNoTime
	}
	if roll != nil && t.Before(now.Add(reminderMin)) {
		t = roll(t)
	}
	if !t.After(now) {
		return time.Time{}, errPastTime
	}
	return checkReminderTime(t, now)
}

func checkReminderTime(t, now time.Time) (time.Time, error) {
	switch {
	case t.Before(now.Add(reminderMin)):
		return time.Time{}, errSoon
	case t.After(now.Add(reminderMax)):
		return time.Time{}, errFar
	}
	return t, nil
}

// splitReminder cuts "<thing> (in|on|at) <time>" at the first in, on, or
// at whose rest is a time, so the thing can have these words too ("log in
// on friday"). A rest that is a time but not a good one (in the past)
// stops the search with its error.
func splitReminder(body string, now time.Time) (string, time.Time, error) {
	for _, loc := range reminderCut.FindAllStringSubmatchIndex(body, -1) {
		word := strings.ToLower(body[loc[2]:loc[3]])
		thing, rest := strings.TrimSpace(body[:loc[0]]), body[loc[1]:]
		var t time.Time
		var e error
		if word == "in" {
			t, e = parseRelative(rest, now)
		} else {
			t, e = parseAbsolute(rest, now)
		}
		switch {
		case e == nil && thing != "":
			return thing, t, nil
		case e != nil && e != errNoTime:
			return "", time.Time{}, e
		}
	}
	return "", time.Time{}, errNoTime
}

// reminderCut finds each "in", "on", and "at". A match does not take the
// space before the word, so "in on friday" has two matches.
var reminderCut = regexp.MustCompile(`(?i)\b(in|on|at)\s+`)
