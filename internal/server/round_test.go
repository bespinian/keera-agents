package server

import (
	"context"
	"testing"
	"time"
)

func TestStepHasItsOwnDeadline(t *testing.T) {
	defer func(d time.Duration) { stepTimeout = d }(stepTimeout)
	stepTimeout = 10 * time.Millisecond
	ctx := context.Background()
	if !step(ctx, func(context.Context) {}) {
		t.Fatal("a quick step did not finish")
	}
	// A step that runs out of time says so, and the next gets time again.
	if step(ctx, func(ctx context.Context) { <-ctx.Done() }) {
		t.Fatal("a step that ran out of time finished")
	}
	if !step(ctx, func(ctx context.Context) {
		if ctx.Err() != nil {
			t.Error("the next step started out of time")
		}
	}) {
		t.Fatal("the next step did not finish")
	}
}
