// Command cli-bump rewrites the pinned agent-CLI (and npm) versions in
// image/Dockerfile to the latest published npm versions, then reports what
// changed.
// It is intended for use by the scheduled bump automation (ROADMAP 4.6).
//
// Usage:
//
//	cli-bump [-dockerfile path] [-latest 'pkg=ver,...']
//
// When -latest is not given, the tool reads "pkg=version" lines from stdin.
// Versions that are not strictly semver-greater than the current pin are
// skipped (no-op). If nothing changed, the tool prints a short notice and
// exits 0 without touching the Dockerfile.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// versionRE is a strict anchored pattern for acceptable version tokens.
// It accepts bare numerics (1.2.3), pre-release suffixes (1.0.0-rc1, 1.0.0-beta.2),
// and nothing else. This guards the Dockerfile ARG pin against injection.
var versionRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*(-[0-9A-Za-z.]+)?$`)

type pkg struct {
	npm string // npm package name
	arg string // Dockerfile ARG name
}

var pkgs = []pkg{
	{"@anthropic-ai/claude-code", "CLAUDE_CODE_VERSION"},
	{"@openai/codex", "CODEX_VERSION"},
	{"opencode-ai", "OPENCODE_VERSION"},
	{"@google/gemini-cli", "GEMINI_CLI_VERSION"},
	// npm is not an agent CLI, but it is pinned in the same Dockerfile for the
	// same reason: its vendored node_modules is a CVE surface that only a
	// version bump refreshes (the base image's bundled npm goes stale with the
	// digest pin).
	{"npm", "NPM_VERSION"},
}

type bump struct {
	Pkg, Arg, From, To string
}

// newer reports whether version b is strictly semver-greater than a.
// It compares dot-separated numeric fields left to right; missing fields are
// treated as 0. Non-numeric suffixes are ignored conservatively: a bump is
// proposed only on a clear numeric increase.
func newer(a, b string) bool {
	af := versionFields(a)
	bf := versionFields(b)
	n := len(af)
	if len(bf) > n {
		n = len(bf)
	}
	for i := range n {
		ai := fieldAt(af, i)
		bi := fieldAt(bf, i)
		if bi > ai {
			return true
		}
		if bi < ai {
			return false
		}
	}
	return false
}

// versionFields splits a version string into its numeric parts.
// Each dot-separated token has any non-numeric suffix stripped before parsing.
func versionFields(v string) []int {
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n := numericPrefix(p)
		out = append(out, n)
	}
	return out
}

// numericPrefix returns the leading integer of s, or 0 if none.
func numericPrefix(s string) int {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, _ := strconv.Atoi(s[:end])
	return n
}

// fieldAt returns fields[i] or 0 if i is out of range.
func fieldAt(fields []int, i int) int {
	if i < len(fields) {
		return fields[i]
	}
	return 0
}

// planBumps rewrites the Dockerfile's ARG pins to latest[npmName] when the
// latest version is strictly newer (semver), and returns the rewritten content
// plus the list of bumps. latest maps npm package name to version string.
//
// A candidate version that does not fully match versionRE is rejected: no bump
// is written for that package and a warning is printed to stderr. This prevents
// malformed or injection-carrying strings from entering the Dockerfile.
func planBumps(dockerfile string, latest map[string]string) (string, []bump) {
	var bumps []bump
	out := dockerfile
	for _, p := range pkgs {
		latestVer, ok := latest[p.npm]
		if !ok {
			continue
		}
		if !versionRE.MatchString(latestVer) {
			fmt.Fprintf(os.Stderr, "cli-bump: warning: %s: rejected malformed version %q (skipping)\n", p.npm, latestVer)
			continue
		}
		re := regexp.MustCompile(`(?m)^(ARG ` + regexp.QuoteMeta(p.arg) + `=)([^\s]+)`)
		m := re.FindStringSubmatchIndex(out)
		if m == nil {
			continue
		}
		from := out[m[4]:m[5]]
		if !newer(from, latestVer) {
			continue
		}
		bumps = append(bumps, bump{
			Pkg:  p.npm,
			Arg:  p.arg,
			From: from,
			To:   latestVer,
		})
		out = re.ReplaceAllString(out, "${1}"+latestVer)
	}
	return out, bumps
}

func main() {
	dockerfilePath := flag.String("dockerfile", "image/Dockerfile", "path to Dockerfile to patch")
	latestFlag := flag.String("latest", "", "comma-separated pkg=version pairs (default: read from stdin)")
	flag.Parse()

	latest, err := parseLatest(*latestFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cli-bump:", err)
		os.Exit(2)
	}

	data, err := os.ReadFile(*dockerfilePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cli-bump: read dockerfile:", err)
		os.Exit(2)
	}

	out, bumps := planBumps(string(data), latest)
	if len(bumps) == 0 {
		fmt.Println("all pinned CLIs are current")
		return
	}

	if err := os.WriteFile(*dockerfilePath, []byte(out), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "cli-bump: write dockerfile:", err)
		os.Exit(2)
	}

	for _, b := range bumps {
		fmt.Printf("%s: %s -> %s\n", b.Pkg, b.From, b.To)
	}
}

// parseLatest returns a map of npm-name to version from the -latest flag or
// stdin. Lines that do not contain "=" are silently skipped.
func parseLatest(flagVal string) (map[string]string, error) {
	m := make(map[string]string)
	if flagVal != "" {
		for _, part := range strings.Split(flagVal, ",") {
			if err := addPair(m, strings.TrimSpace(part)); err != nil {
				return nil, err
			}
		}
		return m, nil
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if err := addPair(m, line); err != nil {
			return nil, err
		}
	}
	return m, scanner.Err()
}

// addPair parses a "key=value" line into m. Lines without "=" are skipped.
func addPair(m map[string]string, s string) error {
	idx := strings.IndexByte(s, '=')
	if idx < 0 {
		return nil
	}
	m[s[:idx]] = s[idx+1:]
	return nil
}
