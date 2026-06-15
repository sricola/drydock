package broker

import "testing"

// HandleTask is exercised by the host-integration end-to-end test (Task 10);
// its pure helpers now live in the gateway and creds packages.

func TestGithubRepoRef(t *testing.T) {
	cases := []struct {
		in    string
		valid bool
	}{
		// Accept the three github.com forms gh can resolve.
		{"https://github.com/sricola/macagent", true},
		{"https://github.com/sricola/macagent.git", true},
		{"git@github.com:sricola/macagent", true},
		{"git@github.com:sricola/macagent.git", true},
		{"ssh://git@github.com/sricola/macagent.git", true},
		// Reject local paths (the bug we just hit: gh pr create fails on these).
		{"/Users/sray/gits/macagent", false},
		{"./macagent", false},
		// Reject other hosts.
		{"https://gitlab.com/x/y", false},
		{"git@gitlab.com:x/y", false},
		// Reject malformed inputs.
		{"", false},
		{"https://github.com/", false},
		{"https://github.com/onlyowner", false},
		{"github.com/x/y", false},
	}
	for _, tc := range cases {
		got := githubRepoRef.MatchString(tc.in)
		if got != tc.valid {
			t.Errorf("MatchString(%q) = %v, want %v", tc.in, got, tc.valid)
		}
	}
}
