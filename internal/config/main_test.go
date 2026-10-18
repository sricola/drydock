package config

import (
	"os"
	"strings"
	"testing"
)

// TestMain strips every operator knob config reads from the environment before
// the suite runs. Load applies DRYDOCK_* (and the legacy SANDBOX_IMAGE,
// AUDIT_ROOT, STAGE_ROOT, BROKER_*, SQUID_RUN_DIR) overrides on top of the
// file, so a developer shell with, say, DRYDOCK_MAX_CONCURRENT_TASKS exported
// would silently change what these tests load. Tests that want an override
// set it themselves with t.Setenv.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "DRYDOCK_"):
		case k == "SANDBOX_IMAGE", k == "AUDIT_ROOT", k == "STAGE_ROOT",
			k == "BROKER_SOCKET", k == "BROKER_ADDR", k == "SQUID_RUN_DIR", k == "EGRESS_CONFIG":
		default:
			continue
		}
		os.Unsetenv(k)
	}
	os.Exit(m.Run())
}
