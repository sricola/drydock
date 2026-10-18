package globmatch

import (
	"math/rand"
	"path"
	"strings"
	"testing"
)

// refMatch is a deliberately naive, memo-free matcher with the same documented
// semantics: "/"-separated segments, "**" spans zero or more segments except
// that a trailing "**" needs at least one, other segments use path.Match.
func refMatch(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		if len(pat) == 1 {
			return len(segs) >= 1
		}
		for k := 0; k <= len(segs); k++ {
			if refMatch(pat[1:], segs[k:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return refMatch(pat[1:], segs[1:])
}

// Match must agree with the reference on random patterns and names. The memo
// is an optimisation only: a key collision or a wrong boundary in the "**"
// expansion would show up as a disagreement here.
func TestMatch_AgreesWithReferenceOnRandomInputs(t *testing.T) {
	rnd := rand.New(rand.NewSource(20261013))
	patAlphabet := []string{"a", "b", "*", "**", "a*", "*b", "?", "a?b", "ab"}
	nameAlphabet := []string{"a", "b", "ab", "ba", "aab", "abb", "c", "axb"}
	checked := 0
	for i := 0; i < 30000; i++ {
		np := 1 + rnd.Intn(5)
		ps := make([]string, np)
		for j := range ps {
			ps[j] = patAlphabet[rnd.Intn(len(patAlphabet))]
		}
		nn := rnd.Intn(6)
		ns := make([]string, nn)
		for j := range ns {
			ns[j] = nameAlphabet[rnd.Intn(len(nameAlphabet))]
		}
		pattern, name := strings.Join(ps, "/"), strings.Join(ns, "/")
		if name == "" {
			continue // Match splits "" into one empty segment; the reference has no such input
		}
		// The reference sees the same collapsed pattern Match documents
		// (consecutive "**" are one).
		var collapsed []string
		for _, s := range ps {
			if s == "**" && len(collapsed) > 0 && collapsed[len(collapsed)-1] == "**" {
				continue
			}
			collapsed = append(collapsed, s)
		}
		want := refMatch(collapsed, ns)
		if got := Match(pattern, name); got != want {
			t.Fatalf("Match(%q, %q) = %v, reference says %v", pattern, name, got, want)
		}
		checked++
	}
	if checked < 20000 {
		t.Fatalf("only %d cases checked", checked)
	}
}
