package broker

import (
	"io"
	"sync"
)

// maxTaskOutputBytes caps the total bytes an untrusted task may emit to the host
// across stdout+stderr. A real coding task's output is well under this; the cap
// stops a `yes`/flood from filling the audit log and the daemon's stdout. 256
// MiB is generous headroom while bounding the disk a single task can consume.
// A var (not const) only so tests can lower it; nothing in production writes it.
var maxTaskOutputBytes int64 = 256 << 20

// outputCap bounds combined task output. Once the running total exceeds max it
// stops forwarding (so the flood cannot reach the audit file or daemon stdout)
// and invokes onExceed once (used to cancel the task). Safe for the concurrent
// stdout and stderr writers of an exec.Cmd.
type outputCap struct {
	mu       sync.Mutex
	n, max   int64
	onExceed func()
	fired    bool
}

func newOutputCap(max int64, onExceed func()) *outputCap {
	return &outputCap{max: max, onExceed: onExceed}
}

// wrap returns a writer that forwards to w while charging bytes against the
// shared budget. After the budget is exceeded, writes are swallowed (reported as
// fully written so the copy loop does not error) and onExceed fires once.
func (c *outputCap) wrap(w io.Writer) io.Writer { return capWriter{c: c, w: w} }

// exceeded reports whether the budget was crossed (the task should be treated as
// terminated for output flooding).
func (c *outputCap) exceeded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fired
}

type capWriter struct {
	c *outputCap
	w io.Writer
}

func (cw capWriter) Write(p []byte) (int, error) {
	c := cw.c
	// The lock is held across the forwarded write, not just the counter update:
	// an exec.Cmd's stdout and stderr are copied by two goroutines, and both
	// wrap the SAME audit file, so releasing before forwarding would let their
	// writes interleave mid-line and corrupt the stream-json .jsonl that
	// audit.Reason/TotalCost later parse. Serializing is cheap (a local-file
	// append); onExceed is a context cancel, non-reentrant, safe under the lock.
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fired {
		return len(p), nil
	}
	c.n += int64(len(p))
	if c.n > c.max {
		c.fired = true
		if c.onExceed != nil {
			c.onExceed()
		}
		return len(p), nil
	}
	return cw.w.Write(p)
}
