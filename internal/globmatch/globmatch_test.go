package globmatch

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{".github/workflows/**", ".github/workflows/ci.yml", true},
		{".github/workflows/**", ".github/workflows/nested/x.yml", true},
		{".github/workflows/**", ".github/workflowsx", false},
		{".github/workflows/**", ".github/other.yml", false},
		{"**/*.lock", "a/b/go.lock", true},
		{"**/*.lock", "go.lock", true},
		{"*.lock", "go.lock", true},
		{"*.lock", "a/go.lock", false},
		{"src/*.go", "src/main.go", true},
		{"src/*.go", "src/a/main.go", false},
		{"src/**/*.go", "src/a/b/main.go", true},
		{"**", "anything/at/all", true},
		{"Dockerfile", "Dockerfile", true},
		{"Dockerfile", "sub/Dockerfile", false},
		{"**/Dockerfile", "sub/Dockerfile", true},

		// Trailing /** matches descendants only, never the directory itself.
		{".github/workflows/**", ".github/workflows", false},
		{"a/**", "a", false},
		{"a/**", "a/b", true},

		// ** mid-pattern may span zero segments.
		{"src/**/*.go", "src/main.go", true},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/bx", false},

		// Consecutive ** collapses.
		{"**/**", "a", true},
		{"**/**/*.go", "main.go", true},

		// ? matches exactly one character within a segment.
		{"a?b", "axb", true},
		{"a?b", "ab", false},
		{"a?b", "a/b", false},

		// Case-sensitive.
		{"README.md", "readme.md", false},

		// * never crosses a segment boundary.
		{"*", "a/b", false},
		{"*/*", "a/b", true},
	}
	for _, c := range cases {
		if got := Match(c.pat, c.name); got != c.want {
			t.Errorf("Match(%q,%q)=%v want %v", c.pat, c.name, got, c.want)
		}
	}
}

func TestValid(t *testing.T) {
	for _, ok := range []string{"**", "*.go", ".github/**", "src/**/*.ts", "a?b"} {
		if err := Valid(ok); err != nil {
			t.Errorf("Valid(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "[", "a[b", "src/[/x.go", "a\\"} {
		if err := Valid(bad); err == nil {
			t.Errorf("Valid(%q) = nil, want error", bad)
		}
	}
}

func TestMatch_HostileNoPanic(t *testing.T) {
	for _, p := range []string{"", "***", "**/**/**", "a/*/**/b"} {
		_ = Match(p, "a/b/c/d/e")
	}
}

// TestMatch_HostileBounded pins that pathological ** stacking cannot blow up:
// many ** segments against a long name must terminate quickly.
func TestMatch_HostileBounded(t *testing.T) {
	pat := ""
	for i := 0; i < 50; i++ {
		pat += "**/"
	}
	pat += "*.go"
	name := ""
	for i := 0; i < 200; i++ {
		name += "d/"
	}
	name += "main.go"
	if !Match(pat, name) {
		t.Errorf("Match(%q-ish, long name) = false, want true", "**/x50 + *.go")
	}
}
