package bot

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/binpas/hcbot/internal/db"
)

// Tour files and templates. A file is CSV with a header row, or a JSON
// array of objects. The "tour" and "date" (YYYY-MM-DD) columns are
// necessary; every column is a template token. {city} is the value as it
// is, and {CITY} is the value in uppercase.

const (
	maxTourFile    = 1 << 20
	maxTourRows    = 500
	maxTourShows   = 100 // shows in one tour
	maxTourColumns = 30
	maxTourValue   = 1000

	defaultTourTitle = "{TOUR} - {date} - {CITY} - {REGION}"
	defaultTourPost  = "{link}"

	maxThreadTitle = 100
	maxTourPost    = 2000
)

// tourDateTokens are the tokens that the bot makes from the date. A file
// cannot use them as column names.
var tourDateTokens = []string{"date_us", "date_eu", "date_iso", "date_local", "weekday", "month"}

// tourRow is one show in an uploaded file. Line is the CSV line or the
// JSON item number, for errors.
type tourRow struct {
	Tour   string
	Date   string
	Fields map[string]string
	Line   int
}

var (
	tourKeyChars = regexp.MustCompile(`^[a-z0-9_]+$`)
	tourISODate  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	tourToken    = regexp.MustCompile(`\{([A-Za-z0-9_]+)\}`)
)

// tourKey makes a column name into a token name: "Ticket Link" is
// "ticket_link".
func tourKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '-' || r == '_' }), "_")
}

