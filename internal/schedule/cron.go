// Package schedule parses cron expressions and says when they run.
// internal/dispatch turns due schedules into tasks.
package schedule

import (
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed five-field expression: minute, hour, day of month, month,
// day of week. Each field is a bit set of the values it allows.
type Cron struct {
	minute, hour, dom, month, dow uint64
	// domStar and dowStar follow cron's rule: when both day fields are
	// restricted, a day matches if either does.
	domStar, dowStar bool
	src              string
}

type field struct {
	name     string
	min, max int
	names    map[string]int
}

var (
	minuteField = field{name: "minute", min: 0, max: 59}
	hourField   = field{name: "hour", min: 0, max: 23}
	domField    = field{name: "day of month", min: 1, max: 31}
	monthField  = field{name: "month", min: 1, max: 12, names: map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}}
	// 7 is Sunday too, as in most crons.
	dowField = field{name: "day of week", min: 0, max: 7, names: map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}}
)

var macros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// Parse reads an expression such as "0 8 * * 1-5" or "@daily".
func Parse(expr string) (Cron, error) {
	src := strings.TrimSpace(expr)
	if m, ok := macros[strings.ToLower(src)]; ok {
		expr = m
	}
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return Cron{}, fmt.Errorf("a schedule has five fields - minute, hour, day of month, "+
			"month and day of week, such as \"0 8 * * 1-5\" - not %d", len(parts))
	}
	c := Cron{src: src}
	for i, f := range []struct {
		field
		set *uint64
	}{{minuteField, &c.minute}, {hourField, &c.hour}, {domField, &c.dom}, {monthField, &c.month}, {dowField, &c.dow}} {
		var err error
		if *f.set, err = f.parse(parts[i]); err != nil {
			return Cron{}, err
		}
	}
	if c.dow&(1<<7) != 0 {
		c.dow = c.dow&^(1<<7) | 1
	}
	c.domStar = parts[2] == "*" || parts[2] == "?"
	c.dowStar = parts[4] == "*" || parts[4] == "?"
	return c, nil
}

// Zone loads a time zone by name, with an error fit to show a person.
func Zone(name string) (*time.Location, error) {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q", name)
	}
	return loc, nil
}

// NextRun is when expr next runs after now, in the time zone tz.
func NextRun(expr, tz string, now time.Time) (time.Time, error) {
	c, err := Parse(expr)
	if err != nil {
		return time.Time{}, err
	}
	loc, err := Zone(tz)
	if err != nil {
		return time.Time{}, err
	}
	return c.Next(now, loc)
}

func (f field) parse(s string) (uint64, error) {
	var set uint64
	for part := range strings.SplitSeq(s, ",") {
		bitsOf, err := f.parsePart(part)
		if err != nil {
			return 0, err
		}
		set |= bitsOf
	}
	return set, nil
}

func (f field) parsePart(s string) (uint64, error) {
	bad := fmt.Errorf("the %s field cannot read %q", f.name, s)
	rangePart, stepPart, hasStep := strings.Cut(s, "/")
	step := 1
	if hasStep {
		n, err := strconv.Atoi(stepPart)
		// A step past the field's end would overflow the loop below.
		if err != nil || n < 1 || n > f.max {
			return 0, bad
		}
		step = n
	}
	lo, hi := f.min, f.max
	switch {
	case rangePart == "*" || rangePart == "?":
	case strings.Contains(rangePart, "-"):
		a, b, _ := strings.Cut(rangePart, "-")
		var err error
		if lo, err = f.value(a); err != nil {
			return 0, bad
		}
		if hi, err = f.value(b); err != nil {
			return 0, bad
		}
		if hi < lo {
			return 0, fmt.Errorf("the %s field has a range that runs backwards: %q", f.name, s)
		}
	default:
		v, err := f.value(rangePart)
		if err != nil {
			return 0, bad
		}
		lo = v
		if !hasStep {
			hi = v
		}
	}
	var set uint64
	for v := lo; v <= hi; v += step {
		set |= 1 << v
	}
	return set, nil
}

func (f field) value(s string) (int, error) {
	if v, ok := f.names[strings.ToLower(s)]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < f.min || v > f.max {
		return 0, fmt.Errorf("the %s must be from %d to %d", f.name, f.min, f.max)
	}
	return v, nil
}

func has(set uint64, v int) bool { return set&(1<<v) != 0 }

