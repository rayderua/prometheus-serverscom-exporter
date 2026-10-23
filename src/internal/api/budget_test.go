package api

import (
	"context"
	"testing"
	"time"
)

func TestBudgetDoesNotDelayRequestsWithinTheWindow(t *testing.T) {
	b := newBudget(50, time.Hour)
	start := time.Now()
	for range 39 {
		if err := b.wait(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("39 requests within the budget took %v, want no delay", elapsed)
	}
	if s := b.state(time.Now()); s.Used != 39 || s.Remaining != 11 || s.Exhausted {
		t.Fatalf("state = %+v", s)
	}
}

func TestBudgetWaitsForTheHourlyReset(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	b := newBudget(2, time.Hour)

	for i := range 2 {
		if delay, _, _ := b.reserve(now.Add(time.Duration(i) * time.Minute)); delay != 0 {
			t.Fatalf("request %d delayed by %v", i, delay)
		}
	}

	delay, first, state := b.reserve(now.Add(10 * time.Minute))
	if delay != 50*time.Minute || !first || !state.Exhausted || !state.Reset.Equal(now.Add(time.Hour)) {
		t.Fatalf("delay = %v first = %v state = %+v", delay, first, state)
	}
	// Further callers in the same window wait too, but are not "first".
	if _, first, _ := b.reserve(now.Add(11 * time.Minute)); first {
		t.Fatal("exhaustion reported twice in one window")
	}

	// A new window opens at the first request after the reset.
	if delay, _, state := b.reserve(now.Add(61 * time.Minute)); delay != 0 || state.Used != 1 || state.Exhausted {
		t.Fatalf("after reset: delay = %v state = %+v", delay, state)
	}
}

func TestBudgetStateWithoutOpenWindow(t *testing.T) {
	b := newBudget(10, time.Hour)
	if s := b.state(time.Now()); s.Remaining != 10 || !s.Reset.IsZero() {
		t.Fatalf("state = %+v", s)
	}
}

func TestBudgetWaitReportsExhaustionAndResumes(t *testing.T) {
	b := newBudget(1, 30*time.Millisecond)
	if err := b.wait(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	calls := 0
	if err := b.wait(context.Background(), func(budgetState) { calls++ }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("onExhausted called %d times, want 1", calls)
	}
}

func TestBudgetWaitHonoursContext(t *testing.T) {
	b := newBudget(1, time.Hour)
	if err := b.wait(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.wait(ctx, nil); err == nil {
		t.Fatal("expected a context error while the budget is used up")
	}
}

func TestBudgetAlignEndsWithTheAccountWindow(t *testing.T) {
	b := newBudget(10, time.Hour)
	now := time.Now()
	b.reserve(now)
	b.align(now.Add(10*time.Minute), now)
	if s := b.state(now); !s.Reset.Equal(now.Add(10*time.Minute)) || s.Used != 1 {
		t.Fatalf("state = %+v, want the window to end with the account window", s)
	}
}

func TestBudgetStartsOverWithTheAccountWindow(t *testing.T) {
	b := newBudget(10, time.Hour)
	now := time.Now()
	for range 3 {
		b.reserve(now)
	}
	b.align(now.Add(5*time.Minute), now)

	// The account window resets; two loops start requests before the first
	// response reports the new reset. Both count in the new window.
	after := now.Add(5*time.Minute + time.Second)
	b.reserve(after)
	b.reserve(after)
	b.align(now.Add(65*time.Minute), after)

	if s := b.state(after); s.Used != 2 || !s.Reset.Equal(now.Add(65*time.Minute)) {
		t.Fatalf("state = %+v, want 2 used until the new account reset", s)
	}
}

func TestBudgetIgnoresPastAndFarResets(t *testing.T) {
	b := newBudget(10, time.Hour)
	now := time.Now()
	b.reserve(now)
	b.align(now.Add(-time.Minute), now)
	b.align(now.Add(2*time.Hour), now)
	if s := b.state(now); !s.Reset.Equal(now.Add(time.Hour)) {
		t.Fatalf("reset = %v, want unchanged", s.Reset)
	}
}
