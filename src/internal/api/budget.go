package api

import (
	"context"
	"sync"
	"time"
)

// budget is the exporter's own request limit: at most limit requests per
// window, counted the same way as the API rate limit.
//
// The window follows the account rate limit window: align moves its end to
// X-RateLimit-Reset, and when the account window resets, the budget starts
// over too. Until the API has reported a reset (or after it has passed), a
// window opens with the first request and lasts one window length.
//
// Requests are never delayed while the window has room; once it is used up,
// callers wait until the window resets.
type budget struct {
	limit  int
	window time.Duration

	mu           sync.Mutex
	end          time.Time // end of the current window; zero before the first request
	accountReset time.Time // last X-RateLimit-Reset passed to align
	sinceReset   int       // requests reserved since accountReset passed
	used         int
	exhausted    bool // the current window ran out at least once
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

	if !now.Before(b.end) {
		b.end, b.used, b.exhausted = now.Add(b.window), 0, false
	}
	if b.used < b.limit {
		b.used++
		if !b.accountReset.IsZero() && !now.Before(b.accountReset) {
			b.sinceReset++
		}
		return 0, false, b.stateLocked(now)
	}

	firstExhaustion = !b.exhausted
	b.exhausted = true
	return b.end.Sub(now), firstExhaustion, b.stateLocked(now)
}

// align makes the window end when the account rate limit window resets.
// reset is X-RateLimit-Reset of a response received at now.
//
//   - the previously reported reset has passed: the account window has reset,
//     so the budget starts over and ends with the new account window; the
//     requests reserved since the reset are counted in it;
//   - reset before the end of the budget window: the budget window opened in
//     the middle of the account window and now ends with it.
//
// Resets in the past or more than one window ahead are ignored.
func (b *budget) align(reset, now time.Time) {
	if !reset.After(now) || reset.After(now.Add(b.window)) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	prev := b.accountReset
	if !reset.Equal(prev) {
		b.accountReset = reset
		defer func() { b.sinceReset = 0 }()
	}
	switch {
	case !now.Before(b.end):
		// No open window: the next request opens one ending at the reset.
		b.end, b.used, b.exhausted = reset, 0, false
	case !prev.IsZero() && !now.Before(prev):
		b.end, b.used, b.exhausted = reset, max(b.sinceReset, 1), false
	case reset.Before(b.end):
		b.end = reset
	}
}

// state returns the current window state.
func (b *budget) state(now time.Time) budgetState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stateLocked(now)
}

func (b *budget) stateLocked(now time.Time) budgetState {
	if !now.Before(b.end) || (b.used == 0 && !b.exhausted) {
		return budgetState{Remaining: b.limit}
	}
	return budgetState{
		Used:      b.used,
		Remaining: b.limit - b.used,
		Reset:     b.end,
		Exhausted: b.exhausted,
	}
}
