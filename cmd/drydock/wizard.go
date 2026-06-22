package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// promptChoice prints a numbered menu and returns the chosen 1-based index.
// Empty input returns dflt; invalid input re-prompts.
func promptChoice(in io.Reader, out io.Writer, q string, opts []string, dflt int) int {
	r := bufio.NewReader(in)
	for {
		fmt.Fprintln(out, q)
		for i, o := range opts {
			tag := ""
			if i+1 == dflt {
				tag = "  (default)"
			}
			fmt.Fprintf(out, "  [%d] %s%s\n", i+1, o, tag)
		}
		fmt.Fprint(out, "> ")
		line, _ := r.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return dflt
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(opts) {
			return n
		}
		fmt.Fprintf(out, "  please enter 1–%d\n", len(opts))
	}
}

// promptYesNo returns the y/n answer; empty input returns dflt.
func promptYesNo(in io.Reader, out io.Writer, q string, dflt bool) bool {
	suffix := " [y/N] "
	if dflt {
		suffix = " [Y/n] "
	}
	r := bufio.NewReader(in)
	for {
		fmt.Fprint(out, q+suffix)
		line, _ := r.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return dflt
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
	}
}

// promptSecret reads one line from stdin with terminal echo disabled, so a
// pasted API key doesn't render on screen. Uses the system `stty` (no new
// dependency); echo is restored even on error.
func promptSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stdout, prompt)
	_ = exec.Command("stty", "-echo").Run()
	defer func() { _ = exec.Command("stty", "echo").Run(); fmt.Fprintln(os.Stdout) }()
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
