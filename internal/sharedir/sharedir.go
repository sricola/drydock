// Package sharedir locates files in the drydock share-dir layout
// (Homebrew / make install: $PREFIX/share/drydock/<rel>; dev checkout:
// <rel> relative to the working directory).
//
// Both cmd/drydock and cmd/brokerd use the same search order so users and
// operators always get consistent results regardless of which binary is running.
package sharedir

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Locate returns the first path among the standard share-dir candidates
// (in priority order) that exists on the filesystem:
//
//  1. <binary>/../share/drydock/<rel>  (installed binary: brew, make install)
//  2. $HOMEBREW_PREFIX/share/drydock/<rel>
//  3. <rel>                             (CWD / repo checkout)
//
// rel must be a slash-separated relative path such as "config/egress.yaml" or
// "image/Dockerfile". An error is returned when none of the candidates exist.
func Locate(rel string) (string, error) {
	candidates := Candidates(rel)
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("not found in share dirs; tried: %s", strings.Join(candidates, ", "))
}

// Candidates returns the share-dir candidate paths for rel in priority order
// (binary-relative, $HOMEBREW_PREFIX, CWD) without checking existence. Callers
// that need a custom existence check (e.g. checking for a Dockerfile inside a
// directory rather than the directory itself) can iterate this list directly.
func Candidates(rel string) []string {
	var out []string
	if self, err := os.Executable(); err == nil {
		// e.g. /opt/homebrew/bin/drydock -> /opt/homebrew/share/drydock/<rel>
		root := filepath.Dir(filepath.Dir(filepath.Clean(self)))
		out = append(out, filepath.Join(root, "share", "drydock", rel))
	}
	if hb := os.Getenv("HOMEBREW_PREFIX"); hb != "" {
		out = append(out, filepath.Join(hb, "share", "drydock", rel))
	}
	out = append(out, rel)
	return out
}
