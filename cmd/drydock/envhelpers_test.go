package main

import (
	"os"
	"testing"
)

// unsetenv removes key for the duration of the test and restores the caller's
// value afterwards. os.Unsetenv alone leaks across tests in the same process:
// a developer's real ANTHROPIC_API_KEY would stay cleared for every later
// test, and the result then depends on test order.
func unsetenv(t *testing.T, key string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unsetenv %s: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv(key, prev)
		}
	})
}
