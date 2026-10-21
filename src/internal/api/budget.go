package api

import (
	"context"
	"sync"
	"time"
)

// budget is the exporter's own request limit: at most limit requests per
// window, counted the same way as the API rate limit.
//
// A window opens with the first request after the previous one has ended.
// Requests are never delayed while the window has room; once it is used up,
// callers wait until the window resets.
type budget struct {
	limit  int
	window time.Duration

	mu        sync.Mutex
	start     time.Time // start of the current window; zero before the first request
	used      int
	exhausted bool // the current window ran out at least once
}

func newBudget(limit int, window time.Duration) *budget {
	return &budget{limit: limit, window: window}
}

// budgetState is a snapshot of the current window.
type budgetState struct {
	Used      int
	Remaining int
	Reset     time.Time // zero when no window is open
	Exhausted bool
}

// wait blocks until a request may start and counts it. onExhausted is called
// once per window, by the first caller that finds it used up.
func (b *budget) wait(ctx context.Context, onExhausted func(budgetState)) error {
	for {
		delay, first, state := b.reserve(time.Now())
		if delay <= 0 {
			return ctx.Err()
		}
		if first && onExhausted != nil {
			onExhausted(state)
		}
		if err := sleep(ctx, delay); err != nil {
			return err
		}
	}
}

// reserve counts a request starting at now if the window has room. Otherwise
// it returns the time until the window resets and whether this call is the
// first one to find the window used up.
func (b *budget) reserve(now time.Time) (delay time.Duration, firstExhaustion bool, state budgetState) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.start.IsZero() || !now.Before(b.start.Add(b.window)) {
		b.start, b.used, b.exhausted = now, 0, false
	}
	if b.used < b.limit {
		b.used++
		return 0, false, b.stateLocked(now)
	}

	firstExhaustion = !b.exhausted
	b.exhausted = true
	return b.start.Add(b.window).Sub(now), firstExhaustion, b.stateLocked(now)
}

// state returns the current window state.
func (b *budget) state(now time.Time) budgetState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stateLocked(now)
}

func (b *budget) stateLocked(now time.Time) budgetState {
	if b.start.IsZero() || !now.Before(b.start.Add(b.window)) {
		return budgetState{Remaining: b.limit}
	}
	return budgetState{
		Used:      b.used,
		Remaining: b.limit - b.used,
		Reset:     b.start.Add(b.window),
		Exhausted: b.exhausted,
	}
}
