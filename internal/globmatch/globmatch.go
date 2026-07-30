// Package globmatch implements a **-aware glob matcher for forward-slash,
// repo-relative paths. Patterns are split on "/": a "**" segment spans any
// number of path segments (including zero when mid-pattern), while every
// other segment must match exactly one name segment via path.Match
// semantics (*, ?, [...] — never crossing a "/"). Matching is
// case-sensitive. A trailing "/**" matches only descendants, not the
// directory itself (gitignore-style): "a/**" matches "a/b" but not "a".
package globmatch

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// Valid reports whether pattern is a well-formed glob. It rejects the empty
// pattern and any pattern containing a segment that path.Match cannot
// compile (path.ErrBadPattern). "**" segments are always allowed.
func Valid(pattern string) error {
	if pattern == "" {
		return errors.New("globmatch: empty pattern")
	}
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "**" {
			continue
		}
		// path.Match's only possible error is ErrBadPattern; matching
		// against "" still validates the full segment syntax.
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("globmatch: bad pattern %q: %w", pattern, err)
		}
	}
	return nil
}

// Match reports whether name (a clean, forward-slash repo-relative path)
// matches pattern. Malformed patterns never match and never panic.
func Match(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	pat := strings.Split(pattern, "/")
	// Collapse consecutive "**" segments; they are equivalent to one.
	collapsed := pat[:0]
	for _, seg := range pat {
		if seg == "**" && len(collapsed) > 0 && collapsed[len(collapsed)-1] == "**" {
			continue
		}
		collapsed = append(collapsed, seg)
	}
	pat = collapsed
	segs := strings.Split(name, "/")

	// memo[pi*(len(segs)+1)+ni] caches results for state (pi, ni), bounding
	// the recursion at O(len(pat) * len(segs)) states so stacked ** patterns
	// cannot blow up.
	memo := make([]int8, (len(pat)+1)*(len(segs)+1))
	return match(pat, segs, 0, 0, memo)
}

// match reports whether pat[pi:] matches segs[ni:]. memo holds 0 (unknown),
// 1 (true), or -1 (false) per (pi, ni) state.
func match(pat, segs []string, pi, ni int, memo []int8) bool {
	key := pi*(len(segs)+1) + ni
	if memo[key] != 0 {
		return memo[key] > 0
	}
	res := func() bool {
		if pi == len(pat) {
			return ni == len(segs)
		}
		if pat[pi] == "**" {
			if pi == len(pat)-1 {
				// Trailing "**" matches descendants only: at least one
				// name segment must remain.
				return ni < len(segs)
			}
			for k := ni; k <= len(segs); k++ {
				if match(pat, segs, pi+1, k, memo) {
					return true
				}
			}
			return false
		}
		if ni == len(segs) {
			return false
		}
		// path.Match errors only on malformed patterns; treat as no match.
		ok, err := path.Match(pat[pi], segs[ni])
		if err != nil || !ok {
			return false
		}
		return match(pat, segs, pi+1, ni+1, memo)
	}()
	if res {
		memo[key] = 1
	} else {
		memo[key] = -1
	}
	return res
}
