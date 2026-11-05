package fleet

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryDueBacksOff(t *testing.T) {
	m := New(nil, nil, nil, nil, Options{})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if !m.retryDue("agt", now) {
		t.Fatal("the first retry is due at once")
	}
	var waits []time.Duration
	at := now
	for range 8 {
		next := m.retries["agt"].at
		if m.retryDue("agt", next.Add(-time.Second)) {
			t.Fatalf("retry due before %v", next)
		}
		if !m.retryDue("agt", next) {
			t.Fatalf("retry not due at %v", next)
		}
		waits = append(waits, next.Sub(at))
		at = next
	}
	want := []time.Duration{
		30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		10 * time.Minute, 10 * time.Minute, 10 * time.Minute,
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("wait %d = %v, want %v", i, waits[i], want[i])
		}
	}

	if !m.retryDue("other", now) {
		t.Error("agents back off on their own")
	}
}

func TestStuckAfterAnHourPending(t *testing.T) {
	m := New(nil, nil, nil, nil, Options{})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if m.stuck("agt", true, now) || m.stuck("agt", true, now.Add(maxStartWait)) {
		t.Fatal("stuck before the wait is over")
	}
	if !m.stuck("agt", true, now.Add(maxStartWait+time.Minute)) {
		t.Fatal("not stuck after the wait")
	}
	// Starting, or having no task, resets the clock.
	if m.stuck("agt", false, now.Add(2*maxStartWait)) || m.stuck("agt", true, now.Add(2*maxStartWait)) {
		t.Fatal("the clock did not reset")
	}
	if m.stuck("other", true, now.Add(3*maxStartWait)) {
		t.Fatal("agents share a clock")
	}
}

func TestLockGivesUpWhenTheCallerDoes(t *testing.T) {
	m := New(nil, nil, nil, nil, Options{})
	unlock, err := m.lock(context.Background(), "agt")
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := m.lock(short, "agt"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock = %v", err)
	}
	// Another agent's lock is free.
	other, err := m.lock(context.Background(), "agt_other")
	if err != nil {
		t.Fatal(err)
	}
	other()
	unlock()
	again, err := m.lock(context.Background(), "agt")
	if err != nil {
		t.Fatal(err)
	}
	again()
}
