package runner

import (
	"fmt"
	"strings"
)

// containerSystemDown classifies a failed `container network ls` probe: true
// only for the "system service not running" signatures, which
// `container system start` fixes. Any other failure (CLI missing, permission)
// is a different problem the caller must surface, not silently "fix".
func containerSystemDown(out string) bool {
	return strings.Contains(out, "XPC connection") || strings.Contains(out, "system service")
}

// EnsureContainerSystem probes the Apple `container` system service and
// starts it when down. run executes the container CLI with the given args and
// returns combined output; notify fires once, before a start is attempted
// (kernel install on first run can take a while — callers surface that).
// Returns started=true only when it had to start the service.
func EnsureContainerSystem(run func(args ...string) (string, error), notify func(string)) (started bool, err error) {
	out, perr := run("network", "ls")
	if perr == nil {
		return false, nil
	}
	if !containerSystemDown(out) {
		return false, fmt.Errorf("container runtime probe failed: %s", strings.TrimSpace(out))
	}
	notify("container system not running — starting (may install kernel on first run)…")
	if sout, serr := run("system", "start", "--enable-kernel-install"); serr != nil {
		return true, fmt.Errorf("container system start: %s", strings.TrimSpace(sout))
	}
	return true, nil
}
