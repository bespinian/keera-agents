package schedule

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skip("no time zone data:", err)
	}
	return loc
}

func TestNext(t *testing.T) {
	zurich := mustLoc(t, "Europe/Zurich")
	from := time.Date(2026, 11, 2, 9, 30, 0, 0, zurich) // a Monday
	for _, tc := range []struct {
		expr string
		want time.Time
	}{
		{"0 8 * * 1-5", time.Date(2026, 11, 3, 8, 0, 0, 0, zurich)},
		{"*/15 * * * *", time.Date(2026, 11, 2, 9, 45, 0, 0, zurich)},
		{"@daily", time.Date(2026, 11, 3, 0, 0, 0, 0, zurich)},
		{"0 9 1 * *", time.Date(2026, 12, 1, 9, 0, 0, 0, zurich)},
		{"0 9 * * sun", time.Date(2026, 11, 8, 9, 0, 0, 0, zurich)},
		{"0 9 * * 7", time.Date(2026, 11, 8, 9, 0, 0, 0, zurich)},
		{"0 12 29 2 *", time.Date(2028, 2, 29, 12, 0, 0, 0, zurich)},
		// Both day fields restricted: either matches.
		{"0 10 15 * fri", time.Date(2026, 11, 6, 10, 0, 0, 0, zurich)},
	} {
		c, err := Parse(tc.expr)
		if err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}
		got, err := c.Next(from, zurich)
		if err != nil || !got.Equal(tc.want) {
			t.Errorf("%s: Next = %v, %v; want %v", tc.expr, got, err, tc.want)
		}
	}
}

func TestNextSkippedHour(t *testing.T) {
	zurich := mustLoc(t, "Europe/Zurich")
	// 02:00 to 03:00 does not exist on 29 March 2026 in Zurich, so a run in
	// it happens at 03:00, once.
	changed := time.Date(2026, 3, 29, 3, 0, 0, 0, zurich)
	for _, tc := range []struct {
		expr      string
		from      time.Time
		want      time.Time
		wantAfter time.Time
	}{
		{"30 2 * * *", time.Date(2026, 3, 29, 0, 0, 0, 0, zurich), changed, time.Date(2026, 3, 30, 2, 30, 0, 0, zurich)},
		{"30 2 29 3 *", time.Date(2026, 3, 1, 0, 0, 0, 0, zurich), changed, time.Date(2027, 3, 29, 2, 30, 0, 0, zurich)},
		{"*/15 2 * * *", time.Date(2026, 3, 29, 1, 59, 30, 0, zurich), changed, time.Date(2026, 3, 30, 2, 0, 0, 0, zurich)},
		// The hour after the change still runs on its own.
		{"30 2,3 * * *", time.Date(2026, 3, 29, 0, 0, 0, 0, zurich), changed, time.Date(2026, 3, 29, 3, 30, 0, 0, zurich)},
		// Every hour: no run to make up, the next one comes anyway.
		{"30 * * * *", time.Date(2026, 3, 29, 1, 45, 0, 0, zurich), time.Date(2026, 3, 29, 3, 30, 0, 0, zurich), time.Date(2026, 3, 29, 4, 30, 0, 0, zurich)},
	} {
		c, err := Parse(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.Next(tc.from, zurich)
		if err != nil || !got.Equal(tc.want) {
			t.Errorf("%s: Next = %v, %v; want %v", tc.expr, got, err, tc.want)
			continue
		}
		if after, err := c.Next(got, zurich); err != nil || !after.Equal(tc.wantAfter) {
			t.Errorf("%s: after %v, Next = %v, %v; want %v", tc.expr, got, after, err, tc.wantAfter)
		}
	}
}

// When the clocks go back, an hour comes twice. A schedule at a set time
// runs in the first; one that runs every hour runs in both.
func TestNextRepeatedHour(t *testing.T) {
	for _, tc := range []struct {
		zone, expr string
		from       time.Time
		want       []string
	}{
		{"Europe/Zurich", "30 2 * * *", time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), []string{
			"2026-10-25T02:30:00+02:00", "2026-10-26T02:30:00+01:00",
		}},
		{"America/New_York", "30 1 * * *", time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), []string{
			"2026-11-01T01:30:00-04:00", "2026-11-02T01:30:00-05:00",
		}},
		{"Europe/Zurich", "0 * * * *", time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC), []string{
			"2026-10-25T02:00:00+01:00", "2026-10-25T03:00:00+01:00",
		}},
		{"Europe/Zurich", "15,45 2 * * *", time.Date(2026, 10, 25, 0, 20, 0, 0, time.UTC), []string{
			"2026-10-25T02:45:00+02:00", "2026-10-26T02:15:00+01:00",
		}},
		// From before the repeated hour, as after the previous day's run.
		{"Europe/Zurich", "30 2 * * *", time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC), []string{
			"2026-10-25T02:30:00+02:00", "2026-10-26T02:30:00+01:00",
		}},
		{"Europe/Zurich", "30 2 * * 0", time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), []string{
			"2026-10-25T02:30:00+02:00", "2026-11-01T02:30:00+01:00",
		}},
		{"Europe/Zurich", "0 */2 * * *", time.Date(2026, 10, 24, 23, 30, 0, 0, time.UTC), []string{
			"2026-10-25T02:00:00+02:00", "2026-10-25T04:00:00+01:00",
		}},
		{"Europe/London", "30 1 * * *", time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC), []string{
			"2026-10-25T01:30:00+01:00", "2026-10-26T01:30:00Z",
		}},
		{"Australia/Sydney", "30 2 * * *", time.Date(2026, 4, 4, 0, 0, 0, 0, time.UTC), []string{
			"2026-04-05T02:30:00+11:00", "2026-04-06T02:30:00+10:00",
		}},
		{"America/New_York", "30 1 * * *", time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), []string{
			"2026-11-01T01:30:00-04:00", "2026-11-02T01:30:00-05:00",
		}},
	} {
		loc := mustLoc(t, tc.zone)
		c, err := Parse(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		at := tc.from
		for _, want := range tc.want {
			got, err := c.Next(at, loc)
			if err != nil || got.Format(time.RFC3339) != want {
				t.Errorf("%s in %s after %v: Next = %v, %v; want %s", tc.expr, tc.zone, at, got, err, want)
				break
			}
			at = got
		}
	}
}