func (c Cron) dayMatches(t time.Time) bool {
	dom, dow := has(c.dom, t.Day()), has(c.dow, int(t.Weekday()))
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dow
	case c.dowStar:
		return dom
	default:
		return dom || dow
	}
}

// ErrNever is an expression that never matches, such as 30 February.
var ErrNever = errors.New("this schedule never runs")

// Next returns the first time after t that matches, in loc. A wall-clock
// time a daylight-saving change skips runs right after the change; one it
// repeats runs once. A schedule that runs every hour does neither: it has a
// run in the next hour anyway.
func (c Cron) Next(t time.Time, loc *time.Location) (time.Time, error) {
	t = t.In(loc).Truncate(time.Minute).Add(time.Minute)
	// Four years covers every leap day; beyond that it never matches.
	limit := t.AddDate(4, 0, 1)
	for t.Before(limit) {
		if c.skipped(t) {
			return t, nil
		}
		if !has(c.month, int(t.Month())) {
			t = dayStart(t.Year(), t.Month()+1, 1, loc)
			continue
		}
		if !c.dayMatches(t) {
			t = dayStart(t.Year(), t.Month(), t.Day()+1, loc)
			continue
		}
		if !has(c.hour, t.Hour()) {
			next := first(time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc))
			if !next.After(t) {
				// A repeated hour at the end of daylight saving.
				next = t.Add(time.Hour).Truncate(time.Hour)
			}
			t = next
			continue
		}
		if !has(c.minute, t.Minute()) || (!all(c.hour, hourField) && repeated(t)) {
			t = t.Add(time.Minute)
			continue
		}
		return t, nil
	}
	return time.Time{}, ErrNever
}

// skipped reports whether t comes right after the clocks went forward, and
// a wall-clock time they jumped over matches.
func (c Cron) skipped(t time.Time) bool {
	before := t.Add(-time.Minute)
	_, now := t.Zone()
	_, earlier := before.Zone()
	if earlier >= now || all(c.hour, hourField) {
		return false
	}
	// The wall-clock times from a minute after before up to t never were.
	// Read in UTC, which has no gaps.
	wall := func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
	}
	for w := wall(before).Add(time.Minute); w.Before(wall(t)); w = w.Add(time.Minute) {
		if has(c.month, int(w.Month())) && c.dayMatches(w) && has(c.hour, w.Hour()) && has(c.minute, w.Minute()) {
			return true
		}
	}
	return false
}

// repeated reports whether t's wall-clock time already passed once, as in
// the hour the clocks go back. Earlier, the offset was larger by the time
// they went back; the same wall clock then was that much sooner.
func repeated(t time.Time) bool {
	_, now := t.Zone()
	// Clocks go back by at most two hours, so three hours before is still
	// on the old offset.
	_, before := t.Add(-3 * time.Hour).Zone()
	if before <= now {
		return false
	}
	u := t.Add(-time.Duration(before-now) * time.Second)
	uy, um, ud := u.Date()
	ty, tm, td := t.Date()
	return uy == ty && um == tm && ud == td && u.Hour() == t.Hour() && u.Minute() == t.Minute()
}

// first is the first time t's wall clock came, where the clocks went back.
// time.Date may give the second, as it does east of UTC; Next would then
// pass the first and skip the second as repeated, and miss the run.
func first(t time.Time) time.Time {
	if !repeated(t) {
		return t
	}
	_, now := t.Zone()
	_, before := t.Add(-3 * time.Hour).Zone()
	return t.Add(-time.Duration(before-now) * time.Second)
}

// dayStart is the first moment of a day in loc. Where daylight saving skips
// midnight, time.Date may return the evening before, which would keep Next
// on the same day forever. The day then starts when the clocks change: at
// midnight by the offset that ends.
func dayStart(year int, month time.Month, day int, loc *time.Location) time.Time {
	t := time.Date(year, month, day, 0, 0, 0, 0, loc)
	// Normalised, as 32 March is 1 April.
	y, m, d := time.Date(year, month, day, 12, 0, 0, 0, time.UTC).Date()
	if ty, tm, td := t.Date(); ty == y && tm == m && td == d {
		return first(t)
	}
	_, offset := t.Zone()
	return time.Date(y, m, d, 0, 0, 0, 0, time.FixedZone("", offset)).In(loc)
}

