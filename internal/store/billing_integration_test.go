package store

import (
	"context"
	"testing"
	"time"
)

// perSecond meters every machine at one millicore, MiB of memory and MiB of
// disk a second, so a total in each column is the seconds metered.
func perSecond(_ Agent, seconds int64) MachineUse {
	return MachineUse{CPUMilliSeconds: seconds, MemoryMiBSeconds: seconds, DiskMiBSeconds: seconds}
}

func TestAgentUsage(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a := newAgent(t, st, "org_a", "Scout")
	other := newAgent(t, st, "org_a", "Ranger")
	ten := time.Date(2026, 11, 17, 10, 30, 0, 0, time.UTC)
	rounds := []time.Time{ten, ten.Add(10 * time.Minute), ten.Add(110 * time.Minute), ten.Add(2 * time.Hour)}
	for _, at := range rounds {
		if _, err := st.Meter(ctx, at, 15*time.Minute, time.Minute, perSecond); err != nil {
			t.Fatal(err)
		}
	}
	// 10 minutes, then 15 for the gap, go to 10:00; the last 10 to 12:00.
	u, err := st.AgentUsage(ctx, a.ID, ten.Truncate(time.Hour))
	if err != nil || u.CPUMilliSeconds != 35*60 {
		t.Errorf("since 10:00: %+v, %v", u, err)
	}
	if u, err = st.AgentUsage(ctx, a.ID, ten.Add(time.Hour)); err != nil || u.CPUMilliSeconds != 10*60 {
		t.Errorf("since 11:30: %+v, %v", u, err)
	}
	if u, err = st.AgentUsage(ctx, other.ID, ten.Add(3*time.Hour)); err != nil || !u.Zero() {
		t.Errorf("nothing since: %+v, %v", u, err)
	}
}

// Rounds rarely come on the whole second; what is left over counts later.
func TestMeterKeepsPartsOfASecond(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	newAgent(t, st, "org_a", "Scout")
	ten := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	var total int64
	for i, at := range []time.Duration{0, 59600 * time.Millisecond, 119200 * time.Millisecond, 180 * time.Second} {
		got, err := st.Meter(ctx, ten.Add(at), 15*time.Minute, time.Minute, perSecond)
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		total += got
	}
	if total != 180 {
		t.Errorf("metered %d seconds in 3 minutes", total)
	}
}