// In these zones daylight saving skips midnight. Next must still move on to
// the following days, not spin on the evening before.
func TestNextSkippedMidnight(t *testing.T) {
	for _, tc := range []struct {
		zone, expr string
		from, want [5]int // year, month, day, hour, minute
	}{
		{"Atlantic/Azores", "0 9 * * 1-5", [5]int{2026, 3, 27, 10, 0}, [5]int{2026, 3, 30, 9, 0}},
		{"America/Havana", "0 9 * * 1-5", [5]int{2026, 3, 6, 10, 0}, [5]int{2026, 3, 9, 9, 0}},
		{"America/Santiago", "0 12 * * 0", [5]int{2026, 9, 5, 13, 0}, [5]int{2026, 9, 6, 12, 0}},
		// The day itself matches: it starts at 01:00.
		{"Atlantic/Azores", "30 1 * * *", [5]int{2026, 3, 28, 12, 0}, [5]int{2026, 3, 29, 1, 30}},
	} {
		loc := mustLoc(t, tc.zone)
		at := func(v [5]int) time.Time { return time.Date(v[0], time.Month(v[1]), v[2], v[3], v[4], 0, 0, loc) }
		c, err := Parse(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		var got time.Time
		go func() {
			defer close(done)
			got, err = c.Next(at(tc.from), loc)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s %s: Next does not return", tc.zone, tc.expr)
		}
		if err != nil || !got.Equal(at(tc.want)) {
			t.Errorf("%s %s: Next = %v, %v; want %v", tc.zone, tc.expr, got, err, at(tc.want))
		}
	}
}

func TestNever(t *testing.T) {
	c, err := Parse("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Next(time.Now(), time.UTC); err != ErrNever {
		t.Fatalf("err = %v", err)
	}
}

func TestParseRefuses(t *testing.T) {
	for _, expr := range []string{
		"", "* * * *", "60 * * * *", "* 24 * * *", "5-1 * * * *", "*/0 * * * *", "x * * * *",
		"1/9223372036854775807 * * * *", "*/60 * * * *",
	} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("%q: accepted", expr)
		}
	}
}

func TestDescribe(t *testing.T) {
	for expr, want := range map[string]string{
		"* * * * *":         "Every minute",
		"0 * * * *":         "Every hour",
		"15 * * * *":        "Every hour at 15 minutes past",
		"*/10 * * * *":      "Every 10 minutes",
		"0 8 * * *":         "Every day at 08:00",
		"30 7 * * 1-5":      "Every weekday at 07:30",
		"0 10 * * 0,6":      "Every weekend at 10:00",
		"0 9 * * 1,3":       "Every Monday and Wednesday at 09:00",
		"0 9 * * fri":       "Every Friday at 09:00",
		"0 6 1 * *":         "On day 1 of every month at 06:00",
		"0 6 1 1 *":         "Every year on 1 January at 06:00",
		"0 6 1 1,7 *":       "At 0 6 1 1,7 *",
		"@weekly":           "Every Sunday at 00:00",
		"0 8,17 * * *":      "At 0 8,17 * * *",
		"0 */2 * * *":       "Every 2 hours",
		"30 */6 * * *":      "Every 6 hours at 30 minutes past",
		"0 8-17/3 * * *":    "Every 3 hours from 08:00 to 17:00",
		"0 8-17 * * 1-5":    "Every hour between 08:00 and 18:00, on weekdays",
		"*/30 8-17 * * 1-5": "Every 30 minutes between 08:00 and 18:00, on weekdays",
		"*/15 * * * 1,5":    "Every 15 minutes, on Monday and Friday",
		"*/15 * 1 * *":      "At */15 * 1 * *",
	} {
		c, err := Parse(expr)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if got := c.Describe(); got != want {
			t.Errorf("%s: %q, want %q", expr, got, want)
		}
	}
}

func TestNextRun(t *testing.T) {
	if _, err := NextRun("0 8 * * *", "Mars/Olympus", time.Now()); err == nil {
		t.Error("accepted an unknown time zone")
	}
	if _, err := NextRun("0 8 * *", "UTC", time.Now()); err == nil {
		t.Error("accepted four fields")
	}
}
