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
	c.mu.Lock()
	if c.fired {
		c.mu.Unlock()
		return len(p), nil
	}
	c.n += int64(len(p))
	crossed := c.n > c.max
	if crossed {
		c.fired = true
	}
	c.mu.Unlock()
	if crossed {
		if c.onExceed != nil {
			c.onExceed()
		}
		return len(p), nil
	}
	return cw.w.Write(p)
}
