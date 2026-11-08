// Command redteam-report reads go test -json output on stdin, groups results
// by red-team claim (A1..A7 or the full test name for non-claim tests), and
// prints a per-claim GREEN/RED/SKIP table plus an overall verdict.
// Exit status is 1 if any claim is RED, else 0.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
)

// claimRe extracts the claim code (A<digits>, or V<digits> for the verifier-VM
// claims) from a red-team test name. Anchored on the Redteam_ prefix so an
// unrelated name such as TestIPV4Parse or TestSHA1Sum is not grouped under
// V4 or A1.
var claimRe = regexp.MustCompile(`Redteam_((?:A|V)[0-9]+)_`)

// testEvent is the subset of a go test -json event that we care about.
type testEvent struct {
	Action  string
	Package string
	Test    string
}

// claimStats accumulates pass/fail/skip counts for a single claim.
type claimStats struct {
	passed  int
	failed  int
	skipped int
}

// report maps claim key -> stats.
type report map[string]*claimStats

// summarize parses go test -json from r and returns a per-claim report plus
// a boolean that is true when no claim has any failures.
func summarize(r io.Reader) (report, bool) {
	rep := make(report)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		var ev testEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			// non-JSON line (build noise, etc.) -- skip silently
			continue
		}
		// Ignore package-level events (no Test field).
		if ev.Test == "" {
			continue
		}
		// Only care about terminal result actions.
		if ev.Action != "pass" && ev.Action != "fail" && ev.Action != "skip" {
			continue
		}
		key := claimKey(ev.Test)
		if rep[key] == nil {
			rep[key] = &claimStats{}
		}
		switch ev.Action {
		case "pass":
			rep[key].passed++
		case "fail":
			rep[key].failed++
		case "skip":
			rep[key].skipped++
		}
	}
	allGreen := true
	for _, s := range rep {
		if s.failed > 0 {
			allGreen = false
			break
		}
	}
	return rep, allGreen
}

// claimKey returns the Adigits claim code extracted from testName, or the
// full test name when no claim code is found.
func claimKey(testName string) string {
	if m := claimRe.FindStringSubmatch(testName); m != nil {
		return m[1]
	}
	return testName
}

// printReport writes the sorted per-claim table and overall verdict to w.
func printReport(w io.Writer, rep report, allGreen bool) {
	// Collect and sort keys so output is deterministic.
	keys := make([]string, 0, len(rep))
	for k := range rep {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "drydock red-team claim summary")
	fmt.Fprintln(w, "-------------------------------")

	for _, k := range keys {
		s := rep[k]
		status := "GREEN"
		if s.failed > 0 {
			status = "RED  "
		} else if s.passed == 0 && s.skipped > 0 {
			status = "SKIP "
		}
		total := s.passed + s.failed + s.skipped
		fmt.Fprintf(w, "  %-40s  %s  (%d test(s))\n", k, status, total)
	}

	fmt.Fprintln(w, "-------------------------------")
	if allGreen {
		fmt.Fprintln(w, "OVERALL: GREEN -- all claims passed or skipped")
	} else {
		fmt.Fprintln(w, "OVERALL: RED   -- one or more claims failed")
	}
	fmt.Fprintln(w, "")
}

func main() {
	rep, allGreen := summarize(os.Stdin)
	printReport(os.Stdout, rep, allGreen)
	if !allGreen {
		os.Exit(1)
	}
}
