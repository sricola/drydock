package broker

// Thin test-only conveniences over production entry points. They used to live
// in production files with no production caller; keeping them here keeps the
// production surface honest about what the daemon actually calls.

// globalCeilingExceeded is globalCeilingExceededDetail without the
// `unmeasured` distinction.
func (b *Broker) globalCeilingExceeded(agent string) (blocked bool, reason string) {
	blocked, reason, _ = b.globalCeilingExceededDetail(agent)
	return blocked, reason
}

// inFlightStarts is how many admitted-but-unreleased starts this process holds.
func (b *Broker) inFlightStarts() int {
	b.capMu.Lock()
	defer b.capMu.Unlock()
	return len(b.capInFlight)
}

// cacheAcquire takes the in-use refcount for one entry dir under b.cacheMu.
func (b *Broker) cacheAcquire(dir string) {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	b.cacheAcquireLocked(dir)
}

// sanitizeUntrustedText is sanitizeUntrustedTextLimit with no limit (the
// output can never be longer than the input).
func sanitizeUntrustedText(s string) string {
	return sanitizeUntrustedTextLimit(s, len(s))
}
