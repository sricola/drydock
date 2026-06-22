package main

import (
	"reflect"
	"testing"
)

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