// validTourDate reports whether s is a real YYYY-MM-DD date.
func validTourDate(s string) bool {
	if !tourISODate.MatchString(s) {
		return false
	}
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

// parseTourFile reads the shows of a CSV or JSON file. It returns the
// problems that it found (at most 10), or the rows.
func parseTourFile(data []byte) ([]tourRow, []string) {
	data = bytes.ToValidUTF8(data, []byte("\uFFFD"))
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // a BOM from Excel
	var records []map[string]string
	var lines []int
	var problems []string
	label := "Line"
	if t := bytes.TrimSpace(data); len(t) > 0 && t[0] == '[' {
		label = "Item"
		var items []map[string]any
		if err := json.Unmarshal(t, &items); err != nil {
			return nil, []string{"The JSON is not an array of objects: " + err.Error()}
		}
		for i, item := range items {
			rec := map[string]string{}
			for k, v := range item {
				switch v := v.(type) {
				case string:
					rec[k] = v
				case float64:
					rec[k] = strconv.FormatFloat(v, 'f', -1, 64)
				case bool:
					rec[k] = strconv.FormatBool(v)
				case nil:
					rec[k] = ""
				default:
					problems = append(problems, fmt.Sprintf("Item %d: `%s` must be text or a number.", i+1, k))
				}
			}
			records, lines = append(records, rec), append(lines, i+1)
		}
	} else {
		r := csv.NewReader(bytes.NewReader(data))
		r.FieldsPerRecord = -1
		r.TrimLeadingSpace = true
		all, err := r.ReadAll()
		if err != nil {
			return nil, []string{"The CSV is not valid: " + err.Error()}
		}
		if len(all) == 0 {
			return nil, []string{"The file is empty."}
		}
		header := all[0]
		for i, row := range all[1:] {
			if len(row) == 1 && strings.TrimSpace(row[0]) == "" {
				continue // a blank line
			}
			if len(row) > len(header) {
				problems = append(problems, fmt.Sprintf("Line %d has more values than the header.", i+2))
				continue
			}
			rec := map[string]string{}
			for j, h := range header {
				if j < len(row) {
					rec[h] = row[j]
				} else {
					rec[h] = ""
				}
			}
			records, lines = append(records, rec), append(lines, i+2)
		}
	}
	if len(records) == 0 && len(problems) == 0 {
		return nil, []string{"The file has no shows."}
	}
	if len(records) > maxTourRows {
		return nil, []string{fmt.Sprintf("The file has %d shows. The limit is %d.", len(records), maxTourRows)}
	}

	var rows []tourRow
	seen := map[string]int{}
	for i, rec := range records {
		row := tourRow{Fields: map[string]string{}, Line: lines[i]}
		where := fmt.Sprintf("%s %d", label, row.Line)
		for k, v := range rec {
			key, v := tourKey(k), strings.TrimSpace(v)
			switch {
			case key == "":
				if v != "" {
					problems = append(problems, where+" has a value in a column with no name.")
				}
				continue
			case !tourKeyChars.MatchString(key):
				problems = append(problems, fmt.Sprintf("The column name `%s` can have only letters, numbers, spaces, `-`, and `_`.", k))
				continue
			case slices.Contains(tourDateTokens, key):
				problems = append(problems, fmt.Sprintf("`%s` is a token that the bot makes from the date. Use a different column name.", key))
				continue
			case len([]rune(v)) > maxTourValue:
				problems = append(problems, fmt.Sprintf("%s: `%s` has more than %d characters.", where, key, maxTourValue))
			}
			switch key {
			case "tour":
				row.Tour = v
			case "date":
				row.Date = v
			default:
				row.Fields[key] = v
			}
		}
		if len(row.Fields)+2 > maxTourColumns {
			return nil, []string{fmt.Sprintf("The file has more than %d columns.", maxTourColumns)}
		}
		switch {
		case row.Tour == "":
			problems = append(problems, where+" has no `tour`.")
		case len([]rune(row.Tour)) > 80:
			problems = append(problems, where+": the tour name has more than 80 characters.")
		case !validTourDate(row.Date):
			problems = append(problems, fmt.Sprintf("%s: the date `%s` is not a YYYY-MM-DD date (for example `2027-03-05`).", where, row.Date))
		default:
			k := strings.ToLower(row.Tour) + "\x00" + row.Date
			if first, dup := seen[k]; dup {
				problems = append(problems, fmt.Sprintf("%s has the same tour and date as %s %d.", where, strings.ToLower(label), first))
			}
			seen[k] = row.Line
		}
		rows = append(rows, row)
	}
	slices.Sort(problems)
	problems = slices.Compact(problems)
	if len(problems) > 10 {
		problems = append(problems[:10], fmt.Sprintf("…and %d more problems.", len(problems)-10))
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return rows, nil
}

// usRegions are the US states, DC, and the territories, by code and name.
var usRegions = func() map[string]bool {
	m := map[string]bool{}
	for _, s := range strings.Split("al alabama|ak alaska|az arizona|ar arkansas|ca california|co colorado|ct connecticut|"+
		"de delaware|fl florida|ga georgia|hi hawaii|id idaho|il illinois|in indiana|ia iowa|ks kansas|ky kentucky|"+
		"la louisiana|me maine|md maryland|ma massachusetts|mi michigan|mn minnesota|ms mississippi|mo missouri|"+
		"mt montana|ne nebraska|nv nevada|nh new hampshire|nj new jersey|nm new mexico|ny new york|"+
		"nc north carolina|nd north dakota|oh ohio|ok oklahoma|or oregon|pa pennsylvania|ri rhode island|"+
		"sc south carolina|sd south dakota|tn tennessee|tx texas|ut utah|vt vermont|va virginia|wa washington|"+
		"wv west virginia|wi wisconsin|wy wyoming|dc district of columbia|dc washington dc|dc washington d.c.|"+
		"pr puerto rico|gu guam|vi us virgin islands|as american samoa|mp northern mariana islands|"+
		"us usa|us united states|us united states of america", "|") {
		code, name, _ := strings.Cut(s, " ")
		m[code], m[name] = true, true
	}
	return m
}()

// usDates reports whether a show uses the US date format: its region is a
// US state, or it has no region.
func usDates(region string) bool {
	r := strings.ToLower(strings.TrimSpace(region))
	return r == "" || usRegions[r]
}

// tourColumns returns the column tokens of a tour's shows.
func tourColumns(shows []db.TourShow) map[string]bool {
	cols := map[string]bool{"tour": true, "date": true}
	for _, t := range tourDateTokens {
		cols[t] = true
	}
	for _, s := range shows {
		for k := range s.Fields {
			cols[k] = true
		}
	}
	return cols
}

// renderTour fills a template for one show. It returns the tokens that are
// not columns of the tour.
func renderTour(tmpl, tourName string, s db.TourShow, cols map[string]bool) (string, []string) {
	var unknown []string
	d, _ := time.Parse(time.DateOnly, s.Date)
	out := tourToken.ReplaceAllStringFunc(tmpl, func(tok string) string {
		name := tok[1 : len(tok)-1]
		key := strings.ToLower(name)
		if !cols[key] {
			if !slices.Contains(unknown, tok) {
				unknown = append(unknown, tok)
			}
			return tok
		}
		var v string
		switch key {
		case "tour":
			v = tourName
		case "date":
			if usDates(s.Fields["region"]) {
				v = d.Format("01/02")
			} else {
				v = d.Format("02/01")
			}
		case "date_us":
			v = d.Format("01/02")
		case "date_eu":
			v = d.Format("02/01")
		case "date_iso":
			v = s.Date
		case "date_local":
			// Noon UTC is the same day in almost every time zone.
			return discordTimestamp(d.Add(12*time.Hour).Unix(), "D")
		case "weekday":
			v = d.Weekday().String()
		case "month":
			v = d.Month().String()
		default:
			v = s.Fields[key]
		}
		if name != key && name == strings.ToUpper(name) {
			v = strings.ToUpper(v)
		}
		return v
	})
	return out, unknown
}

// tourText is the title and the post of a show, or the problems with them.
type tourText struct {
	Title, Post string
	Problems    []string
}

func renderTourShow(t *db.Tour, s db.TourShow, cols map[string]bool) tourText {
	title, u1 := renderTour(t.TitleTemplate, t.Name, s, cols)
	post, u2 := renderTour(t.PostTemplate, t.Name, s, cols)
	out := tourText{Title: strings.TrimSpace(title), Post: strings.TrimSpace(post)}
	if len(u1) > 0 {
		out.Problems = append(out.Problems, "The title template has tokens that are not columns: `"+strings.Join(u1, "` `")+"`")
	}
	if len(u2) > 0 {
		out.Problems = append(out.Problems, "The post template has tokens that are not columns: `"+strings.Join(u2, "` `")+"`")
	}
	switch n := len([]rune(out.Title)); {
	case n == 0:
		out.Problems = append(out.Problems, s.Date+": the title is empty.")
	case n > maxThreadTitle:
		out.Problems = append(out.Problems, fmt.Sprintf("%s: the title has %d characters. The limit is %d.", s.Date, n, maxThreadTitle))
	}
	switch n := len([]rune(out.Post)); {
	case n == 0:
		out.Problems = append(out.Problems, s.Date+": the post is empty.")
	case n > maxTourPost:
		out.Problems = append(out.Problems, fmt.Sprintf("%s: the post has %d characters. The limit is %d.", s.Date, n, maxTourPost))
	}
	return out
}

// tourSampleCSV is the sample file of /tour help.
const tourSampleCSV = `tour,date,venue,city,region,link,support
Example Tour 2027,2027-03-05,The Fillmore,San Francisco,CA,https://example.com/tickets/sf,Opening Band
Example Tour 2027,2027-03-07,Crystal Ballroom,Portland,OR,https://example.com/tickets/pdx,Opening Band
Example Tour 2027,2027-03-08,The Showbox,Seattle,WA,https://example.com/tickets/sea,
Example Tour 2027,2027-04-02,O2 Academy Brixton,London,UK,https://example.com/tickets/london,Other Band
Example Tour 2027,2027-04-05,Columbiahalle,Berlin,Germany,https://example.com/tickets/berlin,Other Band
`
