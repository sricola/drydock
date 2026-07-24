package gateway

import (
	"sync"
	"time"
)

// spendLedger tracks time-stamped USD spend per vendor for the aggregate budget
// cap. window == 0 is total mode (no time decay). It has its own mutex; callers
// may hold the Gateway mutex when calling in, but this lock is never acquired
// before the Gateway's, so there is no lock-order inversion.
type spendLedger struct {
	mu     sync.Mutex
	window time.Duration
	// Windowed mode retains time-stamped entries so aged-out spend can be pruned.
	byVendor map[string][]ledgerEntry
	// Total mode (window == 0) keeps only a running per-vendor sum: there is no
	// decay, so per-entry retention would grow unbounded (one entry per metered
	// response) over a long-lived broker and make every admit re-sum an
	// ever-growing slice.
	total map[string]float64
}

type ledgerEntry struct {
	ts  time.Time
	usd float64
}

func newSpendLedger(window time.Duration) *spendLedger {
	return &spendLedger{window: window, byVendor: map[string][]ledgerEntry{}, total: map[string]float64{}}
}

func (l *spendLedger) add(vendor string, usd float64, ts time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.window == 0 {
		l.total[vendor] += usd
		return
	}
	l.byVendor[vendor] = append(l.byVendor[vendor], ledgerEntry{ts: ts, usd: usd})
}

// windowed returns the vendor's spend within the window (or the running total in
// total mode) and prunes aged-out entries in place.
func (l *spendLedger) windowed(vendor string, now time.Time) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.window == 0 {
		return l.total[vendor]
	}
	entries := l.byVendor[vendor]
	cutoff := now.Add(-l.window)
	kept := entries[:0]
	var sum float64
	for _, e := range entries {
		if e.ts.After(cutoff) {
			kept = append(kept, e)
			sum += e.usd
		}
	}
	l.byVendor[vendor] = kept
	return sum
}
