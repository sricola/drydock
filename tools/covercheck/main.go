// Command covercheck reads a Go cover profile and fails when total statement
// coverage is below a floor. It exists so CI can gate on coverage without a
// third-party action: `go tool cover -func` prints the figure but has no
// threshold flag.
//
//	go run ./tools/covercheck -profile coverage.out -min 70
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func main() {
	profile := flag.String("profile", "coverage.out", "cover profile written by go test -coverprofile")
	min := flag.Float64("min", 0, "minimum total statement coverage, in percent")
	flag.Parse()
	f, err := os.Open(*profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "covercheck:", err)
		os.Exit(2)
	}
	defer f.Close()
	total, err := totalCoverage(bufio.NewScanner(f))
	if err != nil {
		fmt.Fprintln(os.Stderr, "covercheck:", err)
		os.Exit(2)
	}
	fmt.Printf("covercheck: total statement coverage %.1f%% (floor %.1f%%)\n", total, *min)
	if total < *min {
		fmt.Fprintf(os.Stderr, "covercheck: coverage %.1f%% is below the floor of %.1f%%\n", total, *min)
		os.Exit(1)
	}
}

// totalCoverage computes statement coverage from profile lines of the form
// "file:start.col,end.col numStmts count", weighting every block by its
// statement count. This is the whole-profile figure; `go tool cover -func`
// counts only blocks inside function declarations, so the two differ by a few
// tenths of a percent on this repository (77.1% vs 77.4% at the QA baseline).
// Set floors from this tool's own output.
func totalCoverage(sc *bufio.Scanner) (float64, error) {
	var covered, all int64
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			if strings.HasPrefix(line, "mode:") {
				continue
			}
		}
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return 0, fmt.Errorf("malformed profile line %q", line)
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("malformed statement count in %q", line)
		}
		c, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("malformed hit count in %q", line)
		}
		all += n
		if c > 0 {
			covered += n
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	if all == 0 {
		return 0, fmt.Errorf("profile has no statements")
	}
	return 100 * float64(covered) / float64(all), nil
}
