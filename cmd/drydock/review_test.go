package main

import (
	"bufio"
	"reflect"
	"strings"
	"testing"
)

// TestPromptAcks drives the interactive second-look prompts: every category
// must be answered y (or yes) for the acknowledgment set to come back
// complete; any other answer aborts with ok=false and no acks (the caller
// then denies rather than approving under-acknowledged).
func TestPromptAcks(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		required []string
		wantAcks []string
		wantOK   bool
	}{
		{name: "no requirements", input: "", required: nil, wantAcks: []string{}, wantOK: true},
		{name: "all yes", input: "y\nyes\n", required: []string{"ci-workflow", "lockfile"}, wantAcks: []string{"ci-workflow", "lockfile"}, wantOK: true},
		{name: "second refused", input: "y\nn\n", required: []string{"ci-workflow", "lockfile"}, wantOK: false},
		{name: "empty answer refuses", input: "\n", required: []string{"ci-workflow"}, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var acks []string
			var ok bool
			out := captureStdout(t, func() {
				acks, ok = promptAcks(bufio.NewReader(strings.NewReader(tc.input)), tc.required)
			})
			if ok != tc.wantOK {
				t.Fatalf("promptAcks ok = %v, want %v (output %q)", ok, tc.wantOK, out)
			}
			if !tc.wantOK {
				if len(acks) != 0 {
					t.Errorf("refused prompt returned acks %v, want none", acks)
				}
				return
			}
			if len(acks) != len(tc.wantAcks) {
				t.Fatalf("acks = %v, want %v", acks, tc.wantAcks)
			}
			for i := range acks {
				if acks[i] != tc.wantAcks[i] {
					t.Errorf("acks = %v, want %v", acks, tc.wantAcks)
				}
			}
			for _, cat := range tc.required {
				if !strings.Contains(out, "acknowledge "+cat) {
					t.Errorf("prompt output missing category %q:\n%s", cat, out)
				}
			}
		})
	}
}

// TestPagerCommand_PassesPathAsPositionalArg guards against the diff path being
// interpolated into the shell script, where spaces or metacharacters would break
// the pager invocation (e.g. an audit dir under "/Users/My Name/") or, worse,
// inject shell. The path must arrive as the positional arg $1, never spliced
// into the -c script.
func TestPagerCommand_PassesPathAsPositionalArg(t *testing.T) {
	cases := []struct {
		name  string
		pager string
		path  string
		want  []string
	}{
		{
			name:  "default pager with flags",
			pager: "less -R",
			path:  "/home/u/.drydock/audit/abc.diff",
			want:  []string{"sh", "-c", `less -R "$1"`, "sh", "/home/u/.drydock/audit/abc.diff"},
		},
		{
			name:  "path with spaces stays a single argument",
			pager: "less -R",
			path:  "/Users/My Name/.drydock/audit/abc.diff",
			want:  []string{"sh", "-c", `less -R "$1"`, "sh", "/Users/My Name/.drydock/audit/abc.diff"},
		},
		{
			name:  "shell metacharacters in path are inert as $1",
			pager: "cat",
			path:  "/audit/; rm -rf ~ #.diff",
			want:  []string{"sh", "-c", `cat "$1"`, "sh", "/audit/; rm -rf ~ #.diff"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pagerCommand(tc.pager, tc.path)
			if !reflect.DeepEqual(got.Args, tc.want) {
				t.Errorf("pagerCommand(%q, %q).Args = %q, want %q", tc.pager, tc.path, got.Args, tc.want)
			}
		})
	}
}
