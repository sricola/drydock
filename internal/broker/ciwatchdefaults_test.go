package broker

import (
	"testing"

	"drydock/internal/config"
)

// The watcher's built-in fallbacks and the values the shipped config documents
// must be the same numbers. They live in two packages by necessity (config
// cannot import broker — broker imports config), so nothing but this test stops
// them drifting, and drift here means `~/.drydock/config.yaml` documents a poll
// cadence and a watch deadline the daemon does not actually use.
func TestCIWatchDefaultsMatchConfigDefaults(t *testing.T) {
	if defaultCIPollInterval != config.DefaultCIPollInterval {
		t.Errorf("broker defaultCIPollInterval = %v but config.DefaultCIPollInterval = %v; the seeded config would document a cadence the daemon does not use",
			defaultCIPollInterval, config.DefaultCIPollInterval)
	}
	if defaultCIWatchTimeout != config.DefaultCIWatchTimeout {
		t.Errorf("broker defaultCIWatchTimeout = %v but config.DefaultCIWatchTimeout = %v; the seeded config would document a deadline the daemon does not use",
			defaultCIWatchTimeout, config.DefaultCIWatchTimeout)
	}
	// The DISPATCH FLOOR terms matter for the same reason and one more: config's
	// copy is what validate() uses to reject a poll/timeout pair that could
	// never conclude. If the broker's floor grew past config's, a pair config
	// accepted would still dead-letter every watch — the exact failure that
	// cross-field check exists to make impossible.
	if ciDispatchFloorPolls != config.CIDispatchFloorPolls {
		t.Errorf("broker ciDispatchFloorPolls = %d but config.CIDispatchFloorPolls = %d; config would validate a ci.watch_timeout the watcher can never conclude within",
			ciDispatchFloorPolls, config.CIDispatchFloorPolls)
	}
	if ciDispatchFloorGrace != config.CIDispatchFloorGrace {
		t.Errorf("broker ciDispatchFloorGrace = %v but config.CIDispatchFloorGrace = %v; config would validate a ci.watch_timeout the watcher can never conclude within",
			ciDispatchFloorGrace, config.CIDispatchFloorGrace)
	}
	// The configured minimum must be reachable: a poll interval the operator
	// is allowed to set must not be one the watcher would treat as "unset".
	if config.MinCIPollInterval <= 0 {
		t.Errorf("config.MinCIPollInterval = %v; a non-positive floor would let 0 (= built-in default) and a real value collide",
			config.MinCIPollInterval)
	}
}