func TestMeterCountsEachSecondOnceIntoItsHour(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	a := newAgent(t, st, "org_a", "Scout")
	b := newAgent(t, st, "org_a", "Ranger")
	c := newAgent(t, st, "org_b", "Scout")
	gone := newAgent(t, st, "org_b", "Gone")
	if err := st.DeleteAgent(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	ten := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	meter := func(at time.Time, want int64) {
		t.Helper()
		if got, err := st.Meter(ctx, at, 15*time.Minute, time.Minute, perSecond); err != nil || got != want {
			t.Fatalf("Meter at %s = %d, %v; want %d", at.Format(time.TimeOnly), got, err, want)
		}
	}

	meter(ten.Add(58*time.Minute), 0) // the first round only sets the mark
	meter(ten.Add(59*time.Minute), 60)
	meter(ten.Add(59*time.Minute), 0) // no time has passed
	if hours, err := st.UnbilledHours(ctx, ten.Add(-time.Hour), 10); err != nil || len(hours) != 0 {
		t.Fatalf("an hour still metered is not billed yet: %+v, %v", hours, err)
	}
	// This minute started in the 10:00 hour, so it goes there, and ends it.
	meter(ten.Add(60*time.Minute), 60)
	// A gap counts as at most 15 minutes.
	meter(ten.Add(2*time.Hour), 15*60)

	hours, err := st.UnbilledHours(ctx, ten.Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	// 10:00 and 11:00 are complete for both organisations, oldest first.
	if len(hours) != 4 {
		t.Fatalf("hours = %+v; want 10:00 and 11:00 for org_a and org_b", hours)
	}
	for i, h := range hours {
		machines := map[string]int64{"org_a": 2, "org_b": 1}[h.OrgID]
		hour, seconds := ten, int64(120)
		if i >= 2 {
			hour, seconds = ten.Add(time.Hour), 15*60
		}
		if !h.Hour.Equal(hour) || h.CPUMilliSeconds != seconds*machines || h.DiskMiBSeconds != seconds*machines {
			t.Errorf("%+v; want %d seconds at %s for each of %d machines", h, seconds, hour, machines)
		}
	}

	if err := st.MarkBilled(ctx, hours[:1]); err != nil {
		t.Fatal(err)
	}
	left, err := st.UnbilledHours(ctx, ten.Add(-time.Hour), 10)
	if err != nil || len(left) != 3 || left[0].OrgID != hours[1].OrgID || !left[0].Hour.Equal(ten) {
		t.Fatalf("after billing one: %+v, %v", left, err)
	}
	if old, err := st.UnbilledHours(ctx, ten.Add(2*time.Hour), 10); err != nil || len(old) != 0 {
		t.Fatalf("hours before since are left out: %+v, %v", old, err)
	}

	month, err := st.MachineUsage(ctx, "org_a", ten.Add(-time.Hour), ten.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(month) != 2 || month[0].AgentID != b.ID || month[1].AgentID != a.ID {
		t.Fatalf("per machine, by name: %+v", month)
	}
	// 10:00 has 120 seconds, 11:00 the 15 minutes of the gap.
	if month[0].CPUMilliSeconds != 120+15*60 || month[0].Name != "Ranger" || month[0].Kind != KindAgent {
		t.Errorf("Ranger: %+v", month[0])
	}
	if other, err := st.MachineUsage(ctx, "org_b", ten, ten.Add(3*time.Hour)); err != nil ||
		len(other) != 1 || other[0].AgentID != c.ID {
		t.Errorf("org_b, without the machine deleted before metering: %+v, %v", other, err)
	}

	if err := st.PruneUsage(ctx, ten.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if month, _ = st.MachineUsage(ctx, "org_a", ten.Add(-time.Hour), ten.Add(3*time.Hour)); len(month) != 2 ||
		month[0].CPUMilliSeconds != 15*60 {
		t.Errorf("after pruning 10:00: %+v", month)
	}
}

// whileRunning meters a millicore a second while the machine runs, so CPU
// is the seconds billed for running.
func whileRunning(a Agent, seconds int64) MachineUse {
	if a.State != AgentRunning {
		return MachineUse{}
	}
	return MachineUse{CPUMilliSeconds: seconds}
}

func TestMeterBillsEachRunAtLeastAMinute(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	set := func(a Agent, states ...string) {
		t.Helper()
		for _, s := range states {
			if err := st.SetAgentState(ctx, a.ID, s, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	meter := func(at time.Time) {
		t.Helper()
		if _, err := st.Meter(ctx, at, 15*time.Minute, time.Minute, whileRunning); err != nil {
			t.Fatal(err)
		}
	}
	early := newAgent(t, st, "org_a", "Early")
	short := newAgent(t, st, "org_a", "Short")
	twice := newAgent(t, st, "org_a", "Twice")
	seen := newAgent(t, st, "org_a", "Seen")
	back := newAgent(t, st, "org_a", "Back")
	idle := newAgent(t, st, "org_a", "Idle")
	ten := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)

	// Runs before metering started are not billed.
	set(early, AgentRunning, AgentStopped)
	meter(ten)
	set(short, AgentStarting, AgentRunning, AgentStopped)
	set(twice, AgentRunning, AgentStopped, AgentRunning, AgentSuspended)
	// Running again while running is the same run.
	set(seen, AgentRunning, AgentRunning)
	set(back, AgentRunning, AgentStopped, AgentRunning)
	meter(ten.Add(time.Minute))
	// The runs are billed once: only what still runs counts now.
	meter(ten.Add(2 * time.Minute))

	for _, c := range []struct {
		a    Agent
		want int64
	}{
		{early, 0},
		{short, 60},       // a run no round saw
		{twice, 2 * 60},   // two of them
		{seen, 2 * 60},    // two rounds of running, no more
		{back, 60 + 2*60}, // a run no round saw, then two rounds of running
		{idle, 0},
	} {
		u, err := st.AgentUsage(ctx, c.a.ID, ten)
		if err != nil || u.CPUMilliSeconds != c.want {
			t.Errorf("%s: %+v, %v; want %d seconds", c.a.Name, u, err, c.want)
		}
	}
}