// Describe says when a schedule runs, in words, for the shapes the panel
// builds. Others read as the expression itself.
func (c Cron) Describe() string {
	m, mOK := single(c.minute)
	h, hOK := single(c.hour)
	allMonths := all(c.month, monthField)
	raw := "At " + c.src

	if mOK && hOK {
		at := fmt.Sprintf("%02d:%02d", h, m)
		mon, monOK := single(c.month)
		dom, domOK := single(c.dom)
		switch {
		case !allMonths:
			if monOK && domOK && c.dowStar {
				return fmt.Sprintf("Every year on %d %s at %s", dom, time.Month(mon), at)
			}
			return raw
		case c.domStar && c.dowStar:
			return "Every day at " + at
		case c.domStar && c.dow == weekdays:
			return "Every weekday at " + at
		case c.domStar && c.dow == weekends:
			return "Every weekend at " + at
		case c.domStar:
			return "Every " + dayList(c.dow) + " at " + at
		case c.dowStar && domOK:
			return fmt.Sprintf("On day %d of every month at %s", dom, at)
		}
		return raw
	}

	// Runs more than once a day: every few minutes or hours, perhaps only
	// between some hours and on some days.
	if !allMonths || !c.domStar {
		return raw
	}
	var every string
	if first, last, ok := hourRange(c.hour); ok {
		switch {
		case all(c.minute, minuteField):
			every = "Every minute"
		case mOK && m == 0:
			every = "Every hour"
		case mOK:
			every = fmt.Sprintf("Every hour at %d minutes past", m)
		default:
			step, ok := everyN(c.minute, 60)
			if !ok {
				return raw
			}
			every = fmt.Sprintf("Every %d minutes", step)
		}
		if first != 0 || last != 23 {
			every += fmt.Sprintf(" between %02d:00 and %02d:00", first, last+1)
		}
	} else if step, first, last, ok := hourSteps(c.hour); ok && mOK {
		every = fmt.Sprintf("Every %d hours", step)
		switch {
		case first != 0 || last+step <= 23:
			every += fmt.Sprintf(" from %02d:%02d to %02d:%02d", first, m, last, m)
		case m != 0:
			every += fmt.Sprintf(" at %d minutes past", m)
		}
	} else {
		return raw
	}
	if c.dowStar {
		return every
	}
	return every + ", " + c.onDays()
}

func single(set uint64) (int, bool) {
	if bits.OnesCount64(set) != 1 {
		return 0, false
	}
	return bits.TrailingZeros64(set), true
}

// all reports whether set holds every value of f. Parse sets bits only in
// range.
func all(set uint64, f field) bool {
	return bits.OnesCount64(set) == f.max-f.min+1
}

// hourRange reports whether set is every hour from first to last.
func hourRange(set uint64) (first, last int, ok bool) {
	first = bits.TrailingZeros64(set)
	last = 63 - bits.LeadingZeros64(set)
	return first, last, set != 0 && bits.OnesCount64(set) == last-first+1
}

// hourSteps reports whether set is three or more hours the same step apart,
// such as 08:00, 10:00 and 12:00. Two hours read better as the expression.
func hourSteps(set uint64) (step, first, last int, ok bool) {
	first = bits.TrailingZeros64(set)
	last = 63 - bits.LeadingZeros64(set)
	n := bits.OnesCount64(set)
	if n < 3 || (last-first)%(n-1) != 0 {
		return 0, 0, 0, false
	}
	step = (last - first) / (n - 1)
	for v := first; v <= last; v += step {
		if !has(set, v) {
			return 0, 0, 0, false
		}
	}
	return step, first, last, true
}

const (
	weekdays = 0b0111110
	weekends = 0b1000001
)

// onDays names the days of the week a schedule runs on.
func (c Cron) onDays() string {
	switch c.dow {
	case weekdays:
		return "on weekdays"
	case weekends:
		return "on weekends"
	}
	return "on " + dayList(c.dow)
}

func dayList(set uint64) string {
	var names []string
	// Monday first, as people in Switzerland read a week.
	for _, d := range []int{1, 2, 3, 4, 5, 6, 0} {
		if has(set, d) {
			names = append(names, time.Weekday(d).String())
		}
	}
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// everyN reports whether set is 0, n, 2n, … up to size.
func everyN(set uint64, size int) (int, bool) {
	if !has(set, 0) {
		return 0, false
	}
	for n := 2; n < size; n++ {
		var want uint64
		for v := 0; v < size; v += n {
			want |= 1 << v
		}
		if want == set {
			return n, true
		}
	}
	return 0, false
}
